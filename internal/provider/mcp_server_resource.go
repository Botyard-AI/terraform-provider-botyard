package provider

import (
	"context"
	"fmt"
	"time"

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
	_ resource.Resource                   = (*McpServerResource)(nil)
	_ resource.ResourceWithConfigure      = (*McpServerResource)(nil)
	_ resource.ResourceWithImportState    = (*McpServerResource)(nil)
	_ resource.ResourceWithValidateConfig = (*McpServerResource)(nil)
)

// McpServerResource manages an org-scoped MCP server.
type McpServerResource struct {
	data *providerData
}

// McpServerResourceModel maps the botyard_mcp_server resource schema.
type McpServerResourceModel struct {
	ID                    types.String `tfsdk:"id"`
	OrgID                 types.String `tfsdk:"org_id"`
	RuntimeKind           types.String `tfsdk:"runtime_kind"`
	Name                  types.String `tfsdk:"name"`
	Slug                  types.String `tfsdk:"slug"`
	Description           types.String `tfsdk:"description"`
	Transport             types.String `tfsdk:"transport"`
	RequestTimeoutSeconds types.Int64  `tfsdk:"request_timeout_seconds"`
	// container_image variant
	Image            types.String `tfsdk:"image"`
	Port             types.Int64  `tfsdk:"port"`
	Command          types.List   `tfsdk:"command"`
	Args             types.List   `tfsdk:"args"`
	EnvPlaintext     types.Map    `tfsdk:"env_plaintext"`
	EnvSecretRefs    types.Map    `tfsdk:"env_secret_refs"`
	SecretFileMounts types.Map    `tfsdk:"secret_file_mounts"`
	// managed_remote variant
	EndpointURL types.String `tfsdk:"endpoint_url"`
	// computed
	DesiredState     types.String `tfsdk:"desired_state"`
	ObservedState    types.String `tfsdk:"observed_state"`
	ToolCount        types.Int64  `tfsdk:"tool_count"`
	ConfigGeneration types.Int64  `tfsdk:"config_generation"`
	CreatedAt        types.String `tfsdk:"created_at"`
	UpdatedAt        types.String `tfsdk:"updated_at"`
}

// NewMcpServerResource is the resource factory registered with the provider.
func NewMcpServerResource() resource.Resource {
	return &McpServerResource{}
}

func (r *McpServerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_mcp_server"
}

func (r *McpServerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an organization-scoped Botyard MCP server. Two runtime kinds are supported: " +
			"`container_image` (Botyard runs the server as a pod) and `managed_remote` (Botyard proxies to a " +
			"vendor-hosted endpoint). Set the fields for the chosen `runtime_kind`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "MCP server ID (UUID).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"org_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Organization ID that owns the server.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"runtime_kind": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Runtime kind: `container_image` or `managed_remote`. Changing this forces " +
					"replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Human-readable label, unique within the organization.",
			},
			"slug": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "URL-safe identifier. Derived from the name when omitted.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Optional free-form description.",
			},
			"transport": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Wire transport. Defaults to `streamable_http`.",
			},
			"request_timeout_seconds": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Per-server gateway request-timeout override (seconds). Null inherits the gateway default.",
			},
			"image": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Container image reference. Required when `runtime_kind = container_image`.",
			},
			"port": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Container port for the streamable-http transport. Required when `runtime_kind = container_image`.",
			},
			"command": schema.ListAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Argv-style entrypoint override (container_image only).",
			},
			"args": schema.ListAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Argv-style arguments (container_image only).",
			},
			"env_plaintext": schema.MapAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Non-sensitive environment variables (container_image only).",
			},
			"env_secret_refs": schema.MapAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Env-var name → secret_key_path references (container_image only). These are vault key-path " +
					"pointers, not secret values.",
			},
			"secret_file_mounts": schema.MapAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Absolute container path → secret_key_path mounts, read-only (container_image only).",
			},
			"endpoint_url": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Vendor-hosted MCP URL. Required when `runtime_kind = managed_remote`.",
			},
			"desired_state":     schema.StringAttribute{Computed: true, MarkdownDescription: "Control-plane desired state."},
			"observed_state":    schema.StringAttribute{Computed: true, MarkdownDescription: "Observed lifecycle state."},
			"tool_count":        schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of tools the server advertises."},
			"config_generation": schema.Int64Attribute{Computed: true, MarkdownDescription: "Monotonic config generation counter."},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Creation timestamp (RFC 3339).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"updated_at": schema.StringAttribute{Computed: true, MarkdownDescription: "Last-update timestamp (RFC 3339)."},
		},
	}
}

