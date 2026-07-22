package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ resource.Resource                = (*BotToolAssignmentResource)(nil)
	_ resource.ResourceWithConfigure   = (*BotToolAssignmentResource)(nil)
	_ resource.ResourceWithImportState = (*BotToolAssignmentResource)(nil)
)

// BotToolAssignmentResource manages the complete set of tools assigned to a bot.
// Like botyard_bot_skill_assignment it takes *exclusive* ownership of the bot's
// tool assignments. The tools API exposes a whole-collection replace (PUT), so —
// unlike skills — this resource sets the desired set in a single atomic call
// rather than reconciling adds/removes.
type BotToolAssignmentResource struct {
	data *providerData
}

// BotToolAssignmentResourceModel maps the botyard_bot_tool_assignment schema.
type BotToolAssignmentResourceModel struct {
	ID      types.String `tfsdk:"id"`
	BotSlug types.String `tfsdk:"bot_slug"`
	ToolIDs types.Set    `tfsdk:"tool_ids"`
}

// NewBotToolAssignmentResource is the resource factory registered with the provider.
func NewBotToolAssignmentResource() resource.Resource {
	return &BotToolAssignmentResource{}
}

// Metadata sets the resource type name.
func (r *BotToolAssignmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bot_tool_assignment"
}

// Schema defines the botyard_bot_tool_assignment resource schema.
func (r *BotToolAssignmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the complete set of tools assigned to a Botyard bot. This resource takes " +
			"**exclusive** ownership of the bot's tool assignments: any tool assigned outside Terraform is " +
			"removed on the next apply so the bot's assigned tools converge on `tool_ids`. Use at most one " +
			"`botyard_bot_tool_assignment` per bot. Per-tool-domain settings (e.g. sessions visibility) are " +
			"configured on the `botyard_bot` `config` block, not here.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier — the bot slug this assignment manages.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"bot_slug": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Slug of the bot whose tool assignments this resource manages. Changing it " +
					"forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"tool_ids": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				MarkdownDescription: "The exact, complete set of tool catalog IDs assigned to the bot. Tools present " +
					"on the bot but absent from this set are unassigned on apply. An empty set removes all tools.",
			},
		},
	}
}

// Configure receives the shared provider data.
func (r *BotToolAssignmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *BotToolAssignmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan BotToolAssignmentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.replace(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *BotToolAssignmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state BotToolAssignmentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	slug := state.BotSlug.ValueString()
	apiResp, err := r.data.client.ListBotToolsV1OrgsOrgIdBotsBotSlugToolsGetWithResponse(ctx, r.data.orgID, slug)
	if err != nil {
		resp.Diagnostics.AddError("Error reading bot tool assignments", err.Error())
		return
	}
	if apiResp.StatusCode() == 404 {
		// The bot no longer exists — drop the assignment from state.
		resp.State.RemoveResource(ctx)
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError("Unexpected response reading bot tool assignments",
			fmt.Sprintf("List returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	state.ID = types.StringValue(slug)
	state.ToolIDs = strSliceToSet(ctx, toolIDsFrom(apiResp.JSON200), &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *BotToolAssignmentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan BotToolAssignmentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.replace(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *BotToolAssignmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state BotToolAssignmentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ids := setToStrSlice(ctx, state.ToolIDs, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(ids) == 0 {
		return // nothing assigned by this resource
	}
	status, body, err := r.data.client.UnassignBotTools(ctx, r.data.orgID, state.BotSlug.ValueString(), ids)
	if err != nil {
		resp.Diagnostics.AddError("Error deleting bot tool assignments", err.Error())
		return
	}
	switch status {
	case 200, 202, 204, 404:
		// removed, or none of them were assigned any more
	default:
		resp.Diagnostics.AddError("Unexpected response deleting bot tool assignments",
			fmt.Sprintf("Delete returned HTTP %d: %s", status, describeAPIError(body)))
	}
}

// ImportState imports the assignment by the bot slug it manages.
func (r *BotToolAssignmentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("bot_slug"), req, resp)
}

// replace sets the bot's tool assignments to exactly the plan's tool_ids via the
// whole-collection PUT and writes the authoritative returned set back into the
// model. The replace response is the full resulting assignment list, so no
// follow-up read is needed. Shared by Create and Update.
func (r *BotToolAssignmentResource) replace(ctx context.Context, plan *BotToolAssignmentResourceModel, diags *diag.Diagnostics) {
	desired := setToStrSlice(ctx, plan.ToolIDs, diags)
	if diags.HasError() {
		return
	}
	slug := plan.BotSlug.ValueString()
	body, err := json.Marshal(client.BotToolAssignRequest{ToolIds: desired})
	if err != nil {
		diags.AddError("Error encoding bot tool assignment", err.Error())
		return
	}
	apiResp, err := r.data.client.ReplaceToolsV1OrgsOrgIdBotsBotSlugToolsPutWithBodyWithResponse(
		ctx, r.data.orgID, slug, "application/json", bytes.NewReader(body))
	if err != nil {
		diags.AddError("Error replacing bot tool assignments", err.Error())
		return
	}
	if apiResp.JSON200 == nil {
		diags.AddError("Unexpected response replacing bot tool assignments",
			fmt.Sprintf("Replace returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	plan.ID = types.StringValue(slug)
	plan.ToolIDs = strSliceToSet(ctx, toolIDsFrom(apiResp.JSON200), diags)
}

// toolIDsFrom extracts the tool catalog IDs from an assignment list, sorted for
// determinism.
func toolIDsFrom(links *[]client.BotToolAssignmentResponse) []string {
	ids := make([]string, 0, len(*links))
	for _, l := range *links {
		ids = append(ids, l.ToolId)
	}
	sort.Strings(ids)
	return ids
}
