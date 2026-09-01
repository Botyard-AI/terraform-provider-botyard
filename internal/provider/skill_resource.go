package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ resource.Resource                   = (*SkillResource)(nil)
	_ resource.ResourceWithConfigure      = (*SkillResource)(nil)
	_ resource.ResourceWithImportState    = (*SkillResource)(nil)
	_ resource.ResourceWithValidateConfig = (*SkillResource)(nil)
)

// Server-side limits mirrored from the API's skill file validation, so an
// oversized skill fails at plan time with a precise message instead of at apply
// time with a 422 (core/src/botyard_core/services/api/skill.py::_validate_files).
const (
	skillMaxFileBytes  = 100 * 1024
	skillMaxTotalBytes = 500 * 1024
	skillEntrypoint    = "SKILL.md"
)

// skillFrontmatterRE matches a leading YAML frontmatter block, mirroring the
// API's _FRONTMATTER_RE. The API strips frontmatter from SKILL.md before
// storing it, so declaring it in Terraform would read back different content on
// every refresh — a permanent diff. ValidateConfig rejects it up front.
var skillFrontmatterRE = regexp.MustCompile(`(?s)\A---\s*\n(.*?)\n---`)

// SkillResource manages a custom skill in the organization's skill catalogue.
//
// The catalogue also holds platform-provided (`provider != "custom"`) skills.
// Those are read-only server-side — the API refuses to update or delete them —
// so this resource only ever manages custom skills. Use the `botyard_skill` /
// `botyard_skills` data sources to reference platform skills, and
// `botyard_bot_skill_assignment` to assign either kind to a bot.
type SkillResource struct {
	data *providerData
}

// SkillResourceModel maps the botyard_skill resource schema.
//
// Notable omissions:
//   - `provider` — the API returns it, but `provider` is a reserved root
//     attribute name in Terraform, and its value is always "custom" for a
//     skill this resource manages (Read errors out if it is not).
//   - creator attribution (`created_by_*`) — display metadata that only churns
//     state; the vendored OpenAPI spec also predates `created_by_api_key_id`.
//   - per-file `id` / `content_hash` / `sort_order` — server-owned values that
//     would make every file element partially unknown at plan time. Order is
//     the list order; the hash is derivable from `content`.
type SkillResourceModel struct {
	Name    types.String     `tfsdk:"name"`
	Summary types.String     `tfsdk:"summary"`
	Scope   types.String     `tfsdk:"scope"`
	Files   []skillFileModel `tfsdk:"files"`

	ID        types.String `tfsdk:"id"`
	Slug      types.String `tfsdk:"slug"`
	CreatedAt types.String `tfsdk:"created_at"`
	UpdatedAt types.String `tfsdk:"updated_at"`
}

// skillFileModel maps one element of the `files` list.
type skillFileModel struct {
	Filename types.String `tfsdk:"filename"`
	Content  types.String `tfsdk:"content"`
}

// NewSkillResource is the resource factory registered with the provider.
func NewSkillResource() resource.Resource {
	return &SkillResource{}
}

// Metadata sets the resource type name.
func (r *SkillResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_skill"
}

// Schema defines the botyard_skill resource schema.
func (r *SkillResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a custom skill in the organization's skill catalogue: its metadata " +
			"(`name`, `summary`, `scope`) and the files that make it up. Skills are reusable instruction " +
			"bundles; assign them to bots with `botyard_bot_skill_assignment`.\n\n" +
			"Only custom skills are manageable — the API rejects edits and deletes of platform-provided " +
			"skills, so importing one fails by design. Use the `botyard_skill` data source to reference those.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Human-readable skill name (1–100 chars), unique within the organization. " +
					"The `slug` is derived from the name when the skill is created and is **not** recomputed on " +
					"rename, so renaming updates the display name in place and keeps the existing slug.",
			},
			"summary": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Brief description (1–500 chars) of what the skill does and when to use it. " +
					"This is the line an agent sees in its skill index, so make it specific.",
			},
			"scope": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(string(client.SkillScopeOrg)),
				MarkdownDescription: "Visibility scope: `org` (default — visible to the whole organization) or " +
					"`member` (visible only to its creator). The API allows only the skill's creator to change " +
					"scope or to edit a `member`-scoped skill, so keep the same credentials that created it.",
			},
			"files": schema.ListNestedAttribute{
				Required: true,
				MarkdownDescription: "Files comprising the skill, in order. Terraform owns the whole set: an " +
					"update replaces every file, so a file dropped from the list is deleted. Exactly one file " +
					"must be named `SKILL.md` — the entrypoint the agent reads.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"filename": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "Filename within the skill, e.g. `SKILL.md` or `references/api.md`.",
						},
						"content": schema.StringAttribute{
							Required: true,
							MarkdownDescription: "File content (max 100KB per file, 500KB per skill). Usually loaded " +
								"with `file(\"${path.module}/SKILL.md\")`. `SKILL.md` must not carry YAML " +
								"frontmatter — `name` and `summary` are structured fields here, and the API strips " +
								"frontmatter on write, which would leave a permanent diff.",
						},
					},
				},
			},

			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Unique skill identifier (UUID). Use this when assigning the skill to a bot.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"slug": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "URL-friendly identifier derived from the name at creation and stable " +
					"thereafter. Used as the import ID.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Creation timestamp (RFC 3339).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"updated_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Last-update timestamp (RFC 3339).",
			},
		},
	}
}

