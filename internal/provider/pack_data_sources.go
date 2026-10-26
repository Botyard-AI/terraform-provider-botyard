package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// Bot catalogue packs, read-only (PlatDev #4382, P1 of #4380).
//
// The catalogue API is collection-shaped and never 404s for a missing name:
//   - GET /packs?slugs=… lists unarchived packs with a published version, each
//     with only its LATEST version's summary, cursor-paginated in slug order.
//     Unknown slugs come back in `not_found` (computed against the whole
//     catalogue, so it is the same on every page).
//   - GET /packs/versions?pack_version_ids=… returns full version contents,
//     including versions of archived packs (flagged `pack_archived`). Unknown,
//     malformed and invisible ids are indistinguishable in `not_found`.
//
// There is no endpoint that lists a pack's older versions, so a version can be
// addressed by id, or as "the latest version of slug X", but not by
// (slug, version_no).

var (
	_ datasource.DataSource                     = (*PackDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*PackDataSource)(nil)
	_ datasource.DataSource                     = (*PacksDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*PacksDataSource)(nil)
	_ datasource.DataSource                     = (*PackVersionDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*PackVersionDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*PackVersionDataSource)(nil)
)

// packCataloguePageSize is the page size used when walking GET /packs. It is
// the API's maximum.
const packCataloguePageSize = 100

// packCatalogueSlugsMax is the API's bound on the `slugs` filter.
const packCatalogueSlugsMax = 100

// packCatalogueMaxPages stops a misbehaving server (a cursor that never ends)
// from looping forever. 1000 pages of 100 is far beyond any real catalogue.
const packCatalogueMaxPages = 1000

// --- Shared: catalogue entry ------------------------------------------------

// PackVersionSummaryModel mirrors PackVersionSummary.
type PackVersionSummaryModel struct {
	PackVersionID types.String `tfsdk:"pack_version_id"`
	VersionNo     types.Int64  `tfsdk:"version_no"`
	Name          types.String `tfsdk:"name"`
	Summary       types.String `tfsdk:"summary"`
	AvatarSha256  types.String `tfsdk:"avatar_sha256"`
	ContentHash   types.String `tfsdk:"content_hash"`
	CreatedAt     types.String `tfsdk:"created_at"`
}

// CataloguePackModel is one entry of the plural botyard_packs list.
type CataloguePackModel struct {
	PackID         types.String            `tfsdk:"pack_id"`
	Slug           types.String            `tfsdk:"slug"`
	Visibility     types.String            `tfsdk:"visibility"`
	PublisherOrgID types.String            `tfsdk:"publisher_org_id"`
	Latest         PackVersionSummaryModel `tfsdk:"latest"`
}

func packVersionSummaryToModel(v client.PackVersionSummary) PackVersionSummaryModel {
	return PackVersionSummaryModel{
		PackVersionID: types.StringValue(v.PackVersionId),
		VersionNo:     types.Int64Value(int64(v.VersionNo)),
		Name:          types.StringValue(v.Name),
		Summary:       types.StringValue(v.Summary),
		AvatarSha256:  types.StringValue(v.AvatarSha256),
		ContentHash:   types.StringValue(v.ContentHash),
		CreatedAt:     types.StringValue(v.CreatedAt.Format(time.RFC3339)),
	}
}

func cataloguePackToModel(p client.CataloguePack) CataloguePackModel {
	return CataloguePackModel{
		PackID:         types.StringValue(p.PackId),
		Slug:           types.StringValue(p.Slug),
		Visibility:     types.StringValue(string(p.Visibility)),
		PublisherOrgID: types.StringPointerValue(p.PublisherOrgId),
		Latest:         packVersionSummaryToModel(p.Latest),
	}
}

func packVersionSummaryAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"pack_version_id": schema.StringAttribute{Computed: true, MarkdownDescription: "Id of the pack's latest published version. Pass it to `botyard_pack_version`."},
		"version_no":      schema.Int64Attribute{Computed: true, MarkdownDescription: "Version number, counting from 1."},
		"name":            schema.StringAttribute{Computed: true, MarkdownDescription: "Display name of this version."},
		"summary":         schema.StringAttribute{Computed: true, MarkdownDescription: "One-line summary of this version."},
		"avatar_sha256":   schema.StringAttribute{Computed: true, MarkdownDescription: "SHA-256 of the version's avatar image."},
		"content_hash":    schema.StringAttribute{Computed: true, MarkdownDescription: "Hash of the version's contents. Changes whenever the contents do."},
		"created_at":      schema.StringAttribute{Computed: true, MarkdownDescription: "When the version was published (RFC 3339)."},
	}
}

func cataloguePackIdentityAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"pack_id":          schema.StringAttribute{Computed: true, MarkdownDescription: "Unique pack identifier (UUID)."},
		"visibility":       schema.StringAttribute{Computed: true, MarkdownDescription: "Who may see the pack: `curated`, `org_private` or `public`."},
		"publisher_org_id": schema.StringAttribute{Computed: true, MarkdownDescription: "Organization that publishes the pack. Null for platform-curated packs."},
		"latest": schema.SingleNestedAttribute{
			Computed:            true,
			MarkdownDescription: "Summary of the pack's latest published version. The catalogue lists only the latest version of each pack.",
			Attributes:          packVersionSummaryAttributes(),
		},
	}
}

// listPacks walks GET /packs. With slugs non-nil it filters to those slugs and
// also returns `not_found`. It fails on a cursor that repeats, rather than
// looping.
func listPacks(ctx context.Context, data *providerData, slugs []string, diags *diag.Diagnostics) ([]client.CataloguePack, []string, bool) {
	var (
		all      []client.CataloguePack
		notFound []string
		cursor   *string
		seen     = map[string]bool{}
	)
	for page := 0; ; page++ {
		if page >= packCatalogueMaxPages {
			diags.AddError("Error reading packs", fmt.Sprintf("Listing packs did not finish after %d pages.", packCatalogueMaxPages))
			return nil, nil, false
		}
		limit := packCataloguePageSize
		params := &client.ListPacksV1OrgsOrgIdPacksGetParams{Limit: &limit, Cursor: cursor}
		if slugs != nil {
			s := slugs
			params.Slugs = &s
		}
		apiResp, err := data.client.ListPacksV1OrgsOrgIdPacksGetWithResponse(ctx, data.orgID, params)
		if err != nil {
			diags.AddError("Error reading packs", fmt.Sprintf("Could not list packs: %s", err))
			return nil, nil, false
		}
		if apiResp.JSON200 == nil {
			diags.AddError(
				"Unexpected response reading packs",
				fmt.Sprintf("Listing packs returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)),
			)
			return nil, nil, false
		}
		body := apiResp.JSON200
		all = append(all, body.Packs...)
		if page == 0 {
			// Identical on every page; take it once.
			notFound = body.NotFound
		}
		if body.NextCursor == nil {
			break
		}
		next := *body.NextCursor
		if seen[next] || len(body.Packs) == 0 {
			diags.AddError("Error reading packs", fmt.Sprintf("Listing packs returned a cursor that does not advance (%q).", next))
			return nil, nil, false
		}
		seen[next] = true
		cursor = &next
	}
	if notFound == nil {
		notFound = []string{}
	}
	return all, notFound, true
}

// lookupPackBySlug resolves one catalogue pack, reporting not-found as an
// error diagnostic on the given attribute.
func lookupPackBySlug(ctx context.Context, data *providerData, slug string, attr path.Path, diags *diag.Diagnostics) (client.CataloguePack, bool) {
	packs, _, ok := listPacks(ctx, data, []string{slug}, diags)
	if !ok {
		return client.CataloguePack{}, false
	}
	for _, p := range packs {
		if p.Slug == slug {
			return p, true
		}
	}
	diags.AddAttributeError(attr, "Pack not found", fmt.Sprintf(
		"No pack with slug %q is in the organization's pack catalogue. The catalogue lists only packs this "+
			"organization may see that have a published version and are not archived. An archived pack's "+
			"versions can still be read by id with `botyard_pack_version`.", slug))
	return client.CataloguePack{}, false
}

// --- Singular: botyard_pack -------------------------------------------------

// PackDataSource looks up one catalogue pack by slug.
type PackDataSource struct {
	data *providerData
}

// PackDataSourceModel is the botyard_pack state.
type PackDataSourceModel struct {
	Slug           types.String            `tfsdk:"slug"`
	PackID         types.String            `tfsdk:"pack_id"`
	Visibility     types.String            `tfsdk:"visibility"`
	PublisherOrgID types.String            `tfsdk:"publisher_org_id"`
	Latest         PackVersionSummaryModel `tfsdk:"latest"`
}

// NewPackDataSource is the data-source factory registered with the provider.
func NewPackDataSource() datasource.DataSource { return &PackDataSource{} }

func (d *PackDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_pack"
}

func (d *PackDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := cataloguePackIdentityAttributes()
	attrs["slug"] = schema.StringAttribute{Required: true, MarkdownDescription: "Slug of the pack to look up."}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up one pack in the organization's bot catalogue by slug and returns a summary of " +
			"its latest published version. Read the version's contents (skills, tools, connections, settings) " +
			"with `botyard_pack_version`.\n\n" +
			"Archived packs, and packs with no published version, are not in the catalogue and are reported as not found.",
		Attributes: attrs,
	}
}

