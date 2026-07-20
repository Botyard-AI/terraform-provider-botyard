package provider

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

func strp(s string) *string { return &s }

// containerModel / managedModel build minimal valid resource models.
func containerModel() McpServerResourceModel {
	return McpServerResourceModel{
		RuntimeKind:      types.StringValue(client.McpRuntimeContainerImage),
		Name:             types.StringValue("My MCP"),
		Slug:             types.StringNull(),
		Description:      types.StringNull(),
		Transport:        types.StringNull(),
		Image:            types.StringValue("ghcr.io/x:1"),
		Port:             types.Int64Value(8080),
		Command:          types.ListNull(types.StringType),
		Args:             types.ListNull(types.StringType),
		EnvPlaintext:     types.MapNull(types.StringType),
		EnvSecretRefs:    types.MapNull(types.StringType),
		SecretFileMounts: types.MapNull(types.StringType),
		PodHostMode:      types.StringValue("pod_localhost"),
		EndpointURL:      types.StringNull(),
	}
}

func managedModel() McpServerResourceModel {
	return McpServerResourceModel{
		RuntimeKind:      types.StringValue(client.McpRuntimeManagedRemote),
		Name:             types.StringValue("Remote"),
		Slug:             types.StringNull(),
		Description:      types.StringNull(),
		Transport:        types.StringNull(),
		Image:            types.StringNull(),
		Port:             types.Int64Null(),
		Command:          types.ListNull(types.StringType),
		Args:             types.ListNull(types.StringType),
		EnvPlaintext:     types.MapNull(types.StringType),
		EnvSecretRefs:    types.MapNull(types.StringType),
		SecretFileMounts: types.MapNull(types.StringType),
		PodHostMode:      types.StringNull(),
		EndpointURL:      types.StringValue("https://example.com/mcp"),
	}
}

func TestValidateMcpServerConfig(t *testing.T) {
	// happy paths
	if d := validateMcpServerConfig(containerModel()); d.HasError() {
		t.Errorf("valid container config errored: %v", d)
	}
	if d := validateMcpServerConfig(managedModel()); d.HasError() {
		t.Errorf("valid managed config errored: %v", d)
	}

	// container missing image/port
	bad := containerModel()
	bad.Image = types.StringNull()
	bad.Port = types.Int64Null()
	if !validateMcpServerConfig(bad).HasError() {
		t.Error("container without image/port should error")
	}

	// container with endpoint_url (forbidden)
	bad = containerModel()
	bad.EndpointURL = types.StringValue("https://x")
	if !validateMcpServerConfig(bad).HasError() {
		t.Error("container with endpoint_url should error")
	}

	// managed missing endpoint_url
	bad = managedModel()
	bad.EndpointURL = types.StringNull()
	if !validateMcpServerConfig(bad).HasError() {
		t.Error("managed without endpoint_url should error")
	}

	// managed with container-only fields (forbidden)
	bad = managedModel()
	bad.Image = types.StringValue("ghcr.io/x:1")
	if !validateMcpServerConfig(bad).HasError() {
		t.Error("managed with image should error")
	}
	bad = managedModel()
	bad.PodHostMode = types.StringValue("natural")
	if !validateMcpServerConfig(bad).HasError() {
		t.Error("managed with pod_host_mode should error")
	}
}

func TestBuildCreateJSON_ContainerImage(t *testing.T) {
	body, diags := buildCreateJSON(context.Background(), containerModel())
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["runtime_kind"] != "container_image" || m["image"] != "ghcr.io/x:1" {
		t.Errorf("body = %v", m)
	}
	if m["port"].(float64) != 8080 {
		t.Errorf("port = %v", m["port"])
	}
	if m["pod_host_mode"] != "pod_localhost" {
		t.Errorf("pod_host_mode = %v", m["pod_host_mode"])
	}
	if _, ok := m["endpoint_url"]; ok {
		t.Error("container create must not carry endpoint_url")
	}
}

func TestBuildCreateJSON_ManagedRemote(t *testing.T) {
	body, diags := buildCreateJSON(context.Background(), managedModel())
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["runtime_kind"] != "managed_remote" || m["endpoint_url"] != "https://example.com/mcp" {
		t.Errorf("body = %v", m)
	}
	for _, k := range []string{"image", "port", "pod_host_mode"} {
		if _, ok := m[k]; ok {
			t.Errorf("managed create must not carry %q", k)
		}
	}
}

