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
	_ resource.Resource                = (*McpServerMemberResource)(nil)
	_ resource.ResourceWithConfigure   = (*McpServerMemberResource)(nil)
	_ resource.ResourceWithImportState = (*McpServerMemberResource)(nil)
)

// The principal kinds an MCP server's membership relation admits, and the roles
// it declares. Mirrors the server's validate_member_actor_type /
// validate_member_role for Resource.MCP_SERVER; the API still has the final
// word (422), these just fail the obvious typo at plan time.
var (
	mcpMemberActorTypes = []string{
		string(client.ActorTypeUser), string(client.ActorTypeBot), string(client.ActorTypeApiKey),
	}
	mcpMemberRoles = []string{mcpMemberRoleMember, mcpMemberRoleOwner}
)

const (
	mcpMemberRoleMember = "member"
	mcpMemberRoleOwner  = "owner"
)

// McpServerMemberResource manages ONE principal's membership of ONE MCP server.
//
// Deliberately non-authoritative: it owns exactly the (mcp_server_id,
// actor_type, actor_id) row it names and never looks at the rest of the list.
// Three kinds of row exist that no Terraform config declares — the creator's
// automatic `owner` row (epic #2672 D3), the `member` floor the API derives
// when a bot is assigned one of the server's tools, and rows added by hand in
// the app — and an authoritative whole-list resource would plan to delete all
// of them. The API writes one member per call, so several of these resources on
// one server apply in parallel without a provider-side lock.
type McpServerMemberResource struct {
	data *providerData
}

// McpServerMemberResourceModel maps the botyard_mcp_server_member schema.
type McpServerMemberResourceModel struct {
	ID          types.String `tfsdk:"id"`
	McpServerID types.String `tfsdk:"mcp_server_id"`
	ActorType   types.String `tfsdk:"actor_type"`
	ActorID     types.String `tfsdk:"actor_id"`
	Role        types.String `tfsdk:"role"`
	CreatedAt   types.String `tfsdk:"created_at"`
}

// NewMcpServerMemberResource is the resource factory registered with the provider.
func NewMcpServerMemberResource() resource.Resource {
	return &McpServerMemberResource{}
}

// Metadata sets the resource type name.
func (r *McpServerMemberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mcp_server_member"
}

// Schema defines the botyard_mcp_server_member resource schema.
func (r *McpServerMemberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Puts one principal (a user, bot or API key) on a Botyard MCP server's member list. " +
			"MCP servers are private: only members — and, when the server's `access` is `open`, everyone in the " +
			"organization — can see the server and use its tools.\n\n" +
			"**Non-authoritative.** Each resource manages exactly one membership and ignores every other row on " +
			"the list, so the server creator's automatic `owner` row, the `member` row Botyard adds for a bot " +
			"when it is assigned one of the server's tools, and members added in the app never show up as drift. " +
			"Declare one resource per principal.\n\n" +
			"**Existing rows are adopted.** Creating this resource for a principal that is already a member (for " +
			"example a bot that became a member through `botyard_bot_tool_assignment`) takes that row over and " +
			"sets its `role`; it is not an error. Destroying the resource removes the row, whoever created it.\n\n" +
			"**Last owner.** Botyard refuses to remove or demote a server's last `owner`. Give another principal " +
			"`role = \"owner\"` first, or `terraform state rm` this resource to leave the row outside Terraform. " +
			"Be careful declaring the provider's own API key here: it is the server's creator and so its owner, " +
			"and `role = \"member\"` would demote it — which fails if it is the only owner, and otherwise leaves " +
			"the key unable to edit the member list (or, on a `restricted` server without org-wide authority, " +
			"to see the server at all).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier: `{mcp_server_id}/{actor_type}/{actor_id}`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"mcp_server_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ID of the MCP server. Changing it forces replacement.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"actor_type": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Kind of principal: `user`, `bot` or `api_key`. Required because ids from the " +
					"three populations look alike. Changing it forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.OneOf(mcpMemberActorTypes...)},
			},
			"actor_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "ID of the principal, in its own population's namespace (a user ID, a bot " +
					"ID — e.g. `botyard_bot.x.id` — or an API key ID). Changing it forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"role": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(mcpMemberRoleMember),
				MarkdownDescription: "`member` (reaches the server and its tools) or `owner` (additionally edits the " +
					"member list and access mode). Defaults to `member`. Updated in place. Granting `owner` " +
					"requires the provider's API key to hold every permission `owner` carries on this server.",
				Validators: []validator.String{stringvalidator.OneOf(mcpMemberRoles...)},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the membership row was created (RFC 3339). For an adopted row this is when it was first created, not when Terraform took it over.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// Configure receives the shared provider data.