func (d *PackDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configureDataSourceProviderData(req, resp)
}

func (d *PackDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	// Read only the input: the computed nested `latest` is null in config.
	var slug types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("slug"), &slug)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, ok := lookupPackBySlug(ctx, d.data, slug.ValueString(), path.Root("slug"), &resp.Diagnostics)
	if !ok {
		return
	}
	m := cataloguePackToModel(p)
	state := PackDataSourceModel{
		Slug:           m.Slug,
		PackID:         m.PackID,
		Visibility:     m.Visibility,
		PublisherOrgID: m.PublisherOrgID,
		Latest:         m.Latest,
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// --- Plural: botyard_packs --------------------------------------------------

// PacksDataSource lists the organization's pack catalogue.
type PacksDataSource struct {
	data *providerData
}

// PacksDataSourceModel is the botyard_packs state.
type PacksDataSourceModel struct {
	Slugs    types.Set            `tfsdk:"slugs"`
	Packs    []CataloguePackModel `tfsdk:"packs"`
	NotFound []string             `tfsdk:"not_found"`
}

// NewPacksDataSource is the data-source factory registered with the provider.
func NewPacksDataSource() datasource.DataSource { return &PacksDataSource{} }

func (d *PacksDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_packs"
}

func (d *PacksDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	elem := cataloguePackIdentityAttributes()
	elem["slug"] = schema.StringAttribute{Computed: true, MarkdownDescription: "URL-safe pack identifier."}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the organization's bot catalogue: curated and public packs, plus the organization's " +
			"own private packs. Each entry carries a summary of the pack's latest published version. Archived packs " +
			"and packs with no published version are not listed.",
		Attributes: map[string]schema.Attribute{
			"slugs": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Validators: []validator.Set{
					setvalidator.SizeBetween(1, packCatalogueSlugsMax),
				},
				MarkdownDescription: fmt.Sprintf("Only list packs with these slugs (1 to %d). Omit to list the whole catalogue. "+
					"Slugs that match no listed pack are reported in `not_found` rather than as an error.", packCatalogueSlugsMax),
			},
			"packs": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The listed packs, in slug order.",
				NestedObject:        schema.NestedAttributeObject{Attributes: elem},
			},
			"not_found": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Requested `slugs` that match no listed pack. Always empty when `slugs` is omitted.",
			},
		},
	}
}

func (d *PacksDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configureDataSourceProviderData(req, resp)
}