func (r *McpServerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ValidateConfig enforces the per-runtime-kind required/forbidden fields that
// the discriminated API schema encodes.
func (r *McpServerResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg McpServerResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	switch cfg.RuntimeKind.ValueString() {
	case client.McpRuntimeContainerImage:
		if cfg.Image.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("image"), "Missing image",
				"`image` is required when runtime_kind = container_image.")
		}
		if cfg.Port.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("port"), "Missing port",
				"`port` is required when runtime_kind = container_image.")
		}
		if !cfg.EndpointURL.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("endpoint_url"), "Unexpected endpoint_url",
				"`endpoint_url` is only valid when runtime_kind = managed_remote.")
		}
	case client.McpRuntimeManagedRemote:
		if cfg.EndpointURL.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("endpoint_url"), "Missing endpoint_url",
				"`endpoint_url` is required when runtime_kind = managed_remote.")
		}
		for attr, isSet := range map[string]bool{
			"image": !cfg.Image.IsNull(), "port": !cfg.Port.IsNull(),
			"command": !cfg.Command.IsNull(), "args": !cfg.Args.IsNull(),
			"env_plaintext": !cfg.EnvPlaintext.IsNull(), "env_secret_refs": !cfg.EnvSecretRefs.IsNull(),
			"secret_file_mounts": !cfg.SecretFileMounts.IsNull(),
		} {
			if isSet {
				resp.Diagnostics.AddAttributeError(path.Root(attr), "Unexpected "+attr,
					"`"+attr+"` is only valid when runtime_kind = container_image.")
			}
		}
	case "":
		// unknown/interpolated runtime_kind — skip; caught at apply.
	default:
		resp.Diagnostics.AddAttributeError(path.Root("runtime_kind"), "Invalid runtime_kind",
			"runtime_kind must be `container_image` or `managed_remote`.")
	}
}

