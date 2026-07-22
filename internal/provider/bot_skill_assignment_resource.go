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
	_ resource.Resource                = (*BotSkillAssignmentResource)(nil)
	_ resource.ResourceWithConfigure   = (*BotSkillAssignmentResource)(nil)
	_ resource.ResourceWithImportState = (*BotSkillAssignmentResource)(nil)
)

// BotSkillAssignmentResource manages the complete set of skills assigned to a
// bot. It takes *exclusive* ownership of the bot's skill assignments: any skill
// assigned outside Terraform is removed on the next apply to converge on the
// declared `skill_ids`. This mirrors the exclusive-attachment pattern (e.g.
// aws_iam_policy_attachment) and maps naturally onto the additive assign / batch
// unassign / list endpoints the API exposes.
type BotSkillAssignmentResource struct {
	data *providerData
}

// BotSkillAssignmentResourceModel maps the botyard_bot_skill_assignment schema.
type BotSkillAssignmentResourceModel struct {
	ID       types.String `tfsdk:"id"`
	BotSlug  types.String `tfsdk:"bot_slug"`
	SkillIDs types.Set    `tfsdk:"skill_ids"`
}

// NewBotSkillAssignmentResource is the resource factory registered with the provider.
func NewBotSkillAssignmentResource() resource.Resource {
	return &BotSkillAssignmentResource{}
}

// Metadata sets the resource type name.
func (r *BotSkillAssignmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bot_skill_assignment"
}

// Schema defines the botyard_bot_skill_assignment resource schema.
func (r *BotSkillAssignmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the complete set of skills assigned to a Botyard bot. This resource takes " +
			"**exclusive** ownership of the bot's skill assignments: any skill assigned outside Terraform is " +
			"removed on the next apply so the bot's assigned skills converge on `skill_ids`. Use at most one " +
			"`botyard_bot_skill_assignment` per bot. Assigning a skill makes it available to the bot; the " +
			"separate deploy step that pushes SKILL.md files to a running pod is not modeled here.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier — the bot slug this assignment manages.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"bot_slug": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Slug of the bot whose skill assignments this resource manages. Changing it " +
					"forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"skill_ids": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				MarkdownDescription: "The exact, complete set of skill IDs assigned to the bot. Skills present on " +
					"the bot but absent from this set are unassigned on apply. An empty set removes all skills.",
			},
		},
	}
}