// Configure receives the shared provider data.
func (r *SkillResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ValidateConfig enforces the file-set rules the schema cannot express, so they
// fail at plan time rather than as an opaque 422 mid-apply.
func (r *SkillResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg SkillResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(validateSkillConfig(cfg)...)
}

// validateSkillConfig is the pure, unit-testable configuration validation.
// Unknown values (e.g. content interpolated from another resource) are skipped:
// they are only checkable at apply time, where the API validates them anyway.
func validateSkillConfig(cfg SkillResourceModel) diag.Diagnostics {
	var diags diag.Diagnostics

	if !cfg.Scope.IsNull() && !cfg.Scope.IsUnknown() {
		switch cfg.Scope.ValueString() {
		case string(client.SkillScopeOrg), string(client.SkillScopeMember):
		default:
			diags.AddAttributeError(path.Root("scope"), "Invalid scope",
				"scope must be `org` or `member`.")
		}
	}

	if cfg.Files == nil {
		return diags
	}
	if len(cfg.Files) == 0 {
		diags.AddAttributeError(path.Root("files"), "Skill has no files",
			"A skill must declare at least one file, including a `SKILL.md` at its root.")
		return diags
	}

	seen := make(map[string]struct{}, len(cfg.Files))
	hasEntrypoint := false
	total := 0
	anyUnknownName := false

	for i, f := range cfg.Files {
		filePath := path.Root("files").AtListIndex(i)

		if f.Filename.IsUnknown() {
			anyUnknownName = true
		} else if name := f.Filename.ValueString(); name == "" {
			diags.AddAttributeError(filePath.AtName("filename"), "Empty filename",
				"Every skill file must have a non-empty filename.")
		} else {
			if _, dup := seen[name]; dup {
				diags.AddAttributeError(filePath.AtName("filename"), "Duplicate filename",
					fmt.Sprintf("Filename %q is declared more than once; skill filenames must be unique.", name))
			}
			seen[name] = struct{}{}
			if name == skillEntrypoint {
				hasEntrypoint = true
			}
		}

		if f.Content.IsUnknown() {
			continue
		}
		content := f.Content.ValueString()
		total += len(content)
		if len(content) > skillMaxFileBytes {
			diags.AddAttributeError(filePath.AtName("content"), "Skill file too large",
				fmt.Sprintf("File %q is %d bytes; the API limit is %d bytes per file.",
					f.Filename.ValueString(), len(content), skillMaxFileBytes))
		}
		if f.Filename.ValueString() == skillEntrypoint && skillFrontmatterRE.MatchString(content) {
			diags.AddAttributeError(filePath.AtName("content"), "SKILL.md must not contain YAML frontmatter",
				"The API strips leading `---` frontmatter from SKILL.md before storing it, so Terraform would "+
					"read back different content than it wrote and plan a change on every run. Remove the "+
					"frontmatter block and set the skill's `name` and `summary` arguments instead.")
		}
	}

	if !hasEntrypoint && !anyUnknownName {
		diags.AddAttributeError(path.Root("files"), "Missing SKILL.md",
			"Every skill must contain a `SKILL.md` file at its root — this is how an agent discovers the skill.")
	}
	if total > skillMaxTotalBytes {
		diags.AddAttributeError(path.Root("files"), "Skill too large",
			fmt.Sprintf("The declared files total %d bytes; the API limit is %d bytes per skill.",
				total, skillMaxTotalBytes))
	}

	return diags
}

func (r *SkillResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan SkillResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiResp, err := r.data.client.CreateSkillV1OrgsOrgIdSkillsPostWithResponse(ctx, r.data.orgID, client.SkillCreate{
		Name:    plan.Name.ValueString(),
		Summary: plan.Summary.ValueString(),
		Scope:   client.SkillScope(plan.Scope.ValueString()),
		Files:   skillFileInputs(plan.Files),
	})
	if err != nil {
		resp.Diagnostics.AddError("Error creating skill", err.Error())
		return
	}
	if apiResp.JSON201 == nil {
		resp.Diagnostics.AddError("Unexpected response creating skill",
			fmt.Sprintf("Create returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	mapSkillResource(apiResp.JSON201, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *SkillResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state SkillResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiResp, err := r.data.client.GetSkillV1OrgsOrgIdSkillsSkillSlugGetWithResponse(ctx, r.data.orgID, state.Slug.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading skill", err.Error())
		return
	}
	if apiResp.StatusCode() == 404 {
		resp.State.RemoveResource(ctx)
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError("Unexpected response reading skill",
			fmt.Sprintf("Read returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	if diags := checkSkillManageable(apiResp.JSON200); diags.HasError() {
		resp.Diagnostics.Append(diags...)
		return
	}
	mapSkillResource(apiResp.JSON200, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *SkillResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state SkillResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, err := buildSkillUpdateBody(plan, state)
	if err != nil {
		resp.Diagnostics.AddError("Error encoding skill update", err.Error())
		return
	}

	apiResp, err := r.data.client.UpdateSkillV1OrgsOrgIdSkillsSkillSlugPatchWithBodyWithResponse(
		ctx, r.data.orgID, state.Slug.ValueString(), "application/json", bytes.NewReader(body))
	if err != nil {
		resp.Diagnostics.AddError("Error updating skill", err.Error())
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError("Unexpected response updating skill",
			fmt.Sprintf("Update returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	mapSkillResource(apiResp.JSON200, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *SkillResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state SkillResourceModel
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

// ImportState imports an existing skill by its slug (the API's URL identifier).
func (r *SkillResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("slug"), req, resp)
}

// checkSkillManageable rejects a skill the API will not let this resource edit
// or delete. It fires on import of a platform-provided skill, and on the (only
// server-side reachable) case of a managed skill changing provider.
func checkSkillManageable(s *client.SkillResponse) diag.Diagnostics {
	var diags diag.Diagnostics
	if s.Provider != client.SkillProviderCustom {
		diags.AddError("Skill is not managed by Terraform",
			fmt.Sprintf("Skill %q has provider %q, but the API only permits updating or deleting `custom` "+
				"skills. Reference it with the `botyard_skill` data source instead of managing it as a resource.",
				s.Slug, string(s.Provider)))
	}
	return diags
}

// skillDeleteStatusAccepted reports whether a DELETE /skills/{slug} status means
// the skill is gone. 404 counts: something else already removed it.
func skillDeleteStatusAccepted(code int) bool {
	switch code {
	case 200, 202, 204, 404:
		return true
	default:
		return false
	}
}

// skillFileInputs converts the declared file list into the API's create/update
// file payload, preserving order (the API stores the list index as sort_order
// and returns files in that order).
func skillFileInputs(files []skillFileModel) []client.SkillFileInput {
	out := make([]client.SkillFileInput, 0, len(files))
	for _, f := range files {
		out = append(out, client.SkillFileInput{
			Filename: f.Filename.ValueString(),
			Content:  f.Content.ValueString(),
		})
	}
	return out
}

// buildSkillUpdateBody builds a genuinely sparse PATCH body: only the fields
// that actually changed are sent.
//
// This is not just bandwidth. The API gates `scope` on *presence*, not on
// change — supplying `scope` at all requires the caller to be the skill's
// creator, so always sending it would break every update made with credentials
// other than the ones that created the skill. Sending `files` unconditionally
// would likewise delete and reinsert every file row on every apply and bump the
// config generation of every bot the skill is assigned to.
func buildSkillUpdateBody(plan, state SkillResourceModel) ([]byte, error) {
	body := map[string]json.RawMessage{}
	if !plan.Name.Equal(state.Name) {
		body["name"] = rawString(plan.Name)
	}
	if !plan.Summary.Equal(state.Summary) {
		body["summary"] = rawString(plan.Summary)
	}
	if !plan.Scope.Equal(state.Scope) {
		body["scope"] = rawString(plan.Scope)
	}
	if !skillFilesEqual(plan.Files, state.Files) {
		files, err := json.Marshal(skillFileInputs(plan.Files))
		if err != nil {
			return nil, err
		}
		body["files"] = files
	}
	return json.Marshal(body)
}

// skillFilesEqual reports whether two file lists are identical in content and
// order. Order matters: it is what the API persists as sort_order.
func skillFilesEqual(a, b []skillFileModel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Filename.Equal(b[i].Filename) || !a[i].Content.Equal(b[i].Content) {
			return false
		}
	}
	return true
}

// mapSkillResource writes an API SkillResponse into the resource model.
func mapSkillResource(s *client.SkillResponse, m *SkillResourceModel) {
	m.Name = types.StringValue(s.Name)
	m.Summary = types.StringValue(s.Summary)
	m.Scope = types.StringValue(string(s.Scope))

	files := make([]skillFileModel, 0, len(s.Files))
	for _, f := range s.Files {
		files = append(files, skillFileModel{
			Filename: types.StringValue(f.Filename),
			Content:  types.StringValue(f.Content),
		})
	}
	m.Files = files

	m.ID = types.StringValue(s.Id)
	m.Slug = types.StringValue(s.Slug)
	m.CreatedAt = types.StringValue(s.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(s.UpdatedAt.Format(time.RFC3339))
}