func (d *PacksDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg PacksDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var slugs []string
	if !cfg.Slugs.IsNull() {
		resp.Diagnostics.Append(cfg.Slugs.ElementsAs(ctx, &slugs, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		// A set has no order; send a stable one so requests are reproducible.
		sort.Strings(slugs)
	}
	packs, notFound, ok := listPacks(ctx, d.data, slugs, &resp.Diagnostics)
	if !ok {
		return
	}
	state := PacksDataSourceModel{
		Slugs:    cfg.Slugs,
		Packs:    make([]CataloguePackModel, 0, len(packs)),
		NotFound: notFound,
	}
	for _, p := range packs {
		state.Packs = append(state.Packs, cataloguePackToModel(p))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// --- botyard_pack_version ---------------------------------------------------

// PackVersionDataSource reads one pack version's full contents.
type PackVersionDataSource struct {
	data *providerData
}

// PackSkillModel is one skill a pack version pins.
type PackSkillModel struct {
	SkillID        types.String `tfsdk:"skill_id"`
	SkillSlug      types.String `tfsdk:"skill_slug"`
	SkillName      types.String `tfsdk:"skill_name"`
	SkillVersionID types.String `tfsdk:"skill_version_id"`
	VersionNo      types.Int64  `tfsdk:"version_no"`
	VersionName    types.String `tfsdk:"version_name"`
	Required       types.Bool   `tfsdk:"required"`
}

// PackToolModel is one tool in a pack version. The API models a tool as a
// union on `runtime`; Terraform has no union type, so the variant-specific
// field (`mcp_server`, MCP tools only) is null on built-in tools.
type PackToolModel struct {
	ToolID    types.String `tfsdk:"tool_id"`
	Slug      types.String `tfsdk:"slug"`
	Name      types.String `tfsdk:"name"`
	Title     types.String `tfsdk:"title"`
	Runtime   types.String `tfsdk:"runtime"`
	McpServer types.String `tfsdk:"mcp_server"`
	Required  types.Bool   `tfsdk:"required"`
}

// PackMcpCatalogEntryModel is the variant body of an `mcp_catalog_entry`
// connection.
type PackMcpCatalogEntryModel struct {
	McpCatalogEntryID types.String `tfsdk:"mcp_catalog_entry_id"`
	Slug              types.String `tfsdk:"slug"`
	Name              types.String `tfsdk:"name"`
	ToolNames         types.List   `tfsdk:"tool_names"`
}

// PackVaultSecretModel is the variant body of a `vault_secret` connection.
type PackVaultSecretModel struct {
	KeyPath       types.String `tfsdk:"key_path"`
	DisplayName   types.String `tfsdk:"display_name"`
	Description   types.String `tfsdk:"description"`
	Sensitivity   types.String `tfsdk:"sensitivity"`
	MaxTTLSeconds types.Int64  `tfsdk:"max_ttl_seconds"`
}

// PackConnectionModel is one connection a pack version asks for. Exactly one
// of the variant blocks (`mcp_catalog_entry`, `vault_secret`) is set, the one
// named by `kind`. This is Terraform's usual shape for a union.
type PackConnectionModel struct {
	Kind            types.String              `tfsdk:"kind"`
	Required        types.Bool                `tfsdk:"required"`
	Purpose         types.String              `tfsdk:"purpose"`
	McpCatalogEntry *PackMcpCatalogEntryModel `tfsdk:"mcp_catalog_entry"`
	VaultSecret     *PackVaultSecretModel     `tfsdk:"vault_secret"`
}

// PackModelRefModel mirrors ModelRef.
type PackModelRefModel struct {
	Provider types.String `tfsdk:"provider"`
	Model    types.String `tfsdk:"model"`
}

// PackToolSearchModel mirrors PackToolSearchSettings.
type PackToolSearchModel struct {
	Mode                 types.String `tfsdk:"mode"`
	AutoThresholdPercent types.Int64  `tfsdk:"auto_threshold_percent"`
}

// PackToolImagesModel mirrors PackToolImageSettings.
type PackToolImagesModel struct {
	Enabled        types.Bool   `tfsdk:"enabled"`
	MaxDimensionPx types.Int64  `tfsdk:"max_dimension_px"`
	DefaultDetail  types.String `tfsdk:"default_detail"`
}

// PackSettingsModel mirrors PackSettings. Null means "the pack does not set it".
type PackSettingsModel struct {
	ThinkingDefault  types.String         `tfsdk:"thinking_default"`
	ReasoningDefault types.String         `tfsdk:"reasoning_default"`
	ToolSearch       *PackToolSearchModel `tfsdk:"tool_search"`
	ToolImages       *PackToolImagesModel `tfsdk:"tool_images"`
}

// PackVersionDataSourceModel is the botyard_pack_version state.
type PackVersionDataSourceModel struct {
	PackVersionID         types.String          `tfsdk:"pack_version_id"`
	Slug                  types.String          `tfsdk:"slug"`
	PackID                types.String          `tfsdk:"pack_id"`
	Visibility            types.String          `tfsdk:"visibility"`
	PublisherOrgID        types.String          `tfsdk:"publisher_org_id"`
	PackArchived          types.Bool            `tfsdk:"pack_archived"`
	IsLatest              types.Bool            `tfsdk:"is_latest"`
	VersionNo             types.Int64           `tfsdk:"version_no"`
	Name                  types.String          `tfsdk:"name"`
	Summary               types.String          `tfsdk:"summary"`
	DescriptionMd         types.String          `tfsdk:"description_md"`
	Soul                  types.String          `tfsdk:"soul"`
	Changelog             types.String          `tfsdk:"changelog"`
	AvatarSha256          types.String          `tfsdk:"avatar_sha256"`
	AvatarURL             types.String          `tfsdk:"avatar_url"`
	ContentHash           types.String          `tfsdk:"content_hash"`
	CreatedAt             types.String          `tfsdk:"created_at"`
	StartWithDefaultTools types.Bool            `tfsdk:"start_with_default_tools"`
	RecommendedModel      *PackModelRefModel    `tfsdk:"recommended_model"`
	Settings              PackSettingsModel     `tfsdk:"settings"`
	Skills                []PackSkillModel      `tfsdk:"skills"`
	Tools                 []PackToolModel       `tfsdk:"tools"`
	Connections           []PackConnectionModel `tfsdk:"connections"`
	SkillIDs              []string              `tfsdk:"skill_ids"`
	ToolIDs               []string              `tfsdk:"tool_ids"`
}

// NewPackVersionDataSource is the data-source factory registered with the provider.
func NewPackVersionDataSource() datasource.DataSource { return &PackVersionDataSource{} }

func (d *PackVersionDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_pack_version"
}

func (d *PackVersionDataSource) ConfigValidators(_ context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("pack_version_id"), path.MatchRoot("slug")),
	}
}

func (d *PackVersionDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	boolean := func(desc string) schema.BoolAttribute {
		return schema.BoolAttribute{Computed: true, MarkdownDescription: desc}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads the full contents of one bot-pack version: its skills (with pinned skill versions), " +
			"tools, connections, settings and soul.\n\n" +
			"Address the version by `pack_version_id`, or by `slug` for the pack's latest published version. " +
			"The API cannot look up an older version by number, so pin an older version by its id.\n\n" +
			"Versions of archived packs stay readable by id and have `pack_archived = true`.\n\n" +
			"`skill_ids` and `tool_ids` list every skill and tool the version names, required or not. " +
			"Use them with `setunion` in `botyard_bot_skill_assignment` and `botyard_bot_tool_assignment`, " +
			"which are exclusive and would otherwise remove pack items from a bot.",
		Attributes: map[string]schema.Attribute{
			"pack_version_id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Id (UUID) of the version to read. Exactly one of `pack_version_id` and `slug` is required.",
			},
			"slug": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Slug of a catalogue pack whose latest published version to read. Exactly one of `pack_version_id` and `slug` is required.",
			},
			"pack_id":                  str("Id of the pack this version belongs to."),
			"visibility":               str("Who may see the pack: `curated`, `org_private` or `public`."),
			"publisher_org_id":         str("Organization that publishes the pack. Null for platform-curated packs."),
			"pack_archived":            boolean("Whether the pack is archived. Its versions stay readable and installed bots keep working."),
			"is_latest":                boolean("Whether this is the pack's latest published version."),
			"version_no":               schema.Int64Attribute{Computed: true, MarkdownDescription: "Version number, counting from 1."},
			"name":                     str("Display name of this version."),
			"summary":                  str("One-line summary of this version."),
			"description_md":           str("Long description, in Markdown."),
			"soul":                     str("The soul (system instructions) the pack gives a bot."),
			"changelog":                str("What changed in this version. Null when none was given."),
			"avatar_sha256":            str("SHA-256 of the version's avatar image."),
			"avatar_url":               str("URL of the version's avatar image."),
			"content_hash":             str("Hash of the version's contents."),
			"created_at":               str("When the version was published (RFC 3339)."),
			"start_with_default_tools": boolean("Whether a bot created from this version also gets the platform's suggested default tools. No effect when installing onto an existing bot."),
			"recommended_model": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The model the pack recommends. Null when it recommends none.",
				Attributes: map[string]schema.Attribute{
					"provider": str("Model provider key (e.g. `botyard`)."),
					"model":    str("Model id within the provider."),
				},
			},
			"settings": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Bot settings the pack applies. A null field means the pack does not set it.",
				Attributes: map[string]schema.Attribute{
					"thinking_default":  str("Default thinking level."),
					"reasoning_default": str("Default reasoning mode (`off`, `on` or `stream`)."),
					"tool_search": schema.SingleNestedAttribute{
						Computed:            true,
						MarkdownDescription: "Tool Search settings. Null when the pack does not set them.",
						Attributes: map[string]schema.Attribute{
							"mode":                   str("`always`, `auto` or `off`."),
							"auto_threshold_percent": schema.Int64Attribute{Computed: true, MarkdownDescription: "Context-window percentage above which `auto` defers tool schemas."},
						},
					},
					"tool_images": schema.SingleNestedAttribute{
						Computed:            true,
						MarkdownDescription: "Tool-image settings. Null when the pack does not set them.",
						Attributes: map[string]schema.Attribute{
							"enabled":          boolean("Whether tool images are shown to the model."),
							"max_dimension_px": schema.Int64Attribute{Computed: true, MarkdownDescription: "Longest edge tool images are downscaled to."},
							"default_detail":   str("Default image detail level."),
						},
					},
				},
			},
			"skills": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Skills the version includes, each pinned to a skill version.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"skill_id":         str("Skill id. Use with `botyard_bot_skill_assignment`."),
					"skill_slug":       str("Skill slug."),
					"skill_name":       str("Skill name."),
					"skill_version_id": str("Id of the pinned skill version."),
					"version_no":       schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of the pinned skill version."},
					"version_name":     str("Name of the pinned skill version."),
					"required":         boolean("Whether the pack needs this skill."),
				}},
			},
			"tools": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Tools the version includes.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"tool_id":    str("Tool id. Use with `botyard_bot_tool_assignment`."),
					"slug":       str("Tool slug."),
					"name":       str("Tool name."),
					"title":      str("Tool title. Null when the tool has none."),
					"runtime":    str("Where the tool runs: `mcp` (served over MCP), or `openclaw` / `native` (built into the bot's runtime)."),
					"mcp_server": str("MCP server the tool comes from, as the tool catalogue names it. Set only when `runtime` is `mcp`."),
					"required":   boolean("Whether the pack needs this tool."),
				}},
			},
			"connections": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "Connections the version asks the installer for. `kind` says which one of " +
					"`mcp_catalog_entry` and `vault_secret` is set.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"kind":     str("`mcp_catalog_entry` or `vault_secret`."),
					"required": boolean("Whether the pack needs this connection."),
					"purpose":  str("What the connection is for. Null when not given."),
					"mcp_catalog_entry": schema.SingleNestedAttribute{
						Computed:            true,
						MarkdownDescription: "The MCP catalogue entry to connect. Set only when `kind` is `mcp_catalog_entry`.",
						Attributes: map[string]schema.Attribute{
							"mcp_catalog_entry_id": str("MCP catalogue entry id."),
							"slug":                 str("Catalogue entry slug."),
							"name":                 str("Catalogue entry name."),
							"tool_names": schema.ListAttribute{
								Computed:            true,
								ElementType:         types.StringType,
								MarkdownDescription: "Tools of the server the pack uses. Null means all of them.",
							},
						},
					},
					"vault_secret": schema.SingleNestedAttribute{
						Computed:            true,
						MarkdownDescription: "The Runtime Vault secret the installer supplies. Set only when `kind` is `vault_secret`. Never contains a value.",
						Attributes: map[string]schema.Attribute{
							"key_path":        str("Vault key path the secret is stored at."),
							"display_name":    str("Human-readable name."),
							"description":     str("What the secret is for. Null when not given."),
							"sensitivity":     str("`secret` or `plain`."),
							"max_ttl_seconds": schema.Int64Attribute{Computed: true, MarkdownDescription: "Maximum lease TTL bots may request."},
						},
					},
				}},
			},
			"skill_ids": schema.SetAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Ids of every skill in `skills`.",
			},
			"tool_ids": schema.SetAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Ids of every tool in `tools`.",
			},
		},
	}
}

