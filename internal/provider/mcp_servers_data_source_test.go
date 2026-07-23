package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// cannedMcpServersJSON is a discriminated union list with one container-image
// and one managed-remote summary. Both share the id/name/slug/runtime_kind
// fields the data source projects.
const cannedMcpServersJSON = `[
  {
    "mcp_server_id": "mcp-1",
    "org_id": "org-1",
    "name": "GitHub",
    "slug": "github",
    "runtime_kind": "container_image",
    "image": "ghcr.io/x/y:1",
    "port": 8080,
    "transport": "streamable_http",
    "desired_state": "running",
    "observed_state": "running",
    "tool_count": 3,
    "created_at": "2026-07-20T10:00:00Z",
    "updated_at": "2026-07-20T11:00:00Z"
  },
  {
    "mcp_server_id": "mcp-2",
    "org_id": "org-1",
    "name": "Remote Thing",
    "slug": "remote-thing",
    "runtime_kind": "managed_remote",
    "endpoint_url": "https://example.com/mcp",
    "transport": "streamable_http",
    "desired_state": "running",
    "observed_state": "running",
    "tool_count": 0,
    "created_at": "2026-07-20T10:00:00Z",
    "updated_at": "2026-07-20T11:00:00Z"
  }
]`

// TestMcpServers_UnionDecode proves the response body decodes into the minimal
// lite projection for both union variants, mapping mcp_server_id -> id.
func TestMcpServers_UnionDecode(t *testing.T) {
	const (
		apiKey = "byk_test_secret"
		orgID  = "org-1"
	)
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedMcpServersJSON))
	}))
	defer srv.Close()

	c, err := client.NewClientWithResponses(srv.URL, client.WithRequestEditorFn(bearerAuth(apiKey)))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}

	resp, err := c.ListMcpServersV1OrgsOrgIdMcpServersGetWithResponse(context.Background(), orgID, nil)
	if err != nil {
		t.Fatalf("ListMcpServers...: %v", err)
	}
	if want := "/v1/orgs/" + orgID + "/mcp-servers"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if resp.JSON200 == nil {
		t.Fatalf("JSON200 nil (status %d body %q)", resp.StatusCode(), string(resp.Body))
	}

	var lite []mcpServerSummaryLite
	if err := json.Unmarshal(resp.Body, &lite); err != nil {
		t.Fatalf("decode lite: %v", err)
	}
	if len(lite) != 2 {
		t.Fatalf("got %d servers, want 2", len(lite))
	}
	if lite[0].McpServerID != "mcp-1" || lite[0].Slug != "github" || lite[0].RuntimeKind != "container_image" {
		t.Errorf("lite[0] = %+v", lite[0])
	}
	if lite[1].McpServerID != "mcp-2" || lite[1].Name != "Remote Thing" || lite[1].RuntimeKind != "managed_remote" {
		t.Errorf("lite[1] = %+v", lite[1])
	}
}
