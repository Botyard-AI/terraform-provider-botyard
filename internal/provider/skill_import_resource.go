package provider

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ resource.Resource                   = (*SkillImportResource)(nil)
	_ resource.ResourceWithConfigure      = (*SkillImportResource)(nil)
	_ resource.ResourceWithImportState    = (*SkillImportResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*SkillImportResource)(nil)
	_ resource.ResourceWithValidateConfig = (*SkillImportResource)(nil)
)

// skillImportSourceMaxLen mirrors SkillImportRequest.source's maxLength.
const skillImportSourceMaxLen = 500

// SkillImportResource manages a catalogue skill whose content is pulled from a
// source repository (POST /skills/import) and kept on the pinned ref by
// refreshing in place (POST /skills/{slug}/refresh).
//
// The design point is identity: a ref bump is an in-place refresh, never a
// replacement, because a replacement mints a new skill id and silently drops
// every bot assignment that pointed at the old one. `source` therefore has no
// RequiresReplace; every source change is routed through /refresh.
//
// Terraform plan never talks to GitHub. The only server state a plan compares
// against is the recorded provenance from the last refresh, so a floating ref
// (a branch) stays on the commit it last resolved to until the configuration
// asks for something different.
type SkillImportResource struct {
	data *providerData
}

// SkillImportResourceModel maps the botyard_skill_import resource schema.
//
// `provider` and creator attribution are omitted for the same reasons as on
// botyard_skill (see SkillResourceModel). `updated_at` is omitted because it
// would move on every refresh and show up as noise in every plan.
type SkillImportResourceModel struct {
	Source types.String `tfsdk:"source"`
	Name   types.String `tfsdk:"name"`
	Force  types.Bool   `tfsdk:"force"`

	ID      types.String `tfsdk:"id"`
	Slug    types.String `tfsdk:"slug"`
	Summary types.String `tfsdk:"summary"`
	Scope   types.String `tfsdk:"scope"`
	Files   types.List   `tfsdk:"files"`

	SourceKind types.String `tfsdk:"source_kind"`
	SourceURL  types.String `tfsdk:"source_url"`
	Ref        types.String `tfsdk:"ref"`
	Path       types.String `tfsdk:"path"`
	CommitSHA  types.String `tfsdk:"commit_sha"`
	ImportedAt types.String `tfsdk:"imported_at"`
}

// skillImportFileObjectType is the element type of the read-only `files` list.
func skillImportFileObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"filename": types.StringType,
		"content":  types.StringType,
	}}
}

// skillImportFilesValue converts the API's files into the `files` list value,
// keeping the API's order (sort_order).
func skillImportFilesValue(files []client.SkillFileResponse) (types.List, diag.Diagnostics) {
	var diags diag.Diagnostics
	objType := skillImportFileObjectType()
	elems := make([]attr.Value, 0, len(files))
	for _, f := range files {
		obj, d := types.ObjectValue(objType.AttrTypes, map[string]attr.Value{
			"filename": types.StringValue(f.Filename),
			"content":  types.StringValue(f.Content),
		})
		diags.Append(d...)
		elems = append(elems, obj)
	}
	list, d := types.ListValue(objType, elems)
	diags.Append(d...)
	return list, diags
}

// provenanceAttrs are the Computed attributes whose values are re-derived from
// the server on every create/refresh. ModifyPlan marks them unknown when an
// apply will call the API, and keeps them from state otherwise.
var provenanceAttrs = []string{
	"summary", "source_kind", "source_url", "ref", "path", "commit_sha", "imported_at",
}

// NewSkillImportResource is the resource factory registered with the provider.
func NewSkillImportResource() resource.Resource {
	return &SkillImportResource{}
}

// Metadata sets the resource type name.
func (r *SkillImportResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_skill_import"
}

