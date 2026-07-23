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
	_ datasource.DataSource              = (*ToolDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*ToolDataSource)(nil)
	_ datasource.DataSource              = (*ToolsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*ToolsDataSource)(nil)
)

// ToolModel maps one org tool-catalog entry into Terraform state. It is shared
// by the singular botyard_tool (looked up by slug) and the plural botyard_tools
// (one element per catalog entry) data sources.
type ToolModel struct {
	ID              types.String `tfsdk:"id"`
	Slug            types.String `tfsdk:"slug"`
	Name            types.String `tfsdk:"name"`
	Runtime         types.String `tfsdk:"runtime"`
	RuntimeToolName types.String `tfsdk:"runtime_tool_name"`
	Domain          types.String `tfsdk:"domain"`
	Description     types.String `tfsdk:"description"`
	McpServer       types.String `tfsdk:"mcp_server"`
	Enabled         types.Bool   `tfsdk:"enabled"`
	OrgID           types.String `tfsdk:"org_id"`
	CreatedAt       types.String `tfsdk:"created_at"`
	UpdatedAt       types.String `tfsdk:"updated_at"`
}

// ToolsDataSourceModel is the top-level state for the plural botyard_tools list.
type ToolsDataSourceModel struct {
	Tools []ToolModel `tfsdk:"tools"`
}

// toolToModel maps an API ToolResponse into the shared data-source model.
func toolToModel(t client.ToolResponse) ToolModel {
	return ToolModel{
		ID:              types.StringValue(t.Id),
		Slug:            types.StringValue(t.Slug),
		Name:            types.StringValue(t.Name),
		Runtime:         types.StringValue(string(t.Runtime)),
		RuntimeToolName: types.StringValue(t.RuntimeToolName),
		Domain:          types.StringValue(t.Domain),
		Description:     strPtrToStr(t.Description),
		McpServer:       strPtrToStr(t.McpServer),
		Enabled:         types.BoolValue(t.Enabled),
		OrgID:           strPtrToStr(t.OrgId),
		CreatedAt:       types.StringValue(t.CreatedAt.Format(time.RFC3339)),
		UpdatedAt:       types.StringValue(t.UpdatedAt.Format(time.RFC3339)),
	}
}

// toolComputedAttributes is the per-tool attribute schema shared by the singular
// and plural data sources. Every attribute is Computed; the singular data source
// overrides `slug` to Required (it is the lookup key).
func toolComputedAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id":                schema.StringAttribute{Computed: true, MarkdownDescription: "Unique tool identifier (UUID). Use this as `tool_ids` in `botyard_bot_tool_assignment`."},
		"slug":              schema.StringAttribute{Computed: true, MarkdownDescription: "Globally unique composite slug (`runtime:mcp_server:runtime_tool_name`)."},
		"name":              schema.StringAttribute{Computed: true, MarkdownDescription: "Human-readable display name."},
		"runtime":           schema.StringAttribute{Computed: true, MarkdownDescription: "Where the tool executes — `mcp` or `openclaw`."},
		"runtime_tool_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Internal runtime name (MCP function name or OpenClaw tool ID)."},
		"domain":            schema.StringAttribute{Computed: true, MarkdownDescription: "Tool grouping (e.g. `github`, `conversations`)."},
		"description":       schema.StringAttribute{Computed: true, MarkdownDescription: "What the tool does. May be null."},
		"mcp_server":        schema.StringAttribute{Computed: true, MarkdownDescription: "Name of the MCP server backing the tool (null for non-MCP tools)."},
		"enabled":           schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the tool is globally enabled."},
		"org_id":            schema.StringAttribute{Computed: true, MarkdownDescription: "Owning organization ID (null for platform tools)."},
		"created_at":        schema.StringAttribute{Computed: true, MarkdownDescription: "Creation timestamp (RFC 3339)."},
		"updated_at":        schema.StringAttribute{Computed: true, MarkdownDescription: "Last-update timestamp (RFC 3339)."},
	}
}

// listTools fetches the full org tool catalog. It returns (nil, false) after
// appending a diagnostic on transport or non-200 responses.
func listTools(ctx context.Context, data *providerData, diags *diag.Diagnostics) ([]client.ToolResponse, bool) {
	apiResp, err := data.client.ListToolsV1OrgsOrgIdToolsGetWithResponse(ctx, data.orgID, nil)
	if err != nil {
		diags.AddError("Error reading tools", fmt.Sprintf("Could not list tools: %s", err))
		return nil, false
	}
	if apiResp.JSON200 == nil {
		diags.AddError(
			"Unexpected response reading tools",
			fmt.Sprintf("Listing tools returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)),
		)
		return nil, false
	}
	return *apiResp.JSON200, true
}

// findToolBySlug returns the tool whose composite slug matches, or ok=false when
// none match.
func findToolBySlug(tools []client.ToolResponse, slug string) (client.ToolResponse, bool) {
	for _, t := range tools {
		if t.Slug == slug {
			return t, true
		}
	}
	return client.ToolResponse{}, false
}

// --- Singular: botyard_tool -------------------------------------------------

// ToolDataSource resolves a single org tool by its composite slug — the common
// "give me the id for this tool" case that the assignment resources need.
type ToolDataSource struct {
	data *providerData
}

// NewToolDataSource is the data-source factory registered with the provider.
func NewToolDataSource() datasource.DataSource {
	return &ToolDataSource{}
}

func (d *ToolDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tool"
}

func (d *ToolDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := toolComputedAttributes()
	attrs["slug"] = schema.StringAttribute{
		Required:            true,
		MarkdownDescription: "Composite slug of the tool to look up (e.g. `mcp:botyard:github_list_repos`).",
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up a single tool from the organization's tool catalog by its composite " +
			"slug, exposing its `id` for use with `botyard_bot_tool_assignment`.",
		Attributes: attrs,
	}
}

func (d *ToolDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configureDataSourceProviderData(req, resp)
}

func (d *ToolDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg ToolModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	slug := cfg.Slug.ValueString()
	tools, ok := listTools(ctx, d.data, &resp.Diagnostics)
	if !ok {
		return
	}
	t, found := findToolBySlug(tools, slug)
	if !found {
		resp.Diagnostics.AddError(
			"Tool not found",
			fmt.Sprintf("No tool with slug %q was found in the organization's tool catalog.", slug),
		)
		return
	}
	state := toolToModel(t)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// --- Plural: botyard_tools --------------------------------------------------

// ToolsDataSource lists every tool in the organization's tool catalog for
// exploration and for-each composition.
type ToolsDataSource struct {
	data *providerData
}

// NewToolsDataSource is the data-source factory registered with the provider.
func NewToolsDataSource() datasource.DataSource {
	return &ToolsDataSource{}
}

func (d *ToolsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tools"
}

func (d *ToolsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists all tools in the organization's tool catalog.",
		Attributes: map[string]schema.Attribute{
			"tools": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "All tools in the organization's tool catalog.",
				NestedObject:        schema.NestedAttributeObject{Attributes: toolComputedAttributes()},
			},
		},
	}
}

func (d *ToolsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configureDataSourceProviderData(req, resp)
}

func (d *ToolsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	tools, ok := listTools(ctx, d.data, &resp.Diagnostics)
	if !ok {
		return
	}
	state := ToolsDataSourceModel{Tools: make([]ToolModel, 0, len(tools))}
	for _, t := range tools {
		state.Tools = append(state.Tools, toolToModel(t))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
