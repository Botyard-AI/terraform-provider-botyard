package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ datasource.DataSource              = (*SkillDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*SkillDataSource)(nil)
	_ datasource.DataSource              = (*SkillsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*SkillsDataSource)(nil)
)

// skillsPageSize is the page size used when walking the paginated /skills
// catalogue endpoint.
const skillsPageSize = 100

// SkillModel maps one skill-catalogue entry into Terraform state for the plural
// botyard_skills list. It mirrors SkillSummaryResponse and includes `provider`,
// which is only legal as a nested (non-root) attribute name.
type SkillModel struct {
	ID        types.String `tfsdk:"id"`
	Slug      types.String `tfsdk:"slug"`
	Name      types.String `tfsdk:"name"`
	Summary   types.String `tfsdk:"summary"`
	Scope     types.String `tfsdk:"scope"`
	Provider  types.String `tfsdk:"provider"`
	FileCount types.Int64  `tfsdk:"file_count"`
	CreatedAt types.String `tfsdk:"created_at"`
	UpdatedAt types.String `tfsdk:"updated_at"`
}

// SkillDataSourceModel is the singular botyard_skill state. It intentionally
// omits `provider`: `provider` is a reserved Terraform attribute name at the
// root of a data source, so per-skill provider is exposed only by the plural
// botyard_skills (where it lives inside a nested object). The `slug` field is
// the lookup key.
type SkillDataSourceModel struct {
	Slug      types.String `tfsdk:"slug"`
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	Summary   types.String `tfsdk:"summary"`
	Scope     types.String `tfsdk:"scope"`
	FileCount types.Int64  `tfsdk:"file_count"`
	CreatedAt types.String `tfsdk:"created_at"`
	UpdatedAt types.String `tfsdk:"updated_at"`
}

// skillToModel maps an API SkillSummaryResponse into the plural element model.
func skillToModel(s client.SkillSummaryResponse) SkillModel {
	return SkillModel{
		ID:        types.StringValue(s.Id),
		Slug:      types.StringValue(s.Slug),
		Name:      types.StringValue(s.Name),
		Summary:   types.StringValue(s.Summary),
		Scope:     types.StringValue(string(s.Scope)),
		Provider:  types.StringValue(string(s.Provider)),
		FileCount: types.Int64Value(int64(s.FileCount)),
		CreatedAt: types.StringValue(s.CreatedAt.Format(time.RFC3339)),
		UpdatedAt: types.StringValue(s.UpdatedAt.Format(time.RFC3339)),
	}
}

// skillElementAttributes is the per-skill attribute schema for the plural list.
// It includes `provider` (legal as a nested attribute).
func skillElementAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id":         schema.StringAttribute{Computed: true, MarkdownDescription: "Unique skill identifier (UUID). Use this as `skill_ids` in `botyard_bot_skill_assignment`."},
		"slug":       schema.StringAttribute{Computed: true, MarkdownDescription: "URL-safe skill identifier."},
		"name":       schema.StringAttribute{Computed: true, MarkdownDescription: "Human-readable display name."},
		"summary":    schema.StringAttribute{Computed: true, MarkdownDescription: "Brief description of the skill."},
		"scope":      schema.StringAttribute{Computed: true, MarkdownDescription: "Visibility scope (e.g. `org`, `platform`)."},
		"provider":   schema.StringAttribute{Computed: true, MarkdownDescription: "Who authored/provided the skill."},
		"file_count": schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of files in the skill."},
		"created_at": schema.StringAttribute{Computed: true, MarkdownDescription: "Creation timestamp (RFC 3339)."},
		"updated_at": schema.StringAttribute{Computed: true, MarkdownDescription: "Last-update timestamp (RFC 3339)."},
	}
}

