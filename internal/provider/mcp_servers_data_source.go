package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*McpServersDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*McpServersDataSource)(nil)
)

// mcpServerSummaryLite is the minimal projection decoded from each element of
// the /mcp-servers list. The endpoint returns a discriminated union of
// container-image and managed-remote server summaries; the generated client
// wraps each element in an opaque union type, so we decode the shared identity
// fields (present on both variants) directly from the response body.
type mcpServerSummaryLite struct {
	McpServerID string `json:"mcp_server_id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	RuntimeKind string `json:"runtime_kind"`
}

// McpServerModel maps one MCP server into Terraform state. `id` is the server
// UUID, matching the `id` attribute of the botyard_mcp_server resource.
type McpServerModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Slug        types.String `tfsdk:"slug"`
	RuntimeKind types.String `tfsdk:"runtime_kind"`
}

// McpServersDataSourceModel is the top-level state for botyard_mcp_servers.
type McpServersDataSourceModel struct {
	McpServers []McpServerModel `tfsdk:"mcp_servers"`
}

// McpServersDataSource lists the organization's MCP servers. The singular
// botyard_mcp_server is a managed resource; this data source complements it with
// read-only discovery/exploration.
type McpServersDataSource struct {
	data *providerData
}

// NewMcpServersDataSource is the data-source factory registered with the provider.
func NewMcpServersDataSource() datasource.DataSource {
	return &McpServersDataSource{}
}

func (d *McpServersDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mcp_servers"
}

func (d *McpServersDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the organization's MCP servers (both container-image and managed-remote runtimes).",
		Attributes: map[string]schema.Attribute{
			"mcp_servers": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "All MCP servers in the organization.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":           schema.StringAttribute{Computed: true, MarkdownDescription: "Unique MCP server identifier (UUID)."},
						"name":         schema.StringAttribute{Computed: true, MarkdownDescription: "Human-readable label."},
						"slug":         schema.StringAttribute{Computed: true, MarkdownDescription: "URL-safe unique-per-org identifier."},
						"runtime_kind": schema.StringAttribute{Computed: true, MarkdownDescription: "Runtime kind — `container_image` or `managed_remote`."},
					},
				},
			},
		},
	}
}

func (d *McpServersDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.data = configureDataSourceProviderData(req, resp)
}

func (d *McpServersDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	apiResp, err := d.data.client.ListMcpServersV1OrgsOrgIdMcpServersGetWithResponse(ctx, d.data.orgID, nil)
	if err != nil {
		resp.Diagnostics.AddError("Error reading MCP servers", fmt.Sprintf("Could not list MCP servers: %s", err))
		return
	}
	if apiResp.JSON200 == nil {
		resp.Diagnostics.AddError(
			"Unexpected response reading MCP servers",
			fmt.Sprintf("Listing MCP servers returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)),
		)
		return
	}

	var lite []mcpServerSummaryLite
	if err := json.Unmarshal(apiResp.Body, &lite); err != nil {
		resp.Diagnostics.AddError(
			"Error decoding MCP servers",
			fmt.Sprintf("Could not decode the MCP server list response: %s", err),
		)
		return
	}

	state := McpServersDataSourceModel{McpServers: make([]McpServerModel, 0, len(lite))}
	for _, s := range lite {
		state.McpServers = append(state.McpServers, McpServerModel{
			ID:          types.StringValue(s.McpServerID),
			Name:        types.StringValue(s.Name),
			Slug:        types.StringValue(s.Slug),
			RuntimeKind: types.StringValue(s.RuntimeKind),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
