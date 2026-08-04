package provider

import (
	"bytes"
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ datasource.DataSource              = (*BotTemplateDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*BotTemplateDataSource)(nil)
	_ datasource.DataSource              = (*BotTemplatesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*BotTemplatesDataSource)(nil)
)

// BotTemplateModel maps one bot template into Terraform state. It is shared by
// the singular botyard_bot_template (looked up by slug) and the plural
// botyard_bot_templates data sources.
//
// The primary value of a template is its default `tool_ids` + `skill_ids`
// bundle. Visible catalog templates such as "coding-agent" can be wired
// explicitly into the assignment resources, e.g.:
//
//	data "botyard_bot_template" "defaults" { slug = "coding-agent" }
//	resource "botyard_bot_tool_assignment" "x" {
//	  bot_slug = botyard_bot.x.slug
//	  tool_ids = data.botyard_bot_template.defaults.tool_ids
//	}
type BotTemplateModel struct {
	ID                  types.String      `tfsdk:"id"`
	Slug                types.String      `tfsdk:"slug"`
	Name                types.String      `tfsdk:"name"`
	Description         types.String      `tfsdk:"description"`
	Icon                types.String      `tfsdk:"icon"`
	SupportsGuidedSetup types.Bool        `tfsdk:"supports_guided_setup"`
	ToolIDs             []string          `tfsdk:"tool_ids"`
	SkillIDs            []string          `tfsdk:"skill_ids"`
	Files               map[string]string `tfsdk:"files"`
	ConfigJSON          types.String      `tfsdk:"config_json"`
}

// botTemplateToModel maps an API BotTemplateResponse into the shared model. The
// large OpenClawConfigPatch default is surfaced losslessly as a JSON string
// (config_json) rather than a re-modeled nested block: a read-only data source
// does not need to re-declare the entire config schema, and callers who need it
// can jsondecode() the string.
func botTemplateToModel(t client.BotTemplateWithRawConfig) BotTemplateModel {
	template := t.Template
	m := BotTemplateModel{
		ID:                  types.StringValue(template.Id),
		Slug:                types.StringValue(template.Slug),
		Name:                types.StringValue(template.Name),
		Description:         types.StringValue(template.Description),
		Icon:                types.StringValue(string(template.Icon)),
		SupportsGuidedSetup: boolPtrToBool(template.SupportsGuidedSetup),
		ToolIDs:             template.ToolIds,
		SkillIDs:            template.SkillIds,
		Files:               template.Files,
		ConfigJSON:          types.StringNull(),
	}
	if raw := bytes.TrimSpace(t.RawConfig); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		m.ConfigJSON = types.StringValue(string(t.RawConfig))
	}
	return m
}

// botTemplateComputedAttributes is the per-template attribute schema shared by
// the singular and plural data sources. Every attribute is Computed; the
// singular data source overrides `slug` to Required.
func botTemplateComputedAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id":                    schema.StringAttribute{Computed: true, MarkdownDescription: "Unique template identifier (UUID)."},
		"slug":                  schema.StringAttribute{Computed: true, MarkdownDescription: "URL-safe visible template identifier (e.g. `coding-agent`, `personal-assistant`). Hidden/internal templates are not exposed by the catalog."},
		"name":                  schema.StringAttribute{Computed: true, MarkdownDescription: "Display name shown in the wizard."},
		"description":           schema.StringAttribute{Computed: true, MarkdownDescription: "Short description for the template card."},
		"icon":                  schema.StringAttribute{Computed: true, MarkdownDescription: "Icon name for the template."},
		"supports_guided_setup": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether this template offers the in-chat guided setup option."},
		"tool_ids": schema.ListAttribute{
			Computed:            true,
			ElementType:         types.StringType,
			MarkdownDescription: "IDs of tools this template auto-assigns. Wire into `botyard_bot_tool_assignment.tool_ids`.",
		},
		"skill_ids": schema.ListAttribute{
			Computed:            true,
			ElementType:         types.StringType,
			MarkdownDescription: "IDs of skills this template auto-assigns. Wire into `botyard_bot_skill_assignment.skill_ids`.",
		},
		"files": schema.MapAttribute{
			Computed:            true,
			ElementType:         types.StringType,
			MarkdownDescription: "Default bot files keyed by file type (`soul`, `heartbeat`, `agents`, `user`, `tools`).",
		},
		"config_json": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Default bot config patch (heartbeat, model, etc.) as a JSON string, or null when the template sets no config. `jsondecode()` it if you need individual fields.",
		},
	}
}