func (r *McpServerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan McpServerResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := r.buildCreate(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	detail, status, raw, err := r.data.client.CreateMcpServerTyped(ctx, r.data.orgID, body)
	if err != nil {
		resp.Diagnostics.AddError("Error creating MCP server", err.Error())
		return
	}
	if detail == nil {
		resp.Diagnostics.AddError("Unexpected response creating MCP server",
			fmt.Sprintf("Create returned HTTP %d: %s", status, describeAPIError(raw)))
		return
	}
	r.mapDetail(ctx, detail, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *McpServerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state McpServerResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	detail, status, raw, err := r.data.client.GetMcpServerTyped(ctx, r.data.orgID, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading MCP server", err.Error())
		return
	}
	if status == 404 {
		resp.State.RemoveResource(ctx)
		return
	}
	if detail == nil {
		resp.Diagnostics.AddError("Unexpected response reading MCP server",
			fmt.Sprintf("Read returned HTTP %d: %s", status, describeAPIError(raw)))
		return
	}
	r.mapDetail(ctx, detail, &state, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *McpServerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan McpServerResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	update := client.McpServerUpdate{
		Name:                  strToPtr(plan.Name),
		Slug:                  strToPtr(plan.Slug),
		Description:           strToPtr(plan.Description),
		RequestTimeoutSeconds: int64ToIntPtr(plan.RequestTimeoutSeconds),
		Image:                 strToPtr(plan.Image),
		EndpointUrl:           strToPtr(plan.EndpointURL),
		Port:                  int64ToIntPtr(plan.Port),
		Command:               listToStrSlicePtr(ctx, plan.Command, &resp.Diagnostics),
		Args:                  listToStrSlicePtr(ctx, plan.Args, &resp.Diagnostics),
		EnvPlaintext:          mapToStrMapPtr(ctx, plan.EnvPlaintext, &resp.Diagnostics),
		EnvSecretRefs:         mapToStrMapPtr(ctx, plan.EnvSecretRefs, &resp.Diagnostics),
		SecretFileMounts:      mapToStrMapPtr(ctx, plan.SecretFileMounts, &resp.Diagnostics),
	}
	if resp.Diagnostics.HasError() {
		return
	}

	apiResp, err := r.data.client.UpdateMcpServerV1OrgsOrgIdMcpServersMcpServerIdPatchWithResponse(
		ctx, r.data.orgID, plan.ID.ValueString(), update)
	if err != nil {
		resp.Diagnostics.AddError("Error updating MCP server", err.Error())
		return
	}
	if apiResp.StatusCode() != 200 {
		resp.Diagnostics.AddError("Unexpected response updating MCP server",
			fmt.Sprintf("Update returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
		return
	}
	detail, err := client.DecodeMcpServerDetail(apiResp.Body)
	if err != nil {
		resp.Diagnostics.AddError("Error decoding updated MCP server", err.Error())
		return
	}
	r.mapDetail(ctx, detail, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *McpServerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state McpServerResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiResp, err := r.data.client.DeleteMcpServerV1OrgsOrgIdMcpServersMcpServerIdDeleteWithResponse(
		ctx, r.data.orgID, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error deleting MCP server", err.Error())
		return
	}
	switch apiResp.StatusCode() {
	case 200, 202, 204, 404:
		// deleted or already gone
	default:
		resp.Diagnostics.AddError("Unexpected response deleting MCP server",
			fmt.Sprintf("Delete returned HTTP %d: %s", apiResp.StatusCode(), describeAPIError(apiResp.Body)))
	}
}

func (r *McpServerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// buildCreate constructs the concrete per-kind create struct.
func (r *McpServerResource) buildCreate(ctx context.Context, plan *McpServerResourceModel, diags *diag.Diagnostics) any {
	switch plan.RuntimeKind.ValueString() {
	case client.McpRuntimeManagedRemote:
		return client.ManagedRemoteMcpServerCreate{
			RuntimeKind:           client.ManagedRemoteMcpServerCreateRuntimeKind(client.McpRuntimeManagedRemote),
			Name:                  plan.Name.ValueString(),
			Slug:                  strToPtr(plan.Slug),
			Description:           strToPtr(plan.Description),
			Transport:             transportPtr(plan.Transport),
			RequestTimeoutSeconds: int64ToIntPtr(plan.RequestTimeoutSeconds),
			EndpointUrl:           plan.EndpointURL.ValueString(),
		}
	default: // container_image
		return client.ContainerImageMcpServerCreate{
			RuntimeKind:           client.ContainerImageMcpServerCreateRuntimeKind(client.McpRuntimeContainerImage),
			Name:                  plan.Name.ValueString(),
			Slug:                  strToPtr(plan.Slug),
			Description:           strToPtr(plan.Description),
			Transport:             transportPtr(plan.Transport),
			RequestTimeoutSeconds: int64ToIntPtr(plan.RequestTimeoutSeconds),
			Image:                 plan.Image.ValueString(),
			Port:                  int(plan.Port.ValueInt64()),
			Command:               listToStrSlicePtr(ctx, plan.Command, diags),
			Args:                  listToStrSlicePtr(ctx, plan.Args, diags),
			EnvPlaintext:          mapToStrMapPtr(ctx, plan.EnvPlaintext, diags),
			EnvSecretRefs:         mapToStrMapPtr(ctx, plan.EnvSecretRefs, diags),
			SecretFileMounts:      mapToStrMapPtr(ctx, plan.SecretFileMounts, diags),
		}
	}
}

// mapDetail writes an API detail into the resource model.
func (r *McpServerResource) mapDetail(ctx context.Context, d *client.McpServerDetail, m *McpServerResourceModel, diags *diag.Diagnostics) {
	if d.Container != nil {
		c := d.Container
		m.ID = types.StringValue(c.McpServerId)
		m.OrgID = types.StringValue(c.OrgId)
		m.RuntimeKind = types.StringValue(client.McpRuntimeContainerImage)
		m.Name = types.StringValue(c.Name)
		m.Slug = types.StringValue(c.Slug)
		m.Description = strPtrToStr(c.Description)
		m.Transport = types.StringValue(string(c.Transport))
		m.RequestTimeoutSeconds = intPtrToInt64(c.RequestTimeoutSeconds)
		m.Image = types.StringValue(c.Image)
		m.Port = types.Int64Value(int64(c.Port))
		m.Command = strSliceToList(ctx, c.Command, diags)
		m.Args = strSliceToList(ctx, c.Args, diags)
		m.EnvPlaintext = strMapToMap(ctx, c.EnvPlaintext, diags)
		m.EnvSecretRefs = strMapToMap(ctx, c.EnvSecretRefs, diags)
		m.SecretFileMounts = strMapToMap(ctx, c.SecretFileMounts, diags)
		m.EndpointURL = types.StringNull()
		m.DesiredState = types.StringValue(string(c.DesiredState))
		m.ObservedState = types.StringValue(string(c.ObservedState))
		m.ToolCount = types.Int64Value(int64(c.ToolCount))
		m.ConfigGeneration = types.Int64Value(int64(c.ConfigGeneration))
		m.CreatedAt = types.StringValue(c.CreatedAt.Format(time.RFC3339))
		m.UpdatedAt = types.StringValue(c.UpdatedAt.Format(time.RFC3339))
		return
	}
	if d.Managed != nil {
		c := d.Managed
		m.ID = types.StringValue(c.McpServerId)
		m.OrgID = types.StringValue(c.OrgId)
		m.RuntimeKind = types.StringValue(client.McpRuntimeManagedRemote)
		m.Name = types.StringValue(c.Name)
		m.Slug = types.StringValue(c.Slug)
		m.Description = strPtrToStr(c.Description)
		m.Transport = types.StringValue(string(c.Transport))
		m.RequestTimeoutSeconds = intPtrToInt64(c.RequestTimeoutSeconds)
		m.EndpointURL = types.StringValue(c.EndpointUrl)
		// container-only fields are null for this variant
		m.Image = types.StringNull()
		m.Port = types.Int64Null()
		m.Command = types.ListNull(types.StringType)
		m.Args = types.ListNull(types.StringType)
		m.EnvPlaintext = types.MapNull(types.StringType)
		m.EnvSecretRefs = types.MapNull(types.StringType)
		m.SecretFileMounts = types.MapNull(types.StringType)
		m.DesiredState = types.StringValue(string(c.DesiredState))
		m.ObservedState = types.StringValue(string(c.ObservedState))
		m.ToolCount = types.Int64Value(int64(c.ToolCount))
		m.ConfigGeneration = types.Int64Value(int64(c.ConfigGeneration))
		m.CreatedAt = types.StringValue(c.CreatedAt.Format(time.RFC3339))
		m.UpdatedAt = types.StringValue(c.UpdatedAt.Format(time.RFC3339))
		return
	}
	diags.AddError("Empty MCP server detail", "The API returned a server with no recognized runtime_kind variant.")
}

// --- small conversion helpers ---

func strToPtr(s types.String) *string {
	if s.IsNull() || s.IsUnknown() {
		return nil
	}
	v := s.ValueString()
	return &v
}

func strPtrToStr(p *string) types.String {
	if p == nil {
		return types.StringNull()
	}
	return types.StringValue(*p)
}

func int64ToIntPtr(v types.Int64) *int {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	i := int(v.ValueInt64())
	return &i
}

func intPtrToInt64(p *int) types.Int64 {
	if p == nil {
		return types.Int64Null()
	}
	return types.Int64Value(int64(*p))
}

func transportPtr(s types.String) *client.McpServerTransport {
	if s.IsNull() || s.IsUnknown() {
		return nil
	}
	t := client.McpServerTransport(s.ValueString())
	return &t
}

func listToStrSlicePtr(ctx context.Context, l types.List, diags *diag.Diagnostics) *[]string {
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	out := make([]string, 0, len(l.Elements()))
	diags.Append(l.ElementsAs(ctx, &out, false)...)
	return &out
}

func strSliceToList(ctx context.Context, p *[]string, diags *diag.Diagnostics) types.List {
	if p == nil {
		return types.ListNull(types.StringType)
	}
	v, d := types.ListValueFrom(ctx, types.StringType, *p)
	diags.Append(d...)
	return v
}

func mapToStrMapPtr(ctx context.Context, m types.Map, diags *diag.Diagnostics) *map[string]string {
	if m.IsNull() || m.IsUnknown() {
		return nil
	}
	out := make(map[string]string, len(m.Elements()))
	diags.Append(m.ElementsAs(ctx, &out, false)...)
	return &out
}

func strMapToMap(ctx context.Context, p *map[string]string, diags *diag.Diagnostics) types.Map {
	if p == nil {
		return types.MapNull(types.StringType)
	}
	v, d := types.MapValueFrom(ctx, types.StringType, *p)
	diags.Append(d...)
	return v
}
