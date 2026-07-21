package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

var (
	_ datasource.DataSource              = (*BotDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*BotDataSource)(nil)
)

// BotDataSource reads a single bot by slug within the provider's organization.
type BotDataSource struct {
	data *providerData
}

// BotDataSourceModel maps a curated subset of the bot resource into Terraform
// state. Nested config (desired_config) and runtime telemetry are intentionally
// omitted from the data source; they are exposed by the managed botyard_bot
// resource where appropriate.
type BotDataSourceModel struct {
	Slug                 types.String `tfsdk:"slug"`
	ID                   types.String `tfsdk:"id"`
	OrgID                types.String `tfsdk:"org_id"`
	Name                 types.String `tfsdk:"name"`
	Namespace            types.String `tfsdk:"namespace"`
	RuntimeClass         types.String `tfsdk:"runtime_class"`
	StorageClass         types.String `tfsdk:"storage_class"`
	RuntimePrivilegeMode types.String `tfsdk:"runtime_privilege_mode"`
	OnboardingState      types.String `tfsdk:"onboarding_state"`
	HealthStatus         types.String `tfsdk:"health_status"`
	DesiredState         types.String `tfsdk:"desired_state"`
	ConfigGeneration     types.Int64  `tfsdk:"config_generation"`
	CreatedAt            types.String `tfsdk:"created_at"`
	UpdatedAt            types.String `tfsdk:"updated_at"`
}

// NewBotDataSource is the data-source factory registered with the provider.
func NewBotDataSource() datasource.DataSource {
	return &BotDataSource{}
}

func (d *BotDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bot"
}

func (d *BotDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads a single Botyard bot by slug within the configured organization.",
		Attributes: map[string]schema.Attribute{
			"slug": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Slug of the bot to look up.",
			},
			"id":                     schema.StringAttribute{Computed: true, MarkdownDescription: "Unique bot identifier (UUID)."},
			"org_id":                 schema.StringAttribute{Computed: true, MarkdownDescription: "Organization ID that owns the bot."},
			"name":                   schema.StringAttribute{Computed: true, MarkdownDescription: "Human-readable bot name."},
			"namespace":              schema.StringAttribute{Computed: true, MarkdownDescription: "Kubernetes namespace for the bot runtime."},
			"runtime_class":          schema.StringAttribute{Computed: true, MarkdownDescription: "Runtime class."},
			"storage_class":          schema.StringAttribute{Computed: true, MarkdownDescription: "Storage class."},
			"runtime_privilege_mode": schema.StringAttribute{Computed: true, MarkdownDescription: "Runtime privilege mode."},
			"onboarding_state":       schema.StringAttribute{Computed: true, MarkdownDescription: "Onboarding state."},
			"health_status":          schema.StringAttribute{Computed: true, MarkdownDescription: "Health monitoring status."},
			"desired_state":          schema.StringAttribute{Computed: true, MarkdownDescription: "Control-plane desired lifecycle state."},
			"config_generation":      schema.Int64Attribute{Computed: true, MarkdownDescription: "Monotonic config generation counter."},
			"created_at":             schema.StringAttribute{Computed: true, MarkdownDescription: "Creation timestamp (RFC 3339)."},
			"updated_at":             schema.StringAttribute{Computed: true, MarkdownDescription: "Last-update timestamp (RFC 3339)."},
		},
	}
}

func (d *BotDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Data Source Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	d.data = data
}

func (d *BotDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg BotDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	slug := cfg.Slug.ValueString()
	apiResp, err := d.data.client.GetBotV1OrgsOrgIdBotsBotSlugGetWithResponse(ctx, d.data.orgID, slug)
	if err != nil {
		resp.Diagnostics.AddError("Error reading bot", fmt.Sprintf("Could not read bot %q: %s", slug, err))
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError(
			"Unexpected response reading bot",
			fmt.Sprintf("Reading bot %q returned HTTP %d: %s", slug, apiResp.StatusCode(), describeAPIError(apiResp.Body)),
		)
		return
	}

	state := botToModel(apiResp.JSON200)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// botToModel maps an API BotResponse into the data-source state model.
func botToModel(b *client.BotResponse) BotDataSourceModel {
	return BotDataSourceModel{
		Slug:                 types.StringValue(b.Slug),
		ID:                   types.StringValue(b.Id),
		OrgID:                types.StringValue(b.OrgId),
		Name:                 types.StringValue(b.Name),
		Namespace:            types.StringValue(b.Namespace),
		RuntimeClass:         types.StringValue(string(b.RuntimeClass)),
		StorageClass:         types.StringValue(string(b.StorageClass)),
		RuntimePrivilegeMode: types.StringValue(string(b.RuntimePrivilegeMode)),
		OnboardingState:      types.StringValue(string(b.OnboardingState)),
		HealthStatus:         types.StringValue(string(b.HealthStatus)),
		DesiredState:         types.StringValue(string(b.DesiredState)),
		ConfigGeneration:     types.Int64Value(int64(b.ConfigGeneration)),
		CreatedAt:            types.StringValue(b.CreatedAt.Format(time.RFC3339)),
		UpdatedAt:            types.StringValue(b.UpdatedAt.Format(time.RFC3339)),
	}
}

// describeAPIError renders a concise human-readable message from an error
// response body. It prefers a ProblemDetails "detail" (then "title") when the
// body parses as JSON, and falls back to the raw body otherwise.
func describeAPIError(body []byte) string {
	if len(body) == 0 {
		return "(empty response body)"
	}
	var pd struct {
		Detail string `json:"detail"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(body, &pd); err == nil {
		if pd.Detail != "" {
			return pd.Detail
		}
		if pd.Title != "" {
			return pd.Title
		}
	}
	return string(body)
}