func (r *McpServerMemberResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *McpServerMemberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan McpServerMemberResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.put(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *McpServerMemberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state McpServerMemberResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	serverID := state.McpServerID.ValueString()
	apiResp, err := r.data.client.ListMcpServerMembersV1OrgsOrgIdMcpServersMcpServerIdMembersGetWithResponse(
		ctx, r.data.orgID, serverID)
	if err != nil {
		resp.Diagnostics.AddError("Error reading MCP server members", err.Error())
		return
	}
	if apiResp.StatusCode() == 404 {
		// The server is gone (or no longer visible to this key): the membership
		// cannot exist any more.
		resp.State.RemoveResource(ctx)
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError("Unexpected response reading MCP server members",
			fmt.Sprintf("List returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	row := findMcpServerMember(*apiResp.JSON200, state.ActorType.ValueString(), state.ActorID.ValueString())
	if row == nil {
		// Removed outside Terraform: drop it so the next plan re-creates it.
		resp.State.RemoveResource(ctx)
		return
	}
	mapMcpServerMember(serverID, row, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *McpServerMemberResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Only `role` can change in place; everything else is RequiresReplace.
	var plan McpServerMemberResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.put(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *McpServerMemberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state McpServerMemberResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	status, body, err := r.data.client.RemoveMcpServerMember(ctx, r.data.orgID, state.McpServerID.ValueString(),
		client.ActorType(state.ActorType.ValueString()), state.ActorID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error removing MCP server member", err.Error())
		return
	}
	switch status {
	case 200, 202, 204, 404:
		// removed, or already gone (row or whole server)
	default:
		addMcpMemberAPIError(&resp.Diagnostics, "remove", state, status, body)
	}
}

// ImportState imports a membership by `{mcp_server_id}/{actor_type}/{actor_id}`.
func (r *McpServerMemberResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	serverID, actorType, actorID, err := parseMcpServerMemberID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("mcp_server_id"), serverID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("actor_type"), actorType)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("actor_id"), actorID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// put is the add-or-update of this one membership (PUT /members), shared by
// Create and Update. The API answers with the whole list; only our row is read
// back.
func (r *McpServerMemberResource) put(ctx context.Context, plan *McpServerMemberResourceModel, diags *diag.Diagnostics) {
	serverID := plan.McpServerID.ValueString()
	role := plan.Role.ValueString()
	apiResp, err := r.data.client.AddMcpServerMemberV1OrgsOrgIdMcpServersMcpServerIdMembersPutWithResponse(
		ctx, r.data.orgID, serverID, client.McpServerMemberAdd{
			ActorType: client.ActorType(plan.ActorType.ValueString()),
			ActorId:   plan.ActorID.ValueString(),
			Role:      &role,
		})
	if err != nil {
		diags.AddError("Error setting MCP server member", err.Error())
		return
	}
	if apiResp.JSON200 == nil {
		addMcpMemberAPIError(diags, "set", *plan, apiResp.StatusCode(), apiResp.Body)
		return
	}
	row := findMcpServerMember(*apiResp.JSON200, plan.ActorType.ValueString(), plan.ActorID.ValueString())
	if row == nil {
		diags.AddError("MCP server member missing after write",
			fmt.Sprintf("The API accepted the membership of %s %q on MCP server %q but did not return it in the "+
				"member list. This is unexpected; re-run the apply, and report it if it persists.",
				plan.ActorType.ValueString(), plan.ActorID.ValueString(), serverID))
		return
	}
	mapMcpServerMember(serverID, row, plan)
}

func findMcpServerMember(members []client.McpServerMember, actorType, actorID string) *client.McpServerMember {
	for i := range members {
		if string(members[i].ActorType) == actorType && members[i].ActorId == actorID {
			return &members[i]
		}
	}
	return nil
}

func mapMcpServerMember(serverID string, row *client.McpServerMember, m *McpServerMemberResourceModel) {
	m.McpServerID = types.StringValue(serverID)
	m.ActorType = types.StringValue(string(row.ActorType))
	m.ActorID = types.StringValue(row.ActorId)
	m.Role = types.StringValue(row.Role)
	m.CreatedAt = types.StringValue(row.CreatedAt.Format(time.RFC3339))
	m.ID = types.StringValue(mcpServerMemberID(serverID, string(row.ActorType), row.ActorId))
}

func mcpServerMemberID(serverID, actorType, actorID string) string {
	return serverID + "/" + actorType + "/" + actorID
}

// parseMcpServerMemberID splits `{mcp_server_id}/{actor_type}/{actor_id}`.
func parseMcpServerMemberID(id string) (serverID, actorType, actorID string, err error) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf(
			"expected `{mcp_server_id}/{actor_type}/{actor_id}` (e.g. `<server-uuid>/bot/<bot-uuid>`), got %q", id)
	}
	for _, t := range mcpMemberActorTypes {
		if parts[1] == t {
			return parts[0], parts[1], parts[2], nil
		}
	}
	return "", "", "", fmt.Errorf("actor_type %q in import ID %q must be one of: %s",
		parts[1], id, strings.Join(mcpMemberActorTypes, ", "))
}

// addMcpMemberAPIError turns a refused membership write into a diagnostic that
// names the remedy. op is "set" (PUT add-or-update) or "remove" (DELETE).
func addMcpMemberAPIError(diags *diag.Diagnostics, op string, m McpServerMemberResourceModel, status int, body []byte) {
	who := fmt.Sprintf("%s %q", m.ActorType.ValueString(), m.ActorID.ValueString())
	server := m.McpServerID.ValueString()
	apiDetail := describeAPIError(body)
	switch status {
	case 409:
		verb := "removed"
		if op == "set" {
			verb = "demoted"
		}
		diags.AddError("Cannot change the last owner of the MCP server",
			fmt.Sprintf("%s is the last owner of MCP server %q and cannot be %s: a member list with no owner can "+
				"only be repaired by an organization owner.\n\n"+
				"Fix: give another principal the owner role first (for example another botyard_mcp_server_member "+
				"with role = \"owner\"), or leave this row outside Terraform with `terraform state rm`.\n\n"+
				"API: %s", who, server, verb, apiDetail))
	case 403:
		if op == "set" && m.Role.ValueString() == mcpMemberRoleOwner {
			diags.AddError("Not permitted to grant this MCP server role",
				fmt.Sprintf("The provider's API key cannot give %s the %q role on MCP server %q. Either the key is "+
					"not an owner of this server (and holds no org-wide mcp_server.manage), or the role carries "+
					"permissions on this server the key does not hold itself.\n\nAPI: %s",
					who, m.Role.ValueString(), server, apiDetail))
			return
		}
		diags.AddError("Not permitted to change this MCP server's members",
			fmt.Sprintf("The provider's API key cannot change the member list of MCP server %q. Only an owner of "+
				"the server, or a principal holding org-wide mcp_server.manage, may add or remove members.\n\nAPI: %s",
				server, apiDetail))
	case 404:
		diags.AddError("MCP server not found",
			fmt.Sprintf("MCP server %q does not exist, or is not visible to the provider's API key (a restricted "+
				"server is visible only to its members).\n\nAPI: %s", server, apiDetail))
	default:
		diags.AddError(fmt.Sprintf("Unexpected response trying to %s MCP server member", op),
			fmt.Sprintf("Membership of %s on MCP server %q: HTTP %d: %s", who, server, status, apiDetail))
	}
}