// listSkills walks the paginated /skills catalogue and returns every entry.
func listSkills(ctx context.Context, data *providerData, diags *diag.Diagnostics) ([]client.SkillSummaryResponse, bool) {
	var all []client.SkillSummaryResponse
	offset := 0
	for {
		limit := skillsPageSize
		off := offset
		params := &client.ListSkillsV1OrgsOrgIdSkillsGetParams{Limit: &limit, Offset: &off}
		apiResp, err := data.client.ListSkillsV1OrgsOrgIdSkillsGetWithResponse(ctx, data.orgID, params)
		if err != nil {
			diags.AddError("Error reading skills", fmt.Sprintf("Could not list skills: %s", err))
			return nil, false
		}
		if apiResp.JSON200 == nil {
			diags.AddError(
				"Unexpected response reading skills",
				fmt.Sprintf("Listing skills returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)),
			)
			return nil, false
		}
		page := apiResp.JSON200
		all = append(all, page.Items...)
		// Stop when the server reports no more pages, or defensively when a page
		// is empty (guards against a stuck has_more from advancing forever).
		if !page.HasMore || len(page.Items) == 0 {
			break
		}
		offset += len(page.Items)
	}
	return all, true
}

// --- Singular: botyard_skill ------------------------------------------------

// SkillDataSource resolves a single skill by slug — the "give me the id for this
// skill" case the assignment resources need.
type SkillDataSource struct {
	data *providerData
}

// NewSkillDataSource is the data-source factory registered with the provider.
func NewSkillDataSource() datasource.DataSource {
	return &SkillDataSource{}
}

func (d *SkillDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_skill"
}

func (d *SkillDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a single skill from the organization's skill catalogue by slug, " +
			"exposing its `id` for use with `botyard_bot_skill_assignment`. Per-skill `provider` is " +
			"available via the plural `botyard_skills` data source (`provider` is a reserved attribute " +
			"name at a data source's root).",
		Attributes: map[string]schema.Attribute{
			"slug":       schema.StringAttribute{Required: true, MarkdownDescription: "URL-safe slug of the skill to look up."},
			"id":         schema.StringAttribute{Computed: true, MarkdownDescription: "Unique skill identifier (UUID). Use this as `skill_ids` in `botyard_bot_skill_assignment`."},
			"name":       schema.StringAttribute{Computed: true, MarkdownDescription: "Human-readable display name."},
			"summary":    schema.StringAttribute{Computed: true, MarkdownDescription: "Brief description of the skill."},
			"scope":      schema.StringAttribute{Computed: true, MarkdownDescription: "Visibility scope (e.g. `org`, `platform`)."},
			"file_count": schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of files in the skill."},
			"created_at": schema.StringAttribute{Computed: true, MarkdownDescription: "Creation timestamp (RFC 3339)."},
			"updated_at": schema.StringAttribute{Computed: true, MarkdownDescription: "Last-update timestamp (RFC 3339)."},
		},
	}
}

func (d *SkillDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configureDataSourceProviderData(req, resp)
}

func (d *SkillDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg SkillDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	slug := cfg.Slug.ValueString()
	skills, ok := listSkills(ctx, d.data, &resp.Diagnostics)
	if !ok {
		return
	}
	for _, s := range skills {
		if s.Slug == slug {
			state := SkillDataSourceModel{
				Slug:      types.StringValue(s.Slug),
				ID:        types.StringValue(s.Id),
				Name:      types.StringValue(s.Name),
				Summary:   types.StringValue(s.Summary),
				Scope:     types.StringValue(string(s.Scope)),
				FileCount: types.Int64Value(int64(s.FileCount)),
				CreatedAt: types.StringValue(s.CreatedAt.Format(time.RFC3339)),
				UpdatedAt: types.StringValue(s.UpdatedAt.Format(time.RFC3339)),
			}
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	resp.Diagnostics.AddError(
		"Skill not found",
		fmt.Sprintf("No skill with slug %q was found in the organization's skill catalogue.", slug),
	)
}

// --- Plural: botyard_skills -------------------------------------------------

// SkillsDataSource lists every skill in the organization's skill catalogue.
type SkillsDataSource struct {
	data *providerData
}

// SkillsDataSourceModel is the top-level state for the plural botyard_skills list.
type SkillsDataSourceModel struct {
	Skills []SkillModel `tfsdk:"skills"`
}

// NewSkillsDataSource is the data-source factory registered with the provider.
func NewSkillsDataSource() datasource.DataSource {
	return &SkillsDataSource{}
}

func (d *SkillsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_skills"
}

func (d *SkillsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists all skills in the organization's skill catalogue.",
		Attributes: map[string]schema.Attribute{
			"skills": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "All skills in the organization's skill catalogue.",
				NestedObject:        schema.NestedAttributeObject{Attributes: skillElementAttributes()},
			},
		},
	}
}

func (d *SkillsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configureDataSourceProviderData(req, resp)
}

func (d *SkillsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	skills, ok := listSkills(ctx, d.data, &resp.Diagnostics)
	if !ok {
		return
	}
	state := SkillsDataSourceModel{Skills: make([]SkillModel, 0, len(skills))}
	for _, s := range skills {
		state.Skills = append(state.Skills, skillToModel(s))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