// Schema defines the botyard_skill_import resource schema.
func (r *SkillImportResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	computedString := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: desc,
			PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		}
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "Imports a skill into the organization's catalogue from a GitHub repository " +
			"and keeps it on the ref you pin.\n\n" +
			"Changing `source` — typically bumping the pinned ref from `#v1` to `#v2` — **refreshes the " +
			"skill in place**: the skill keeps its `id`, so every bot assignment pointing at it survives. " +
			"`terraform plan` never contacts GitHub; the plan compares your configuration against the " +
			"provenance recorded by the last import or refresh. A branch ref therefore stays on the commit " +
			"it last resolved to until the configuration changes. Pin a tag or commit for reproducibility.\n\n" +
			"**Local edits.** Editing an imported skill's content in Botyard detaches it from its source. " +
			"On the next plan this resource shows an update, and by default the apply **fails** with an " +
			"explanation rather than overwriting the edit. Either set `force = true` to discard the edit and " +
			"re-attach the skill to `source`, or run `terraform state rm` to hand the skill over to whoever " +
			"edited it.\n\n" +
			"**Private repositories** need no credential here. The API key the provider uses must belong to " +
			"an actor holding the `skill:private_source.create` permission, and the organization must have a " +
			"GitHub integration connected; the server fetches with that integration.\n\n" +
			"To author a skill's files directly in Terraform instead, use `botyard_skill`.",
		Attributes: map[string]schema.Attribute{
			"source": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Where to fetch the skill from (max 500 characters). Accepts a repository " +
					"(`owner/repo`), a directory in one (`owner/repo/path/to/skill`), a named skill in one " +
					"(`owner/repo@skill-name`), a github.com URL, or a skills.sh URL. Pin a branch, tag, or " +
					"commit by appending `#ref` (for example `acme/skills/deploy#v1.2.0`).\n\n" +
					"Changing only the `#ref` of the same coordinate refreshes the skill at the new ref. " +
					"Changing the repository, path, or skill name re-points the skill at the new source, also " +
					"in place — the skill keeps its `id` either way.",
			},
			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Catalogue name for the skill. Defaults to the name declared by the source. " +
					"Set it to resolve a collision with a skill that already exists in the catalogue. The " +
					"name is only applied at import time, so changing it **replaces** the skill (new `id`).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplaceIf(skillImportNameChanged,
						"Changing an explicit name re-imports the skill.",
						"Changing an explicit name re-imports the skill."),
				},
			},
			"force": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Permit an apply to overwrite skill content that was edited in Botyard. " +
					"An edit detaches a skill from its source; by default the next apply fails and names the " +
					"edit instead of reverting it. With `force = true` the apply discards the edit and " +
					"re-attaches the skill to `source`. This also lets you adopt a skill that was authored in " +
					"Botyard (imported with `terraform import`) by attaching it to a source. Leaving this " +
					"`true` means later edits are also overwritten without warning, so prefer setting it for " +
					"one apply and removing it afterwards.",
			},

			"id": computedString("Unique skill identifier (UUID). Preserved across ref changes; use it " +
				"when assigning the skill to a bot."),
			"slug": computedString("URL-friendly identifier, derived from the name at import and stable " +
				"thereafter. Used as the import ID."),
			"summary": computedString("Catalogue summary, taken from the source."),
			"scope":   computedString("Visibility scope. Imported skills are currently always `org`-scoped."),
			"files": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The skill's files as imported, in order. Read-only: the source " +
					"repository owns them.",
				PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"filename": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Filename within the skill, e.g. `SKILL.md`.",
						},
						"content": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "File content.",
						},
					},
				},
			},
			"source_kind": computedString("Where the content was fetched from. Currently always `github` " +
				"(a skills.sh reference resolves to GitHub)."),
			"source_url": computedString("Canonical repository URL, e.g. `https://github.com/owner/repo`."),
			"ref":        computedString("The ref recorded for the source; null means the default branch."),
			"path":       computedString("Directory holding `SKILL.md` within the repository; null means the root."),
			"commit_sha": computedString("Commit the content actually came from — the reproducible pin. " +
				"Null when the skill has been detached from its source by a local edit."),
			"imported_at": computedString("When the skill was last imported or refreshed from its source " +
				"(RFC 3339)."),
		},
	}
}

// skillImportNameChanged replaces only when an explicit name differs from
// state. An omitted name is Computed and follows state (UseStateForUnknown).
func skillImportNameChanged(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() || req.StateValue.IsNull() {
		return
	}
	resp.RequiresReplace = !req.ConfigValue.Equal(req.StateValue)
}