// listBotTemplates fetches all bot templates for the organization.
func listBotTemplates(ctx context.Context, data *providerData, diags *diag.Diagnostics) ([]client.BotTemplateWithRawConfig, bool) {
	templates, status, body, err := data.client.ListBotTemplatesWithRawConfig(ctx, data.orgID)
	if err != nil {
		diags.AddError("Error reading bot templates", fmt.Sprintf("Could not list bot templates: %s", err))
		return nil, false
	}
	if status != 200 {
		diags.AddError(
			"Unexpected response reading bot templates",
			fmt.Sprintf("Listing bot templates returned HTTP %d: %s", status, describeAPIError(body)),
		)
		return nil, false
	}
	return templates, true
}

// findBotTemplateBySlug returns the template whose slug matches, or ok=false when
// none match.
func findBotTemplateBySlug(templates []client.BotTemplateWithRawConfig, slug string) (client.BotTemplateWithRawConfig, bool) {
	for _, t := range templates {
		if t.Template.Slug == slug {
			return t, true
		}
	}
	return client.BotTemplateWithRawConfig{}, false
}

// --- Singular: botyard_bot_template -----------------------------------------

// BotTemplateDataSource resolves a single visible catalog template by slug. Its
// main use is exposing default tool_ids/skill_ids so template defaults can be
// wired explicitly into the (exclusive) assignment resources —
// deliberately as a data source rather than a flag on botyard_bot, so the
// defaults stay explicit and composable and never fight the assignment
// resources for ownership of a bot's tools/skills.
type BotTemplateDataSource struct {
	data *providerData
}

// NewBotTemplateDataSource is the data-source factory registered with the provider.
func NewBotTemplateDataSource() datasource.DataSource {
	return &BotTemplateDataSource{}
}

func (d *BotTemplateDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bot_template"
}

func (d *BotTemplateDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := botTemplateComputedAttributes()
	attrs["slug"] = schema.StringAttribute{
		Required:            true,
		MarkdownDescription: "Slug of the visible bot template to look up (e.g. `coding-agent`). Hidden/internal templates, including the guided-setup wizard bundle, are not exposed by the catalog.",
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a single visible bot template by slug. Use it to source a template's " +
			"default `tool_ids` and `skill_ids` and wire them " +
			"explicitly into the assignment resources (`botyard_bot_tool_assignment`, " +
			"`botyard_bot_skill_assignment`). This keeps the defaults explicit and composable; the " +
			"assignment resources remain the single, exclusive owner of a bot's tools/skills. Hidden/internal " +
			"templates, including the guided-setup wizard bundle, are intentionally absent from the catalog.",
		Attributes: attrs,
	}
}

func (d *BotTemplateDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configureDataSourceProviderData(req, resp)
}

func (d *BotTemplateDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg BotTemplateModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	slug := cfg.Slug.ValueString()
	templates, ok := listBotTemplates(ctx, d.data, &resp.Diagnostics)
	if !ok {
		return
	}
	t, found := findBotTemplateBySlug(templates, slug)
	if !found {
		resp.Diagnostics.AddError(
			"Bot template not found",
			fmt.Sprintf("No bot template with slug %q was found in the organization.", slug),
		)
		return
	}
	state := botTemplateToModel(t)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// --- Plural: botyard_bot_templates ------------------------------------------

// BotTemplatesDataSource lists every bot template in the organization.
type BotTemplatesDataSource struct {
	data *providerData
}

// BotTemplatesDataSourceModel is the top-level state for botyard_bot_templates.
type BotTemplatesDataSourceModel struct {
	BotTemplates []BotTemplateModel `tfsdk:"bot_templates"`
}

// NewBotTemplatesDataSource is the data-source factory registered with the provider.
func NewBotTemplatesDataSource() datasource.DataSource {
	return &BotTemplatesDataSource{}
}

func (d *BotTemplatesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bot_templates"
}

func (d *BotTemplatesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists all bot templates in the organization, including their default tool/skill bundles.",
		Attributes: map[string]schema.Attribute{
			"bot_templates": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "All bot templates in the organization.",
				NestedObject:        schema.NestedAttributeObject{Attributes: botTemplateComputedAttributes()},
			},
		},
	}
}

func (d *BotTemplatesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configureDataSourceProviderData(req, resp)
}

func (d *BotTemplatesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	templates, ok := listBotTemplates(ctx, d.data, &resp.Diagnostics)
	if !ok {
		return
	}
	state := BotTemplatesDataSourceModel{BotTemplates: make([]BotTemplateModel, 0, len(templates))}
	for _, t := range templates {
		state.BotTemplates = append(state.BotTemplates, botTemplateToModel(t))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
