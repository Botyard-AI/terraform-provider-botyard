package provider

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

func TestMcpServerMapDetail_ContainerImage(t *testing.T) {
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

	var m McpServerResourceModel
	var diags diag.Diagnostics
	(&McpServerResource{}).mapDetail(context.Background(), d, &m, &diags)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}

	if m.ID.ValueString() != "m-1" || m.RuntimeKind.ValueString() != "container_image" {
		t.Errorf("id/kind = %q/%q", m.ID.ValueString(), m.RuntimeKind.ValueString())
	}
	if m.Image.ValueString() != "ghcr.io/x:1" || m.Port.ValueInt64() != 8080 {
		t.Errorf("image/port = %q/%d", m.Image.ValueString(), m.Port.ValueInt64())
	}
	if !m.EndpointURL.IsNull() {
		t.Error("endpoint_url should be null for container_image")
	}
	if m.ToolCount.ValueInt64() != 3 || m.CreatedAt.ValueString() != "2026-07-20T10:00:00Z" {
		t.Errorf("tool_count/created_at = %d/%q", m.ToolCount.ValueInt64(), m.CreatedAt.ValueString())
	}
	secretRefs := m.EnvSecretRefs.Elements()
	if len(secretRefs) != 1 {
		t.Errorf("env_secret_refs = %v", secretRefs)
	}
}

func TestMcpServerMapDetail_ManagedRemote(t *testing.T) {
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
	(&McpServerResource{}).mapDetail(context.Background(), d, &m, &diags)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}

	if m.RuntimeKind.ValueString() != "managed_remote" {
		t.Errorf("kind = %q", m.RuntimeKind.ValueString())
	}
	if m.EndpointURL.ValueString() != "https://example.com/mcp" {
		t.Errorf("endpoint_url = %q", m.EndpointURL.ValueString())
	}
	if !m.Image.IsNull() || !m.Port.IsNull() || !m.Command.IsNull() || !m.EnvPlaintext.IsNull() {
		t.Error("container-only fields should be null for managed_remote")
	}
}