// Configure receives the shared provider data.
func (r *SkillImportResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ValidateConfig catches the source mistakes the API would otherwise reject
// mid-apply. The full grammar is the server's; only the cheap, unambiguous
// rules are mirrored here.
func (r *SkillImportResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var source types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("source"), &source)...)
	if resp.Diagnostics.HasError() || source.IsNull() || source.IsUnknown() {
		return
	}
	resp.Diagnostics.Append(validateSkillImportSource(source.ValueString())...)
}

func validateSkillImportSource(s string) diag.Diagnostics {
	var diags diag.Diagnostics
	trimmed := strings.TrimSpace(s)
	switch {
	case trimmed == "":
		diags.AddAttributeError(path.Root("source"), "Empty source",
			"source must name a skill, e.g. `owner/repo` or `owner/repo/path/to/skill#v1`.")
	case len(s) > skillImportSourceMaxLen:
		diags.AddAttributeError(path.Root("source"), "Source too long",
			fmt.Sprintf("source is %d characters; the API limit is %d.", len(s), skillImportSourceMaxLen))
	case strings.IndexFunc(trimmed, unicode.IsSpace) >= 0:
		diags.AddAttributeError(path.Root("source"), "Source contains whitespace",
			"source must not contain whitespace.")
	}
	return diags
}

// ModifyPlan decides whether an apply will call the API, and makes the plan
// say so.
//
// Attribute-level UseStateForUnknown keeps every Computed value stable, which
// is right for a no-op plan. But a refresh rewrites the provenance, files and
// summary, so whenever an apply will refresh, those are marked unknown here.
//
// A refresh is planned when:
//   - `source` changed in configuration;
//   - the skill was detached by a local edit (state has no commit_sha) — the
//     apply then fails unless `force` is set; or
//   - the `#ref` in configuration no longer matches the ref recorded on the
//     server, i.e. someone refreshed the skill at a different ref out of band.
func (r *SkillImportResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return // destroy, or create (everything is already unknown).
	}
	var plan, state SkillImportResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.Source.IsUnknown() {
		markSkillImportRefreshUnknown(ctx, resp)
		return
	}

	detached := state.CommitSHA.IsNull()
	if !plannedSkillImportRefresh(plan, state) {
		return
	}
	markSkillImportRefreshUnknown(ctx, resp)

	if detached && !plan.Force.ValueBool() {
		resp.Diagnostics.AddAttributeWarning(path.Root("source"), "Skill has local edits",
			fmt.Sprintf("Skill %q is not attached to a source: it was edited in Botyard, or authored there. "+
				"This apply will fail rather than overwrite it. Set `force = true` to discard the "+
				"edits and re-attach it to %q, or run `terraform state rm` on this resource to leave "+
				"the skill as edited.", state.Slug.ValueString(), plan.Source.ValueString()))
	}
}

// plannedSkillImportRefresh reports whether an apply of plan over state needs
// to call /refresh. It is pure so it can be unit-tested without a plan.
func plannedSkillImportRefresh(plan, state SkillImportResourceModel) bool {
	if state.CommitSHA.IsNull() {
		return true // detached (or adopted from a never-imported skill)
	}
	if !plan.Source.Equal(state.Source) {
		return true
	}
	return skillImportRefDrifted(plan.Source.ValueString(), state.Ref)
}

// skillImportRefDrifted reports whether the ref pinned in source disagrees
// with the ref the server has recorded. It only judges sources whose ref is
// unambiguous from the string: a `#ref` fragment, or no ref at all on a
// non-URL source. A github.com `/tree/<ref>` URL is left alone, because a ref
// containing `/` cannot be split from the path without the repository.
func skillImportRefDrifted(source string, recorded types.String) bool {
	parsed := parseSkillImportSource(source)
	if parsed.ref != nil {
		return recorded.IsNull() || recorded.ValueString() != *parsed.ref
	}
	if strings.Contains(strings.ToLower(parsed.head), "://") {
		return false
	}
	return !recorded.IsNull()
}

