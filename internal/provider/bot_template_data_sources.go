package provider

import (
	"context"
	"encoding/json"
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
// bundle: the guided-setup template (slug "guided-setup") exposes the same
// onboarding defaults the in-chat setup applies, so they can be wired
// explicitly into the assignment resources, e.g.:
//
//	data "botyard_bot_template" "defaults" { slug = "guided-setup" }
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
func botTemplateToModel(t client.BotTemplateResponse) (BotTemplateModel, error) {
	m := BotTemplateModel{
		ID:                  types.StringValue(t.Id),
		Slug:                types.StringValue(t.Slug),
		Name:                types.StringValue(t.Name),
		Description:         types.StringValue(t.Description),
		Icon:                types.StringValue(string(t.Icon)),
		SupportsGuidedSetup: boolPtrToBool(t.SupportsGuidedSetup),
		ToolIDs:             t.ToolIds,
		SkillIDs:            t.SkillIds,
		Files:               t.Files,
		ConfigJSON:          types.StringNull(),
	}
	if t.Config != nil {
		raw, err := json.Marshal(t.Config)
		if err != nil {
			return BotTemplateModel{}, fmt.Errorf("marshal config for template %q: %w", t.Slug, err)
		}
		m.ConfigJSON = types.StringValue(string(raw))
	}
	return m, nil
}

// botTemplateComputedAttributes is the per-template attribute schema shared by
// the singular and plural data sources. Every attribute is Computed; the
// singular data source overrides `slug` to Required.
func botTemplateComputedAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id":                    schema.StringAttribute{Computed: true, MarkdownDescription: "Unique template identifier (UUID)."},
		"slug":                  schema.StringAttribute{Computed: true, MarkdownDescription: "URL-safe template identifier (e.g. `guided-setup`, `personal-assistant`)."},
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
func listBotTemplates(ctx context.Context, data *providerData, diags *diag.Diagnostics) ([]client.BotTemplateResponse, bool) {
	apiResp, err := data.client.ListBotTemplatesV1OrgsOrgIdBotTemplatesGetWithResponse(ctx, data.orgID)
	if err != nil {
		diags.AddError("Error reading bot templates", fmt.Sprintf("Could not list bot templates: %s", err))
		return nil, false
	}
	if apiResp.JSON200 == nil {
		diags.AddError(
			"Unexpected response reading bot templates",
			fmt.Sprintf("Listing bot templates returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)),
		)
		return nil, false
	}
	return *apiResp.JSON200, true
}

// --- Singular: botyard_bot_template -----------------------------------------

// BotTemplateDataSource resolves a single bot template by slug. Its main use is
// exposing the guided-setup template's default tool_ids/skill_ids so onboarding
// defaults can be wired explicitly into the (exclusive) assignment resources —
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
		MarkdownDescription: "Slug of the bot template to look up (e.g. `guided-setup`).",
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a single bot template by slug. Use it to source onboarding defaults — " +
			"most notably the guided-setup template's default `tool_ids` and `skill_ids` — and wire them " +
			"explicitly into the assignment resources (`botyard_bot_tool_assignment`, " +
			"`botyard_bot_skill_assignment`). This keeps the defaults explicit and composable; the " +
			"assignment resources remain the single, exclusive owner of a bot's tools/skills.",
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
	for _, t := range templates {
		if t.Slug == slug {
			state, err := botTemplateToModel(t)
			if err != nil {
				resp.Diagnostics.AddError("Error decoding bot template", err.Error())
				return
			}
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	resp.Diagnostics.AddError(
		"Bot template not found",
		fmt.Sprintf("No bot template with slug %q was found in the organization.", slug),
	)
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
		m, err := botTemplateToModel(t)
		if err != nil {
			resp.Diagnostics.AddError("Error decoding bot template", err.Error())
			return
		}
		state.BotTemplates = append(state.BotTemplates, m)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