// Configure receives the shared provider data.
func (r *BotSkillAssignmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *BotSkillAssignmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan BotSkillAssignmentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *BotSkillAssignmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state BotSkillAssignmentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	slug := state.BotSlug.ValueString()
	ids, status, raw, err := listBotSkillIDs(ctx, r.data.client, r.data.orgID, slug)
	if err != nil {
		resp.Diagnostics.AddError("Error reading bot skill assignments", err.Error())
		return
	}
	if status == 404 {
		// The bot no longer exists — drop the assignment from state.
		resp.State.RemoveResource(ctx)
		return
	}
	if status != 200 {
		resp.Diagnostics.AddError("Unexpected response reading bot skill assignments",
			fmt.Sprintf("List returned HTTP %d: %s", status, describeAPIError(raw)))
		return
	}
	state.ID = types.StringValue(slug)
	state.SkillIDs = strSliceToSet(ctx, ids, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *BotSkillAssignmentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan BotSkillAssignmentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *BotSkillAssignmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state BotSkillAssignmentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ids := setToStrSlice(ctx, state.SkillIDs, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(ids) == 0 {
		return // nothing assigned by this resource
	}
	status, body, err := r.data.client.UnassignBotSkills(ctx, r.data.orgID, state.BotSlug.ValueString(), ids)
	if err != nil {
		resp.Diagnostics.AddError("Error deleting bot skill assignments", err.Error())
		return
	}
	switch status {
	case 200, 202, 204, 404:
		// removed, or none of them were assigned any more
	default:
		resp.Diagnostics.AddError("Unexpected response deleting bot skill assignments",
			fmt.Sprintf("Delete returned HTTP %d: %s", status, describeAPIError(body)))
	}
}

// ImportState imports the assignment by the bot slug it manages.
func (r *BotSkillAssignmentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("bot_slug"), req, resp)
}

// apply reconciles the bot's assigned skills to the plan's skill_ids and writes
// the authoritative server set back into the model. Shared by Create and Update.
func (r *BotSkillAssignmentResource) apply(ctx context.Context, plan *BotSkillAssignmentResourceModel, diags *diag.Diagnostics) {
	desired := setToStrSlice(ctx, plan.SkillIDs, diags)
	if diags.HasError() {
		return
	}
	slug := plan.BotSlug.ValueString()
	final := reconcileBotSkills(ctx, r.data.client, r.data.orgID, slug, desired, diags)
	if diags.HasError() {
		return
	}
	plan.ID = types.StringValue(slug)
	plan.SkillIDs = strSliceToSet(ctx, final, diags)
}

// reconcileBotSkills makes the bot's assigned skills exactly `desired`: it lists
// the current assignments, unassigns the extras (current − desired) via the
// batch DELETE, assigns the missing ones (desired − current) via the additive
// PUT, then re-lists and returns the authoritative server set. Both writes are
// skipped when there is nothing to do, so a converged assignment issues only the
// two idempotent reads. It is client-driven (no plugin-framework types) so it
// can be exercised hermetically against an httptest server.
func reconcileBotSkills(
	ctx context.Context,
	c *client.ClientWithResponses,
	orgID, botSlug string,
	desired []string,
	diags *diag.Diagnostics,
) []string {
	current, status, raw, err := listBotSkillIDs(ctx, c, orgID, botSlug)
	if err != nil {
		diags.AddError("Error reading bot skill assignments", err.Error())
		return nil
	}
	if status != 200 {
		diags.AddError("Unexpected response reading bot skill assignments",
			fmt.Sprintf("List returned HTTP %d: %s", status, describeAPIError(raw)))
		return nil
	}

	toAdd, toRemove := diffStrings(desired, current)

	if len(toRemove) > 0 {
		st, body, derr := c.UnassignBotSkills(ctx, orgID, botSlug, toRemove)
		if derr != nil {
			diags.AddError("Error unassigning bot skills", derr.Error())
			return nil
		}
		switch st {
		case 200, 202, 204, 404:
		default:
			diags.AddError("Unexpected response unassigning bot skills",
				fmt.Sprintf("Unassign returned HTTP %d: %s", st, describeAPIError(body)))
			return nil
		}
	}

	if len(toAdd) > 0 {
		body, err := json.Marshal(client.BotSkillAssign{SkillIds: toAdd})
		if err != nil {
			diags.AddError("Error encoding bot skill assignment", err.Error())
			return nil
		}
		apiResp, err := c.AssignSkillsV1OrgsOrgIdBotsBotSlugSkillsPutWithBodyWithResponse(
			ctx, orgID, botSlug, "application/json", bytes.NewReader(body))
		if err != nil {
			diags.AddError("Error assigning bot skills", err.Error())
			return nil
		}
		if apiResp.JSON201 == nil {
			diags.AddError("Unexpected response assigning bot skills",
				fmt.Sprintf("Assign returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
			return nil
		}
	}

	final, status, raw, err := listBotSkillIDs(ctx, c, orgID, botSlug)
	if err != nil {
		diags.AddError("Error reading bot skill assignments", err.Error())
		return nil
	}
	if status != 200 {
		diags.AddError("Unexpected response reading bot skill assignments",
			fmt.Sprintf("List returned HTTP %d: %s", status, describeAPIError(raw)))
		return nil
	}
	return final
}

// listBotSkillIDs GETs the bot's assigned skills and returns their skill IDs
// (sorted for determinism), the HTTP status, and the raw body. A non-200 status
// is returned to the caller to interpret (404 = bot gone); err is non-nil only
// for transport-level failures.
func listBotSkillIDs(ctx context.Context, c *client.ClientWithResponses, orgID, botSlug string) ([]string, int, []byte, error) {
	apiResp, err := c.ListBotSkillsV1OrgsOrgIdBotsBotSlugSkillsGetWithResponse(ctx, orgID, botSlug)
	if err != nil {
		return nil, 0, nil, err
	}
	if apiResp.JSON200 == nil {
		return nil, apiResp.StatusCode(), apiResp.Body, nil
	}
	ids := make([]string, 0, len(*apiResp.JSON200))
	for _, s := range *apiResp.JSON200 {
		ids = append(ids, s.SkillId)
	}
	sort.Strings(ids)
	return ids, apiResp.StatusCode(), apiResp.Body, nil
}

// diffStrings computes the set difference between the desired and current string
// slices: toAdd = desired − current, toRemove = current − desired. Inputs are
// treated as sets (deduplicated); the results are sorted for determinism.
func diffStrings(desired, current []string) (toAdd, toRemove []string) {
	desiredSet := make(map[string]struct{}, len(desired))
	for _, s := range desired {
		desiredSet[s] = struct{}{}
	}
	currentSet := make(map[string]struct{}, len(current))
	for _, s := range current {
		currentSet[s] = struct{}{}
	}
	for s := range desiredSet {
		if _, ok := currentSet[s]; !ok {
			toAdd = append(toAdd, s)
		}
	}
	for s := range currentSet {
		if _, ok := desiredSet[s]; !ok {
			toRemove = append(toRemove, s)
		}
	}
	sort.Strings(toAdd)
	sort.Strings(toRemove)
	return toAdd, toRemove
}
