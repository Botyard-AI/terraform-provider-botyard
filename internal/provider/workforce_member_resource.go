package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
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
	_ resource.Resource                = (*WorkforceMemberResource)(nil)
	_ resource.ResourceWithConfigure   = (*WorkforceMemberResource)(nil)
	_ resource.ResourceWithImportState = (*WorkforceMemberResource)(nil)

	workforceMemberIDPart     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
	workforceMemberActorTypes = []string{
		string(client.ActorTypeUser), string(client.ActorTypeBot), string(client.ActorTypeApiKey),
	}
)

// WorkforceMemberResource owns one (board ID, principal type, principal ID) row,
// never the board or its entire member list. Every write carries a strict-body
// ID precondition, so older servers reject it before mutation. Slugs are only
// locators, resolved afresh by ID and fenced at the API's resolve-once boundary.
type WorkforceMemberResource struct{ data *providerData }

type WorkforceMemberResourceModel struct {
	ID            types.String `tfsdk:"id"`
	WorkforceID   types.String `tfsdk:"workforce_id"`
	WorkforceSlug types.String `tfsdk:"workforce_slug"`
	ActorType     types.String `tfsdk:"actor_type"`
	ActorID       types.String `tfsdk:"actor_id"`
	Role          types.String `tfsdk:"role"`
	CreatedAt     types.String `tfsdk:"created_at"`
}

func NewWorkforceMemberResource() resource.Resource { return &WorkforceMemberResource{} }

func (r *WorkforceMemberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_workforce_member"
}

func (r *WorkforceMemberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	idValidator := []validator.String{stringvalidator.RegexMatches(workforceMemberIDPart, "Use an identifier of 1–255 letters, digits, underscores or hyphens, without path separators.")}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages one user, bot or API key's membership of an existing workforce board. " +
			"**Non-authoritative:** other members, including creator/owner rows and members added in the app, are never changed. " +
			"The board's immutable ID is the identity; renaming its slug updates only computed metadata.\n\n" +
			"**Adoption:** creating a resource for an existing member adopts the row and sets its configured role. Destroy removes that row, regardless of who originally added it. " +
			"The API refuses removing or demoting the last owner; grant another principal owner first, or use `terraform state rm` to relinquish management without removing access. " +
			"Removing a member does not necessarily revoke access to an `open` board. This resource does not change board access mode. " +
			"Demoting or removing the provider API key itself can revoke its ability to manage a restricted board. " +
			"A board absent from the caller-visible list is treated as missing, including loss of visibility; use a key with appropriate visibility when managing memberships.\n\n" +
			"Requires an API supporting `expected_workforce_id` and `X-Botyard-Workforce-Id` on membership operations. " +
			"The provider verifies support with a guarded read before writing; every write also carries the ID in the strict request body, so old API replicas reject it rather than ignore it. " +
			"No automatic write retries are made. A lost response may mean a write succeeded; refresh before retrying.\n\n" +
			"The provider API key must be able to list/view the board. Member writes require board-owner or organization-wide `workforce:board.manage` authority, and the API also checks that the caller may confer the requested role.",
		Attributes: map[string]schema.Attribute{
			"id":             schema.StringAttribute{Computed: true, MarkdownDescription: "`{workforce_id}/{actor_type}/{actor_id}`.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"workforce_id":   schema.StringAttribute{Required: true, MarkdownDescription: "Immutable ID of the existing workforce board. Changing it forces replacement; a slug rename does not.", Validators: idValidator, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"workforce_slug": schema.StringAttribute{Computed: true, MarkdownDescription: "Current slug of the board resolved by its immutable ID. Not a configured identity."},
			"actor_type":     schema.StringAttribute{Required: true, MarkdownDescription: "Principal population: `user`, `bot` or `api_key`. Changing it forces replacement.", Validators: []validator.String{stringvalidator.OneOf(workforceMemberActorTypes...)}, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"actor_id":       schema.StringAttribute{Required: true, MarkdownDescription: "Principal ID in the specified population (for a bot, `botyard_bot.example.id`). Changing it forces replacement.", Validators: idValidator, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"role":           schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("member"), MarkdownDescription: "`member` works with board tasks; `owner` additionally governs the board and its members. Defaults to `member`; updated in place.", Validators: []validator.String{stringvalidator.OneOf("member", "owner")}},
			"created_at":     schema.StringAttribute{Computed: true, MarkdownDescription: "Membership creation timestamp (RFC 3339), including the original timestamp when adopting a row."},
		},
	}
}

func (r *WorkforceMemberResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type", fmt.Sprintf("Expected *providerData, got %T. This is a provider bug.", req.ProviderData))
		return
	}
	r.data = data
}

