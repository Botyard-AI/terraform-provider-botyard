package provider

import (
	"context"
	"fmt"
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
	_ resource.Resource                = (*BotMemberBotResource)(nil)
	_ resource.ResourceWithConfigure   = (*BotMemberBotResource)(nil)
	_ resource.ResourceWithImportState = (*BotMemberBotResource)(nil)
)

// BotMemberBotResource manages ONE bot's membership of ANOTHER bot.
//
// Non-authoritative in the same way as botyard_bot_member: it owns exactly the
// (bot_slug, member_bot_id) row it names, and writes go through PATCH
// /member-bots so parallel resources on one bot never overwrite each other.
type BotMemberBotResource struct {
	data *providerData
}

// BotMemberBotResourceModel maps the botyard_bot_member_bot schema.
type BotMemberBotResourceModel struct {
	ID            types.String `tfsdk:"id"`
	BotSlug       types.String `tfsdk:"bot_slug"`
	MemberBotID   types.String `tfsdk:"member_bot_id"`
	Role          types.String `tfsdk:"role"`
	MemberBotSlug types.String `tfsdk:"member_bot_slug"`
	MemberBotName types.String `tfsdk:"member_bot_name"`
	CreatedAt     types.String `tfsdk:"created_at"`
}

// NewBotMemberBotResource is the resource factory registered with the provider.
func NewBotMemberBotResource() resource.Resource {
	return &BotMemberBotResource{}
}

// Metadata sets the resource type name.
func (r *BotMemberBotResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bot_member_bot"
}

// Schema defines the botyard_bot_member_bot resource schema.
func (r *BotMemberBotResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Makes one bot a member of another bot.\n\n" +
			"**Non-authoritative.** Each resource manages exactly one member bot and ignores every other row on " +
			"the list, so member bots added in the Botyard app never show up as drift. Declare one resource per " +
			"member bot; several on the same bot apply safely in parallel.\n\n" +
			"**Existing rows are adopted.** Creating this resource for a bot that is already a member takes that " +
			"row over and sets its `role`. Destroying the resource removes the row, whoever created it.\n\n" +
			"**Who may change members.** Only an owner of the bot named by `bot_slug` can change its member " +
			"bots. An API key owns only the bots it created.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier: `{bot_slug}/{member_bot_id}`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"bot_slug": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Slug of the bot whose member list this row is on, e.g. " +
					"`botyard_bot.x.slug`. Changing it forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"member_bot_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "ID of the member bot, e.g. `botyard_bot.y.id`. It must be a live bot in " +
					"the same organization, other than the bot itself. Changing it forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"role": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(botMemberRoleMember),
				MarkdownDescription: "`member` or `owner`. Defaults to `member`. Updated in place. The role is " +
					"recorded only: Botyard does not yet grant a member bot any permission from it.",
				Validators: []validator.String{stringvalidator.OneOf(botMemberRoles...)},
			},
			"member_bot_slug": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Slug of the member bot.",
			},
			"member_bot_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Display name of the member bot.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the membership was created (RFC 3339).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// Configure receives the shared provider data.
func (r *BotMemberBotResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *BotMemberBotResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan BotMemberBotResourceModel
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

func (r *BotMemberBotResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state BotMemberBotResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	slug := state.BotSlug.ValueString()
	apiResp, err := r.data.client.ListBotMemberBotsV1OrgsOrgIdBotsBotSlugMemberBotsGetWithResponse(ctx, r.data.orgID, slug)
	if err != nil {
		resp.Diagnostics.AddError("Error reading bot member bots", err.Error())
		return
	}
	if apiResp.StatusCode() == 404 {
		resp.State.RemoveResource(ctx)
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError("Unexpected response reading bot member bots",
			fmt.Sprintf("List returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	row := findBotMemberBot(*apiResp.JSON200, state.MemberBotID.ValueString())
	if row == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	mapBotMemberBot(slug, row, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *BotMemberBotResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Only `role` can change in place; everything else is RequiresReplace.
	var plan BotMemberBotResourceModel
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

func (r *BotMemberBotResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state BotMemberBotResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	remove := []string{state.MemberBotID.ValueString()}
	apiResp, err := r.data.client.PatchBotMemberBotsV1OrgsOrgIdBotsBotSlugMemberBotsPatchWithResponse(
		ctx, r.data.orgID, state.BotSlug.ValueString(), client.BotMemberBotsPatch{Remove: &remove})
	if err != nil {
		resp.Diagnostics.AddError("Error removing bot member bot", err.Error())
		return
	}
	switch apiResp.StatusCode() {
	case 200, 404:
		// Removed, already not a member, or the whole bot is gone.
	default:
		addBotMembershipAPIError(&resp.Diagnostics, "remove", state.BotSlug.ValueString(),
			fmt.Sprintf("bot %q", state.MemberBotID.ValueString()), apiResp.StatusCode(), apiResp.Body)
	}
}

// ImportState imports a membership by `{bot_slug}/{member_bot_id}`.
func (r *BotMemberBotResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	slug, memberBotID, err := parseBotMembershipID(req.ID, "member_bot_id")
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("bot_slug"), slug)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("member_bot_id"), memberBotID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// upsert adds this one member bot or changes its role (PATCH /member-bots with
// a single upsert entry), shared by Create and Update.
func (r *BotMemberBotResource) upsert(ctx context.Context, plan *BotMemberBotResourceModel, diags *diag.Diagnostics) {
	slug := plan.BotSlug.ValueString()
	memberBotID := plan.MemberBotID.ValueString()
	role := client.BotMemberBotEntryRole(plan.Role.ValueString())
	upsert := []client.BotMemberBotEntry{{BotId: memberBotID, Role: &role}}
	apiResp, err := r.data.client.PatchBotMemberBotsV1OrgsOrgIdBotsBotSlugMemberBotsPatchWithResponse(
		ctx, r.data.orgID, slug, client.BotMemberBotsPatch{Upsert: &upsert})
	if err != nil {
		diags.AddError("Error setting bot member bot", err.Error())
		return
	}
	if apiResp.JSON200 == nil {
		addBotMembershipAPIError(diags, "set", slug, fmt.Sprintf("bot %q", memberBotID), apiResp.StatusCode(), apiResp.Body)
		return
	}
	row := findBotMemberBot(*apiResp.JSON200, memberBotID)
	if row == nil {
		diags.AddError("Member bot missing after write",
			fmt.Sprintf("The API accepted bot %q as a member of bot %q but did not return it in the member "+
				"list. This is unexpected; re-run the apply, and report it if it persists.", memberBotID, slug))
		return
	}
	mapBotMemberBot(slug, row, plan)
}

func findBotMemberBot(members []client.BotMemberBotResponse, botID string) *client.BotMemberBotResponse {
	for i := range members {
		if members[i].BotId == botID {
			return &members[i]
		}
	}
	return nil
}

func mapBotMemberBot(slug string, row *client.BotMemberBotResponse, m *BotMemberBotResourceModel) {
	m.BotSlug = types.StringValue(slug)
	m.MemberBotID = types.StringValue(row.BotId)
	m.Role = types.StringValue(string(row.Role))
	m.MemberBotSlug = types.StringValue(row.Slug)
	m.MemberBotName = types.StringValue(row.Name)
	m.CreatedAt = types.StringValue(row.CreatedAt.Format(time.RFC3339))
	m.ID = types.StringValue(slug + "/" + row.BotId)
}