func (d *PackVersionDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configureDataSourceProviderData(req, resp)
}

func (d *PackVersionDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	// Read only the two inputs: computed nested objects are null in config.
	var cfg struct{ PackVersionID, Slug types.String }
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("pack_version_id"), &cfg.PackVersionID)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("slug"), &cfg.Slug)...)
	if resp.Diagnostics.HasError() {
		return
	}

	versionID := cfg.PackVersionID.ValueString()
	attr := path.Root("pack_version_id")
	if !cfg.Slug.IsNull() {
		p, ok := lookupPackBySlug(ctx, d.data, cfg.Slug.ValueString(), path.Root("slug"), &resp.Diagnostics)
		if !ok {
			return
		}
		versionID = p.Latest.PackVersionId
		attr = path.Root("slug")
	}

	var id openapi_types.UUID
	if err := id.UnmarshalText([]byte(versionID)); err != nil {
		resp.Diagnostics.AddAttributeError(attr, "Invalid pack version id",
			fmt.Sprintf("%q is not a UUID: %s", versionID, err))
		return
	}
	apiResp, err := d.data.client.GetPackVersionsV1OrgsOrgIdPacksVersionsGetWithResponse(ctx, d.data.orgID,
		&client.GetPackVersionsV1OrgsOrgIdPacksVersionsGetParams{PackVersionIds: []openapi_types.UUID{id}})
	if err != nil {
		resp.Diagnostics.AddError("Error reading pack version", fmt.Sprintf("Could not read pack version %s: %s", versionID, err))
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError("Unexpected response reading pack version", fmt.Sprintf(
			"Reading pack version %s returned HTTP %d: %s", versionID, apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	var found *client.OrgPackVersion
	for i := range apiResp.JSON200.Versions {
		if apiResp.JSON200.Versions[i].PackVersionId == id.String() {
			found = &apiResp.JSON200.Versions[i]
			break
		}
	}
	if found == nil {
		resp.Diagnostics.AddAttributeError(attr, "Pack version not found", fmt.Sprintf(
			"Pack version %s does not exist or belongs to a pack this organization cannot see.", versionID))
		return
	}

	state, diags := packVersionToModel(*found)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Keep the configured address verbatim (the API returns a canonical,
	// lower-case UUID); the other attribute is computed.
	if !cfg.Slug.IsNull() {
		state.Slug = cfg.Slug
	} else {
		state.PackVersionID = cfg.PackVersionID
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// packVersionToModel maps an OrgPackVersion into state. It fails, rather than
// dropping the item, on a tool or connection variant this provider does not
// know.
func packVersionToModel(v client.OrgPackVersion) (PackVersionDataSourceModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	m := PackVersionDataSourceModel{
		PackVersionID:         types.StringValue(v.PackVersionId),
		Slug:                  types.StringValue(v.PackSlug),
		PackID:                types.StringValue(v.PackId),
		Visibility:            types.StringValue(string(v.Visibility)),
		PublisherOrgID:        types.StringPointerValue(v.PublisherOrgId),
		PackArchived:          types.BoolValue(v.PackArchived),
		IsLatest:              types.BoolValue(v.IsLatest),
		VersionNo:             types.Int64Value(int64(v.VersionNo)),
		Name:                  types.StringValue(v.Name),
		Summary:               types.StringValue(v.Summary),
		DescriptionMd:         types.StringValue(v.DescriptionMd),
		Soul:                  types.StringValue(v.Soul),
		Changelog:             types.StringPointerValue(v.Changelog),
		AvatarSha256:          types.StringValue(v.AvatarSha256),
		AvatarURL:             types.StringValue(v.AvatarUrl),
		ContentHash:           types.StringValue(v.ContentHash),
		CreatedAt:             types.StringValue(v.CreatedAt.Format(time.RFC3339)),
		StartWithDefaultTools: types.BoolValue(v.StartWithDefaultTools),
		Settings:              packSettingsToModel(v.Settings),
		Skills:                make([]PackSkillModel, 0, len(v.Skills)),
		Tools:                 make([]PackToolModel, 0, len(v.Tools)),
		Connections:           make([]PackConnectionModel, 0, len(v.Connections)),
		SkillIDs:              make([]string, 0, len(v.Skills)),
		ToolIDs:               make([]string, 0, len(v.Tools)),
	}
	if v.RecommendedModel != nil {
		m.RecommendedModel = &PackModelRefModel{
			Provider: types.StringPointerValue(v.RecommendedModel.Provider),
			Model:    types.StringValue(v.RecommendedModel.Model),
		}
	}
	for _, s := range v.Skills {
		m.Skills = append(m.Skills, PackSkillModel{
			SkillID:        types.StringValue(s.Skill.SkillId),
			SkillSlug:      types.StringValue(s.Skill.SkillSlug),
			SkillName:      types.StringValue(s.Skill.SkillName),
			SkillVersionID: types.StringValue(s.Skill.SkillVersionId),
			VersionNo:      types.Int64Value(int64(s.Skill.VersionNo)),
			VersionName:    types.StringValue(s.Skill.VersionName),
			Required:       types.BoolValue(s.Required),
		})
		m.SkillIDs = append(m.SkillIDs, s.Skill.SkillId)
	}
	for i, t := range v.Tools {
		tm, err := packToolToModel(t)
		if err != nil {
			diags.AddError("Unsupported pack tool", unknownVariantDetail(v, fmt.Sprintf("tools[%d]", i), err))
			continue
		}
		m.Tools = append(m.Tools, tm)
		m.ToolIDs = append(m.ToolIDs, tm.ToolID.ValueString())
	}
	for i, c := range v.Connections {
		cm, err := packConnectionToModel(c)
		if err != nil {
			diags.AddError("Unsupported pack connection", unknownVariantDetail(v, fmt.Sprintf("connections[%d]", i), err))
			continue
		}
		m.Connections = append(m.Connections, cm)
	}
	return m, diags
}

func unknownVariantDetail(v client.OrgPackVersion, where string, err error) string {
	msg := fmt.Sprintf("Pack %q version %d (%s), %s: %s.", v.PackSlug, v.VersionNo, v.PackVersionId, where, err)
	var unknown *client.UnknownDiscriminatorError
	if errors.As(err, &unknown) {
		msg += " The API returned a kind of pack item this provider version does not support. " +
			"Upgrade the Botyard provider. Refusing to read the version rather than leave the item out."
	}
	return msg
}

func packToolToModel(t client.NamedPackTool) (PackToolModel, error) {
	tool, err := client.DecodePackTool(t.Tool)
	if err != nil {
		return PackToolModel{}, err
	}
	switch {
	case tool.Mcp != nil:
		return PackToolModel{
			ToolID:    types.StringValue(tool.Mcp.ToolId),
			Slug:      types.StringValue(tool.Mcp.Slug),
			Name:      types.StringValue(tool.Mcp.Name),
			Title:     types.StringPointerValue(tool.Mcp.Title),
			Runtime:   types.StringValue(tool.Runtime),
			McpServer: types.StringValue(tool.Mcp.McpServer),
			Required:  types.BoolValue(t.Required),
		}, nil
	case tool.Builtin != nil:
		return PackToolModel{
			ToolID:    types.StringValue(tool.Builtin.ToolId),
			Slug:      types.StringValue(tool.Builtin.Slug),
			Name:      types.StringValue(tool.Builtin.Name),
			Title:     types.StringPointerValue(tool.Builtin.Title),
			Runtime:   types.StringValue(tool.Runtime),
			McpServer: types.StringNull(),
			Required:  types.BoolValue(t.Required),
		}, nil
	default:
		return PackToolModel{}, fmt.Errorf("decoded tool has no variant (runtime %q)", tool.Runtime)
	}
}

func packConnectionToModel(c client.OrgPackVersion_Connections_Item) (PackConnectionModel, error) {
	conn, err := client.DecodePackConnection(c)
	if err != nil {
		return PackConnectionModel{}, err
	}
	switch {
	case conn.Mcp != nil:
		toolNames := types.ListNull(types.StringType)
		if conn.Mcp.ToolNames != nil {
			elems := make([]attr.Value, 0, len(*conn.Mcp.ToolNames))
			for _, n := range *conn.Mcp.ToolNames {
				elems = append(elems, types.StringValue(n))
			}
			toolNames = types.ListValueMust(types.StringType, elems)
		}
		e := conn.Mcp.McpCatalogEntry
		return PackConnectionModel{
			Kind:     types.StringValue(conn.Kind),
			Required: types.BoolValue(conn.Mcp.Required),
			Purpose:  types.StringPointerValue(conn.Mcp.Purpose),
			McpCatalogEntry: &PackMcpCatalogEntryModel{
				McpCatalogEntryID: types.StringValue(e.McpCatalogEntryId),
				Slug:              types.StringValue(e.Slug),
				Name:              types.StringValue(e.Name),
				ToolNames:         toolNames,
			},
		}, nil
	case conn.Vault != nil:
		s := conn.Vault.SecretSpec
		sensitivity := types.StringNull()
		if s.Sensitivity != nil {
			sensitivity = types.StringValue(string(*s.Sensitivity))
		}
		return PackConnectionModel{
			Kind:     types.StringValue(conn.Kind),
			Required: types.BoolValue(conn.Vault.Required),
			Purpose:  types.StringPointerValue(conn.Vault.Purpose),
			VaultSecret: &PackVaultSecretModel{
				KeyPath:       types.StringValue(s.KeyPath),
				DisplayName:   types.StringValue(s.DisplayName),
				Description:   types.StringPointerValue(s.Description),
				Sensitivity:   sensitivity,
				MaxTTLSeconds: types.Int64Value(int64(s.MaxTtlSeconds)),
			},
		}, nil
	default:
		return PackConnectionModel{}, fmt.Errorf("decoded connection has no variant (kind %q)", conn.Kind)
	}
}

func packSettingsToModel(s client.PackSettings) PackSettingsModel {
	m := PackSettingsModel{ThinkingDefault: types.StringNull(), ReasoningDefault: types.StringNull()}
	if s.ThinkingDefault != nil {
		m.ThinkingDefault = types.StringValue(string(*s.ThinkingDefault))
	}
	if s.ReasoningDefault != nil {
		m.ReasoningDefault = types.StringValue(string(*s.ReasoningDefault))
	}
	if ts := s.ToolSearch; ts != nil {
		mode := types.StringNull()
		if ts.Mode != nil {
			mode = types.StringValue(string(*ts.Mode))
		}
		m.ToolSearch = &PackToolSearchModel{Mode: mode, AutoThresholdPercent: int64PointerValue(ts.AutoThresholdPercent)}
	}
	if ti := s.ToolImages; ti != nil {
		detail := types.StringNull()
		if ti.DefaultDetail != nil {
			detail = types.StringValue(string(*ti.DefaultDetail))
		}
		m.ToolImages = &PackToolImagesModel{
			Enabled:        types.BoolPointerValue(ti.Enabled),
			MaxDimensionPx: int64PointerValue(ti.MaxDimensionPx),
			DefaultDetail:  detail,
		}
	}
	return m
}

func int64PointerValue(v *int) types.Int64 {
	if v == nil {
		return types.Int64Null()
	}
	return types.Int64Value(int64(*v))
}
