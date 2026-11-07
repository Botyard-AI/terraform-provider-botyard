package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ resource.Resource                = (*BotMemberResource)(nil)
	_ resource.ResourceWithConfigure   = (*BotMemberResource)(nil)
	_ resource.ResourceWithImportState = (*BotMemberResource)(nil)
)

// The roles a bot's human member list admits. Mirrors the API's BotMemberRole;
// the API still has the final word (422), this fails the obvious typo at plan
// time.
const (
	botMemberRoleMember = "member"
	botMemberRoleOwner  = "owner"
)

var botMemberRoles = []string{botMemberRoleMember, botMemberRoleOwner}

// BotMemberResource manages ONE user's membership of ONE bot.
//
// Deliberately non-authoritative, like botyard_mcp_server_member: it owns
// exactly the (bot_slug, user_id) row it names and never looks at the rest of
// the list, so members added in the app, the creator's own row and pending
// invitations never show up as drift. Writes go through PATCH /members, which
// changes only the named user, so several of these resources on one bot apply
// in parallel without overwriting each other.
type BotMemberResource struct {
	data *providerData
}

// BotMemberResourceModel maps the botyard_bot_member schema.
type BotMemberResourceModel struct {
	ID        types.String `tfsdk:"id"`
	BotSlug   types.String `tfsdk:"bot_slug"`
	UserID    types.String `tfsdk:"user_id"`
	Role      types.String `tfsdk:"role"`
	Email     types.String `tfsdk:"email"`
	Name      types.String `tfsdk:"name"`
	CreatedAt types.String `tfsdk:"created_at"`
}

// NewBotMemberResource is the resource factory registered with the provider.
func NewBotMemberResource() resource.Resource {
	return &BotMemberResource{}
}

// Metadata sets the resource type name.
func (r *BotMemberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bot_member"
}

// Schema defines the botyard_bot_member resource schema.
func (r *BotMemberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Puts one person on a Botyard bot's member list. When the bot's `access` is " +
			"`restricted`, only its members can use it; owners also manage its access and members.\n\n" +
			"**Non-authoritative.** Each resource manages exactly one membership and ignores every other row on " +
			"the list, so members added in the Botyard app and pending invitations never show up as drift. " +
			"Declare one resource per person. Several of these resources on the same bot apply safely in " +
			"parallel: each changes only its own row.\n\n" +
			"**Existing rows are adopted.** Creating this resource for someone who is already a member takes that " +
			"row over and sets its `role`; it is not an error. Destroying the resource removes the row, whoever " +
			"created it.\n\n" +
			"**Who may change members.** Only a bot owner can change its member list. An API key owns only the " +
			"bots it created, so manage memberships with the key that created the bot.\n\n" +
			"**Last owner.** Botyard refuses a change that would leave the bot's member list without an owner. " +
			"Give someone else `role = \"owner\"` first, or `terraform state rm` this resource to leave the row " +
			"outside Terraform.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier: `{bot_slug}/{user_id}`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"bot_slug": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Slug of the bot, e.g. `botyard_bot.x.slug`. Changing it forces " +
					"replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"user_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "ID of the user. They must be a member of the organization. Changing it " +
					"forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"role": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(botMemberRoleMember),
				MarkdownDescription: "`member` (uses the bot) or `owner` (also manages its access and members). " +
					"Defaults to `member`. Updated in place.",
				Validators: []validator.String{stringvalidator.OneOf(botMemberRoles...)},
			},
			"email": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The member's email address.",
			},
			"name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The member's display name.",
			},
			"created_at": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "When the membership was created (RFC 3339). For an adopted row this is when " +
					"it was first created, not when Terraform took it over.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// Configure receives the shared provider data.
func (r *BotMemberResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T. This is a bug in the provider.", req.ProviderData))
		return
	}
	r.data = data
}