func markSkillImportRefreshUnknown(ctx context.Context, resp *resource.ModifyPlanResponse) {
	for _, name := range provenanceAttrs {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(name), types.StringUnknown())...)
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("files"),
		types.ListUnknown(skillImportFileObjectType()))...)
}

func (r *SkillImportResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan SkillImportResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := client.SkillImportRequest{Source: plan.Source.ValueString()}
	if !plan.Name.IsNull() && !plan.Name.IsUnknown() {
		name := plan.Name.ValueString()
		body.Name = &name
	}
	apiResp, err := r.data.client.ImportSkillV1OrgsOrgIdSkillsImportPostWithResponse(ctx, r.data.orgID, body)
	if err != nil {
		resp.Diagnostics.AddError("Error importing skill", err.Error())
		return
	}
	if apiResp.JSON201 == nil {
		resp.Diagnostics.AddError("Unexpected response importing skill",
			fmt.Sprintf("Import of %q returned HTTP %d: %s", plan.Source.ValueString(),
				apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	resp.Diagnostics.Append(mapSkillImport(apiResp.JSON201, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *SkillImportResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state SkillImportResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	skill, found, diags := r.getSkill(ctx, state.Slug.ValueString())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(mapSkillImport(skill, &state)...)
	// After `terraform import` there is no configured source in state yet;
	// rebuild one from the recorded provenance so the first plan is clean
	// when the configuration uses the same form.
	if state.Source.IsNull() && skill.Source != nil {
		state.Source = types.StringValue(skillImportSourceFromProvenance(skill.Source))
	}
	if state.Force.IsNull() {
		state.Force = types.BoolValue(false)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *SkillImportResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state SkillImportResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	slug := state.Slug.ValueString()

	// Decide against the skill as it is now, not as it was at plan time: the
	// question "does this apply overwrite someone's edit?" must be answered as
	// close to the write as possible.
	current, found, diags := r.getSkill(ctx, slug)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.Diagnostics.AddError("Skill no longer exists",
			fmt.Sprintf("Skill %q was deleted outside Terraform. Run `terraform apply` again to re-import it.", slug))
		return
	}

	action := decideSkillImportRefresh(plan, state, current, plan.Force.ValueBool())
	switch action.kind {
	case skillRefreshNone:
		resp.Diagnostics.Append(mapSkillImport(current, &plan)...)
	case skillRefreshBlocked:
		resp.Diagnostics.Append(skillImportDetachedError(slug, plan.Source.ValueString()))
		return
	default:
		refreshed, rdiags := r.refresh(ctx, slug, plan.Source.ValueString(), action.body)
		resp.Diagnostics.Append(rdiags...)
		if resp.Diagnostics.HasError() {
			return
		}
		resp.Diagnostics.Append(mapSkillImport(refreshed, &plan)...)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *SkillImportResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state SkillImportResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	status, body, err := r.data.client.DeleteSkill(ctx, r.data.orgID, state.Slug.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error deleting skill", err.Error())
		return
	}
	if !skillDeleteStatusAccepted(status) {
		resp.Diagnostics.AddError("Unexpected response deleting skill",
			fmt.Sprintf("Delete returned HTTP %d: %s", status, describeAPIError(body)))
	}
}

// ImportState imports an existing custom skill by slug. Read then fills every
// provenance attribute and, for a skill that is attached to a source, `source`.
func (r *SkillImportResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("slug"), req, resp)
}

// getSkill GETs a skill by slug. found is false on 404. A non-custom skill is
// an error: the API refuses to refresh or delete it.
func (r *SkillImportResource) getSkill(ctx context.Context, slug string) (*client.SkillResponse, bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	apiResp, err := r.data.client.GetSkillV1OrgsOrgIdSkillsSkillSlugGetWithResponse(ctx, r.data.orgID, slug)
	if err != nil {
		diags.AddError("Error reading skill", err.Error())
		return nil, false, diags
	}
	if apiResp.StatusCode() == 404 {
		return nil, false, diags
	}
	if apiResp.JSON200 == nil {
		diags.AddError("Unexpected response reading skill",
			fmt.Sprintf("Read returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return nil, false, diags
	}
	diags.Append(checkSkillManageable(apiResp.JSON200)...)
	return apiResp.JSON200, true, diags
}

func (r *SkillImportResource) refresh(ctx context.Context, slug, source string, body client.SkillRefreshRequest) (*client.SkillResponse, diag.Diagnostics) {
	var diags diag.Diagnostics
	apiResp, err := r.data.client.RefreshSkillV1OrgsOrgIdSkillsSkillSlugRefreshPostWithResponse(ctx, r.data.orgID, slug, body)
	if err != nil {
		diags.AddError("Error refreshing skill", err.Error())
		return nil, diags
	}
	if apiResp.StatusCode() == 409 {
		// The skill was edited between our GET and the refresh.
		diags.Append(skillImportDetachedError(slug, source))
		return nil, diags
	}
	if apiResp.JSON200 == nil {
		diags.AddError("Unexpected response refreshing skill",
			fmt.Sprintf("Refresh of %q returned HTTP %d: %s", slug, apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return nil, diags
	}
	return apiResp.JSON200, diags
}

func skillImportDetachedError(slug, source string) diag.Diagnostic {
	return diag.NewAttributeErrorDiagnostic(path.Root("source"), "Skill has local edits",
		fmt.Sprintf("Skill %q is not attached to a source: it was edited in Botyard after import, or "+
			"authored there. Terraform will not overwrite that content by default.\n\n"+
			"To keep Terraform in charge, set `force = true` on this resource and apply again. The edits are "+
			"discarded and the skill is re-attached to %q, keeping its id and bot assignments. Remove "+
			"`force` again afterwards.\n\n"+
			"To keep the edits, run `terraform state rm` on this resource and remove it from your "+
			"configuration. The skill then stays in the catalogue, unmanaged.", slug, source))
}

type skillRefreshKind int

const (
	// skillRefreshNone: the server already matches the configuration.
	skillRefreshNone skillRefreshKind = iota
	// skillRefreshRef: same coordinate, new ref — POST {ref}.
	skillRefreshRef
	// skillRefreshRepoint: different coordinate — POST {source, force}.
	skillRefreshRepoint
	// skillRefreshBlocked: detached and force is off — fail the apply.
	skillRefreshBlocked
)

type skillRefreshAction struct {
	kind skillRefreshKind
	body client.SkillRefreshRequest
}

// decideSkillImportRefresh picks the /refresh call an update needs. current is
// the skill as fetched immediately before the write.
//
//   - Detached (no provenance): only force may overwrite it, by re-attaching to
//     the configured source. Without force, the apply is blocked.
//   - Same coordinate as last applied (repository/path/skill name unchanged):
//     send only `{ref}`. The server re-resolves its recorded repository and
//     path at the new ref, and no-ops if the commit is unchanged. `source` is
//     never sent on this path, so no force is involved.
//   - Anything else — a new repository/path/skill, or a `#ref` removed to go
//     back to the default branch (a null `ref` means "the recorded ref" to
//     the API) — re-points with `{source, force: true}`. `force` is the API's
//     required acknowledgement that the recorded origin is replaced; it
//     overwrites no local edit because the skill was attached a moment ago.
func decideSkillImportRefresh(plan, state SkillImportResourceModel, current *client.SkillResponse, force bool) skillRefreshAction {
	desired := plan.Source.ValueString()
	if current.Source == nil {
		if !force {
			return skillRefreshAction{kind: skillRefreshBlocked}
		}
		return repointAction(desired)
	}

	if state.Source.IsNull() {
		// Adopted by import but the rebuilt source differed from config.
		return repointAction(desired)
	}
	next := parseSkillImportSource(desired)
	prev := parseSkillImportSource(state.Source.ValueString())
	if next.head != prev.head || next.skill != prev.skill {
		return repointAction(desired)
	}

	recorded := current.Source.Ref
	switch {
	case next.ref != nil && (recorded == nil || *recorded != *next.ref):
		ref := *next.ref
		return skillRefreshAction{kind: skillRefreshRef, body: client.SkillRefreshRequest{Ref: &ref}}
	case next.ref == nil && prev.ref != nil:
		return repointAction(desired)
	case next.ref == nil && recorded != nil && !strings.Contains(strings.ToLower(next.head), "://"):
		// Pinned out of band; config wants the default branch.
		return repointAction(desired)
	}
	return skillRefreshAction{kind: skillRefreshNone}
}

func repointAction(source string) skillRefreshAction {
	s := source
	force := true
	return skillRefreshAction{kind: skillRefreshRepoint, body: client.SkillRefreshRequest{Source: &s, Force: &force}}
}

// parsedSkillSource is the part of the server's reference grammar the provider
// needs to classify a change (core/.../skill_import/source.py::_split_fragment).
type parsedSkillSource struct {
	head  string  // everything before the first '#'
	ref   *string // the fragment ref, nil when absent or empty
	skill string  // the fragment's '@skill' selector, "" when absent
}

// parseSkillImportSource mirrors the server's _split_fragment: split on the
// first '#', then split the fragment on the first '@', percent-decoding both.
func parseSkillImportSource(source string) parsedSkillSource {
	head, fragment, found := strings.Cut(strings.TrimSpace(source), "#")
	out := parsedSkillSource{head: strings.TrimPrefix(head, "github:")}
	if !found || fragment == "" {
		return out
	}
	ref, skill, hasSkill := strings.Cut(fragment, "@")
	if ref != "" {
		decoded := unquoteSkillRef(ref)
		out.ref = &decoded
	}
	if hasSkill {
		out.skill = unquoteSkillRef(skill)
	}
	return out
}

// unquoteSkillRef matches Python's urllib.parse.unquote closely enough for
// refs: invalid escapes are left as they are rather than rejected.
func unquoteSkillRef(s string) string {
	if decoded, err := url.PathUnescape(s); err == nil {
		return decoded
	}
	return s
}

// skillImportSourceFromProvenance rebuilds a source reference from recorded
// provenance: `owner/repo[/path][#ref]`. The path form is used (not
// `@skill-name`) because the path is what the server pins to.
func skillImportSourceFromProvenance(src *client.SkillSourceResponse) string {
	repo := strings.TrimSuffix(src.Url, "/")
	repo = strings.TrimSuffix(repo, ".git")
	if u, err := url.Parse(repo); err == nil && u.Host != "" {
		repo = strings.TrimPrefix(u.Path, "/")
	}
	out := repo
	if src.Path != nil && strings.Trim(*src.Path, "/") != "" {
		out += "/" + strings.Trim(*src.Path, "/")
	}
	if src.Ref != nil && *src.Ref != "" {
		out += "#" + escapeSkillRef(*src.Ref)
	}
	return out
}

// escapeSkillRef escapes only the characters the fragment grammar treats as
// delimiters, so common refs like `release/v2` stay readable.
func escapeSkillRef(ref string) string {
	return strings.NewReplacer("%", "%25", "#", "%23", "@", "%40").Replace(ref)
}

// mapSkillImport writes an API SkillResponse into the resource model. It never
// touches `source` or `force`: those belong to the configuration.
func mapSkillImport(s *client.SkillResponse, m *SkillImportResourceModel) diag.Diagnostics {
	m.ID = types.StringValue(s.Id)
	m.Slug = types.StringValue(s.Slug)
	m.Name = types.StringValue(s.Name)
	m.Summary = types.StringValue(s.Summary)
	m.Scope = types.StringValue(string(s.Scope))

	files, diags := skillImportFilesValue(s.Files)
	m.Files = files

	if s.Source == nil {
		m.SourceKind = types.StringNull()
		m.SourceURL = types.StringNull()
		m.Ref = types.StringNull()
		m.Path = types.StringNull()
		m.CommitSHA = types.StringNull()
		m.ImportedAt = types.StringNull()
		return diags
	}
	m.SourceKind = types.StringValue(string(s.Source.Kind))
	m.SourceURL = types.StringValue(s.Source.Url)
	m.Ref = types.StringPointerValue(s.Source.Ref)
	m.Path = types.StringPointerValue(s.Source.Path)
	m.CommitSHA = types.StringValue(s.Source.CommitSha)
	m.ImportedAt = types.StringValue(s.Source.ImportedAt.UTC().Format(time.RFC3339))
	return diags
}