func TestBuildUpdateJSON_SparsePerKind(t *testing.T) {
	// container update: no endpoint_url key (would be rejected by the API)
	body, diags := buildUpdateJSON(context.Background(), containerModel(), client.McpRuntimeContainerImage)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	var cm map[string]any
	_ = json.Unmarshal(body, &cm)
	if _, ok := cm["endpoint_url"]; ok {
		t.Error("container update must omit endpoint_url")
	}
	if cm["image"] != "ghcr.io/x:1" || cm["pod_host_mode"] != "pod_localhost" {
		t.Errorf("container update body = %v", cm)
	}

	// managed update: no container-only keys
	body, diags = buildUpdateJSON(context.Background(), managedModel(), client.McpRuntimeManagedRemote)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	var mm map[string]any
	_ = json.Unmarshal(body, &mm)
	for _, k := range []string{"image", "port", "command", "args", "env_plaintext", "env_secret_refs", "secret_file_mounts", "pod_host_mode"} {
		if _, ok := mm[k]; ok {
			t.Errorf("managed update must omit %q", k)
		}
	}
	if mm["endpoint_url"] != "https://example.com/mcp" {
		t.Errorf("managed update body = %v", mm)
	}
}

func TestMapDetail_ContainerImage(t *testing.T) {
	ts := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	d := &client.McpServerDetail{
		RuntimeKind: client.McpRuntimeContainerImage,
		Container: &client.ContainerImageMcpServerDetail{
			McpServerId:      "m-1",
			OrgId:            "org-1",
			Slug:             "my-mcp",
			Name:             "My MCP",
			Description:      strp("desc"),
			Transport:        client.McpServerTransportStreamableHttp,
			Image:            "ghcr.io/x:1",
			Port:             8080,
			Command:          &[]string{"run"},
			EnvSecretRefs:    &map[string]string{"TOKEN": "vault.token"},
			ToolCount:        3,
			ConfigGeneration: 2,
			DesiredState:     client.McpServerDesiredStateRunning,
			ObservedState:    client.McpServerStateRunning,
			CreatedAt:        ts,
			UpdatedAt:        ts,
		},
	}
	// pod_host_mode is preserved from the incoming model (write-only field).
	m := McpServerResourceModel{PodHostMode: types.StringValue("pod_localhost")}
	var diags diag.Diagnostics
	mapDetail(context.Background(), d, &m, &diags)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if m.ID.ValueString() != "m-1" || m.Image.ValueString() != "ghcr.io/x:1" || m.Port.ValueInt64() != 8080 {
		t.Errorf("id/image/port = %q/%q/%d", m.ID.ValueString(), m.Image.ValueString(), m.Port.ValueInt64())
	}
	if m.PodHostMode.ValueString() != "pod_localhost" {
		t.Error("pod_host_mode should be preserved (not overwritten) on container read")
	}
	if !m.EndpointURL.IsNull() {
		t.Error("endpoint_url should be null for container_image")
	}
}

func TestMapDetail_ManagedRemote(t *testing.T) {
	ts := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	d := &client.McpServerDetail{
		RuntimeKind: client.McpRuntimeManagedRemote,
		Managed: &client.ManagedRemoteMcpServerDetail{
			McpServerId:      "m-2",
			OrgId:            "org-1",
			Slug:             "remote",
			Name:             "Remote",
			Transport:        client.McpServerTransportStreamableHttp,
			EndpointUrl:      "https://example.com/mcp",
			ToolCount:        0,
			ConfigGeneration: 1,
			DesiredState:     client.McpServerDesiredStateRunning,
			ObservedState:    client.McpServerStateRunning,
			CreatedAt:        ts,
			UpdatedAt:        ts,
		},
	}
	var m McpServerResourceModel
	var diags diag.Diagnostics
	mapDetail(context.Background(), d, &m, &diags)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	if m.RuntimeKind.ValueString() != "managed_remote" || m.EndpointURL.ValueString() != "https://example.com/mcp" {
		t.Errorf("kind/endpoint = %q/%q", m.RuntimeKind.ValueString(), m.EndpointURL.ValueString())
	}
	if !m.Image.IsNull() || !m.Port.IsNull() || !m.PodHostMode.IsNull() {
		t.Error("container-only fields (incl pod_host_mode) should be null for managed_remote")
	}
}