func (r *WorkforceMemberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan WorkforceMemberResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.put(ctx, &plan, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *WorkforceMemberResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan WorkforceMemberResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.put(ctx, &plan, &resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
}

func (r *WorkforceMemberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state WorkforceMemberResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	slug, rows, absent := r.members(ctx, state.WorkforceID.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	row := findWorkforceMember(rows, state.ActorType.ValueString(), state.ActorID.ValueString())
	if absent || row == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	mapWorkforceMember(slug, row, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *WorkforceMemberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state WorkforceMemberResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.WorkforceID.ValueString()
	slug, rows, absent := r.members(ctx, id, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if absent || findWorkforceMember(rows, state.ActorType.ValueString(), state.ActorID.ValueString()) == nil {
		return
	}
	status, body, ack, err := r.data.client.RemoveWorkforceMember(ctx, r.data.orgID, slug, id,
		client.ActorType(state.ActorType.ValueString()), state.ActorID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error removing workforce member", "The removal may have succeeded; refresh before retrying. No automatic retry was made.\n\n"+err.Error())
		return
	}
	switch status {
	case http.StatusNoContent:
		if ack != id {
			resp.Diagnostics.AddError("Missing workforce identity acknowledgement", "The API did not confirm the expected board ID after removal. The row may have been removed; refresh before retrying. No automatic retry was made.")
		}
	case http.StatusNotFound:
		// The one row (or the board) disappeared after our guarded preflight.
	default:
		addWorkforceMemberAPIError(&resp.Diagnostics, "remove", status, body)
	}
}

func (r *WorkforceMemberResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, actorType, actorID, err := parseWorkforceMemberID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("workforce_id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("actor_type"), actorType)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("actor_id"), actorID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// members is read-only. A bounded retry handles a board renamed between the
// list-by-ID lookup and the guarded GET. Writes themselves are never retried.
func (r *WorkforceMemberResource) members(ctx context.Context, id string, diags *diag.Diagnostics) (string, []client.WorkforceMember, bool) {
	for attempt := 0; attempt < 2; attempt++ {
		boards, err := r.data.client.ListWorkforcesV1OrgsOrgIdWorkforcesGetWithResponse(ctx, r.data.orgID)
		if err != nil {
			diags.AddError("Error resolving workforce ID", err.Error())
			return "", nil, false
		}
		if boards.JSON200 == nil || *boards.JSON200 == nil {
			diags.AddError("Unexpected response listing workforces", fmt.Sprintf("HTTP %d: %s", boards.StatusCode(), describeAPIError(boards.Body)))
			return "", nil, false
		}
		slug := ""
		found := false
		for _, board := range *boards.JSON200 {
			if board.Id == id {
				if found || board.Slug == "" {
					diags.AddError("Invalid workforce identity response", "The API returned an empty slug or duplicate entries for the configured board ID.")
					return "", nil, false
				}
				slug, found = board.Slug, true
			}
		}
		if !found {
			return "", nil, true
		}
		listed, err := r.data.client.ListWorkforceMembersV1OrgsOrgIdWorkforcesWorkforceSlugMembersGetWithResponse(ctx,
			r.data.orgID, slug, &client.ListWorkforceMembersV1OrgsOrgIdWorkforcesWorkforceSlugMembersGetParams{ExpectedWorkforceId: &id})
		if err != nil {
			diags.AddError("Error reading workforce members", err.Error())
			return "", nil, false
		}
		if listed.StatusCode() == http.StatusNotFound || listed.StatusCode() == http.StatusConflict {
			continue
		}
		if listed.JSON200 == nil || *listed.JSON200 == nil {
			addWorkforceMemberAPIError(diags, "read", listed.StatusCode(), listed.Body)
			return "", nil, false
		}
		if !acknowledgeWorkforce(listed.HTTPResponse, id, diags) {
			return "", nil, false
		}
		for _, row := range *listed.JSON200 {
			if row.ActorId == "" || row.ActorType == "" || row.Role == "" || row.CreatedAt.IsZero() {
				diags.AddError("Invalid workforce member response", "The API returned a membership without its required principal, role or creation timestamp. Existing state is preserved and no write is made.")
				return "", nil, false
			}
		}
		return slug, *listed.JSON200, false
	}
	diags.AddError("Workforce changed during membership lookup", "The ID-to-slug lookup no longer matches the membership endpoint. Refresh and retry after board renames have settled. No membership write was made.")
	return "", nil, false
}

func (r *WorkforceMemberResource) put(ctx context.Context, plan *WorkforceMemberResourceModel, diags *diag.Diagnostics) {
	id := plan.WorkforceID.ValueString()
	slug, _, absent := r.members(ctx, id, diags)
	if diags.HasError() {
		return
	}
	if absent {
		diags.AddError("Workforce not found", "The configured workforce ID is absent or not visible to the provider API key. No membership write was made.")
		return
	}
	role := plan.Role.ValueString()
	written, err := r.data.client.SetWorkforceMembersV1OrgsOrgIdWorkforcesWorkforceSlugMembersPutWithResponse(ctx,
		r.data.orgID, slug, &client.SetWorkforceMembersV1OrgsOrgIdWorkforcesWorkforceSlugMembersPutParams{ExpectedWorkforceId: &id},
		client.WorkforceMembersSet{ExpectedWorkforceId: &id, Members: []client.WorkforceMemberSet{{ActorType: client.ActorType(plan.ActorType.ValueString()), ActorId: plan.ActorID.ValueString(), Role: &role}}})
	if err != nil {
		diags.AddError("Error setting workforce member", "The membership may have been written; refresh before retrying. No automatic retry was made.\n\n"+err.Error())
		return
	}
	if written.JSON200 == nil || *written.JSON200 == nil {
		addWorkforceMemberAPIError(diags, "set", written.StatusCode(), written.Body)
		return
	}
	if !acknowledgeWorkforce(written.HTTPResponse, id, diags) {
		return
	}
	row := findWorkforceMember(*written.JSON200, plan.ActorType.ValueString(), plan.ActorID.ValueString())
	if row == nil || row.Role != role || row.CreatedAt.IsZero() {
		diags.AddError("Invalid workforce member write response", "The API accepted the write but did not return the intended member, role and creation timestamp. The write may have succeeded; refresh before retrying. No automatic retry was made.")
		return
	}
	mapWorkforceMember(slug, row, plan)
}

func acknowledgeWorkforce(response *http.Response, id string, diags *diag.Diagnostics) bool {
	if response == nil || response.Header.Get(client.WorkforceIdentityHeader) != id {
		diags.AddError("API does not acknowledge workforce identity", "This resource requires membership identity preconditions and a matching X-Botyard-Workforce-Id response header. Upgrade the API before managing memberships. A read preflight failure makes no membership write; a missing acknowledgement after a write requires a refresh before retrying.")
		return false
	}
	return true
}

func findWorkforceMember(rows []client.WorkforceMember, actorType, actorID string) *client.WorkforceMember {
	for i := range rows {
		if string(rows[i].ActorType) == actorType && rows[i].ActorId == actorID {
			return &rows[i]
		}
	}
	return nil
}

func mapWorkforceMember(slug string, row *client.WorkforceMember, model *WorkforceMemberResourceModel) {
	model.ID = types.StringValue(workforceMemberID(model.WorkforceID.ValueString(), string(row.ActorType), row.ActorId))
	model.WorkforceSlug = types.StringValue(slug)
	model.ActorType = types.StringValue(string(row.ActorType))
	model.ActorID = types.StringValue(row.ActorId)
	model.Role = types.StringValue(row.Role)
	model.CreatedAt = types.StringValue(row.CreatedAt.Format(time.RFC3339Nano))
}

func workforceMemberID(id, actorType, actorID string) string {
	return id + "/" + actorType + "/" + actorID
}

func parseWorkforceMemberID(value string) (id, actorType, actorID string, err error) {
	parts := strings.Split(value, "/")
	if len(parts) == 3 && workforceMemberIDPart.MatchString(parts[0]) && workforceMemberIDPart.MatchString(parts[2]) {
		for _, kind := range workforceMemberActorTypes {
			if parts[1] == kind {
				return parts[0], parts[1], parts[2], nil
			}
		}
	}
	return "", "", "", fmt.Errorf("expected `{workforce_id}/{actor_type}/{actor_id}` with actor_type user, bot or api_key and non-empty identifier segments; got %q", value)
}

func addWorkforceMemberAPIError(diags *diag.Diagnostics, operation string, status int, body []byte) {
	detail := fmt.Sprintf("HTTP %d: %s", status, describeAPIError(body))
	switch status {
	case http.StatusForbidden:
		diags.AddError("Workforce membership operation forbidden", "The API key must be able to see the board and, for writes, own the board or hold workforce:board.manage. It must also hold the permissions of any role it grants.\n\n"+detail)
	case http.StatusConflict:
		diags.AddError("Workforce membership conflict", "The board identity changed or the change would remove/demote its last owner. Refresh the board locator, or give another principal owner first. To relinquish management without removing membership, use terraform state rm. No automatic retry was made.\n\n"+detail)
	case http.StatusNotFound:
		diags.AddError("Workforce or principal not found", "The board or principal is absent or not visible in this organization.\n\n"+detail)
	default:
		diags.AddError("Unexpected response trying to "+operation+" workforce member", detail)
	}
}