func (r *BotMemberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan BotMemberResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.upsert(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *BotMemberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state BotMemberResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	slug := state.BotSlug.ValueString()
	apiResp, err := r.data.client.ListBotMembersV1OrgsOrgIdBotsBotSlugMembersGetWithResponse(ctx, r.data.orgID, slug)
	if err != nil {
		resp.Diagnostics.AddError("Error reading bot members", err.Error())
		return
	}
	if apiResp.StatusCode() == 404 {
		// The bot is gone (or no longer visible to this key): the membership
		// cannot exist any more.
		resp.State.RemoveResource(ctx)
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError("Unexpected response reading bot members",
			fmt.Sprintf("List returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	row := findBotMember(*apiResp.JSON200, state.UserID.ValueString())
	if row == nil {
		// Removed outside Terraform: drop it so the next plan re-creates it.
		resp.State.RemoveResource(ctx)
		return
	}
	mapBotMember(slug, row, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *BotMemberResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Only `role` can change in place; everything else is RequiresReplace.
	var plan BotMemberResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.upsert(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *BotMemberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state BotMemberResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	remove := []string{state.UserID.ValueString()}
	apiResp, err := r.data.client.PatchBotMembersV1OrgsOrgIdBotsBotSlugMembersPatchWithResponse(
		ctx, r.data.orgID, state.BotSlug.ValueString(), client.BotMembersPatch{Remove: &remove})
	if err != nil {
		resp.Diagnostics.AddError("Error removing bot member", err.Error())
		return
	}
	switch apiResp.StatusCode() {
	case 200, 404:
		// Removed, already not a member (the API ignores an absent user), or the
		// whole bot is gone.
	default:
		addBotMembershipAPIError(&resp.Diagnostics, "remove", state.BotSlug.ValueString(),
			fmt.Sprintf("user %q", state.UserID.ValueString()), apiResp.StatusCode(), apiResp.Body)
	}
}

// ImportState imports a membership by `{bot_slug}/{user_id}`.
func (r *BotMemberResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	slug, userID, err := parseBotMembershipID(req.ID, "user_id")
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("bot_slug"), slug)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_id"), userID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// upsert adds this one membership or changes its role (PATCH /members with a
// single upsert entry), shared by Create and Update. The API answers with the
// whole list; only our row is read back.
func (r *BotMemberResource) upsert(ctx context.Context, plan *BotMemberResourceModel, diags *diag.Diagnostics) {
	slug := plan.BotSlug.ValueString()
	userID := plan.UserID.ValueString()
	role := client.BotMemberRole(plan.Role.ValueString())
	upsert := []client.BotMemberEntry{{UserId: userID, RoleId: &role}}
	apiResp, err := r.data.client.PatchBotMembersV1OrgsOrgIdBotsBotSlugMembersPatchWithResponse(
		ctx, r.data.orgID, slug, client.BotMembersPatch{Upsert: &upsert})
	if err != nil {
		diags.AddError("Error setting bot member", err.Error())
		return
	}
	if apiResp.JSON200 == nil {
		addBotMembershipAPIError(diags, "set", slug, fmt.Sprintf("user %q", userID), apiResp.StatusCode(), apiResp.Body)
		return
	}
	row := findBotMember(*apiResp.JSON200, userID)
	if row == nil {
		diags.AddError("Bot member missing after write",
			fmt.Sprintf("The API accepted the membership of user %q on bot %q but did not return it in the "+
				"member list. This is unexpected; re-run the apply, and report it if it persists.", userID, slug))
		return
	}
	mapBotMember(slug, row, plan)
}

func findBotMember(members []client.BotMemberResponse, userID string) *client.BotMemberResponse {
	for i := range members {
		if members[i].UserId == userID {
			return &members[i]
		}
	}
	return nil
}

func mapBotMember(slug string, row *client.BotMemberResponse, m *BotMemberResourceModel) {
	m.BotSlug = types.StringValue(slug)
	m.UserID = types.StringValue(row.UserId)
	m.Role = types.StringValue(string(row.RoleId))
	m.Email = types.StringValue(row.Email)
	m.Name = types.StringValue(row.Name)
	m.CreatedAt = types.StringValue(row.CreatedAt.Format(time.RFC3339))
	m.ID = types.StringValue(slug + "/" + row.UserId)
}

// parseBotMembershipID splits `{bot_slug}/{member_id}`. memberAttr names the
// second part in the error message (`user_id` or `member_bot_id`).
func parseBotMembershipID(id, memberAttr string) (slug, memberID string, err error) {
	parts := strings.Split(id, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected `{bot_slug}/{%s}`, got %q", memberAttr, id)
	}
	return parts[0], parts[1], nil
}

// addBotMembershipAPIError turns a refused PATCH /members or /member-bots into a
// diagnostic that names the remedy. op is "set" or "remove"; who names the
// member ("user \"…\"" or "bot \"…\"").
func addBotMembershipAPIError(diags *diag.Diagnostics, op, slug, who string, status int, body []byte) {
	detail := describeAPIError(body)
	switch {
	case isBotOwnerForbidden(status, body):
		diags.AddError("Not the bot's owner",
			fmt.Sprintf("Botyard refused to %s the membership of %s on bot %q (HTTP 403: %s).\n\n"+
				"Only a bot owner can change its members. An API key owns only the bots it created itself, so "+
				"it cannot change the members of a bot created in the Botyard app or with another key, even if "+
				"the key's organization role is owner. Make the change in the Botyard app, or manage the "+
				"membership with the key that created the bot.", op, who, slug, detail))
	case status == 404:
		diags.AddError("Bot not found",
			fmt.Sprintf("Bot %q does not exist, or is not visible to the provider's API key.\n\nAPI: %s", slug, detail))
	case status == 422:
		diags.AddError("Botyard rejected the membership change",
			fmt.Sprintf("Botyard refused to %s the membership of %s on bot %q: %s\n\n"+
				"If the change would leave the bot without an owner, give another member the owner role first, "+
				"or leave this row outside Terraform with `terraform state rm`.", op, who, slug, detail))
	default:
		diags.AddError(fmt.Sprintf("Unexpected response trying to %s bot member", op),
			fmt.Sprintf("Membership of %s on bot %q: HTTP %d: %s", who, slug, status, detail))
	}
}
