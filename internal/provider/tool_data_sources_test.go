package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// cannedToolsJSON is a two-entry tool catalog exercising both a non-null
// description/mcp_server/org_id and their null forms.
const cannedToolsJSON = `[
  {
    "id": "tool-1",
    "org_id": "org-1",
    "name": "List Repos",
    "slug": "mcp:botyard:github_list_repos",
    "runtime_tool_name": "github_list_repos",
    "description": "Lists repositories",
    "runtime": "mcp",
    "mcp_server": "botyard",
    "domain": "github",
    "enabled": true,
    "created_at": "2026-07-20T10:00:00Z",
    "updated_at": "2026-07-20T11:00:00Z"
  },
  {
    "id": "tool-2",
    "org_id": null,
    "name": "Send Message",
    "slug": "openclaw::message",
    "runtime_tool_name": "message",
    "description": null,
    "runtime": "openclaw",
    "mcp_server": null,
    "domain": "messaging",
    "enabled": false,
    "created_at": "2026-07-20T10:00:00Z",
    "updated_at": "2026-07-20T11:00:00Z"
  }
]`

// newToolCatalogServer serves the canned catalog and records the request auth + path.
func newToolCatalogServer(t *testing.T, gotAuth, gotPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotAuth = r.Header.Get("Authorization")
		*gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedToolsJSON))
	}))
}

func TestListTools_RoundTripAndMapping(t *testing.T) {
	const (
		apiKey = "byk_test_secret"
		orgID  = "org-1"
	)
	var gotAuth, gotPath string
	srv := newToolCatalogServer(t, &gotAuth, &gotPath)
	defer srv.Close()

	c, err := client.NewClientWithResponses(srv.URL, client.WithRequestEditorFn(bearerAuth(apiKey)))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	data := &providerData{client: c, orgID: orgID}

	var diags diag.Diagnostics
	tools, ok := listTools(context.Background(), data, &diags)
	if !ok || diags.HasError() {
		t.Fatalf("listTools failed: ok=%v diags=%v", ok, diags)
	}
	if want := "Bearer " + apiKey; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
	if want := "/v1/orgs/" + orgID + "/tools"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}

	// Non-null entry.
	m0 := toolToModel(tools[0])
	if m0.ID.ValueString() != "tool-1" || m0.Slug.ValueString() != "mcp:botyard:github_list_repos" {
		t.Errorf("m0 id/slug = %q/%q", m0.ID.ValueString(), m0.Slug.ValueString())
	}
	if m0.Runtime.ValueString() != "mcp" || m0.Domain.ValueString() != "github" {
		t.Errorf("m0 runtime/domain = %q/%q", m0.Runtime.ValueString(), m0.Domain.ValueString())
	}
	if m0.Description.ValueString() != "Lists repositories" || m0.McpServer.ValueString() != "botyard" {
		t.Errorf("m0 description/mcp_server = %q/%q", m0.Description.ValueString(), m0.McpServer.ValueString())
	}
	if m0.OrgID.ValueString() != "org-1" || !m0.Enabled.ValueBool() {
		t.Errorf("m0 org_id/enabled = %q/%v", m0.OrgID.ValueString(), m0.Enabled.ValueBool())
	}

	// Null-bearing entry: description, mcp_server, org_id must be null.
	m1 := toolToModel(tools[1])
	if !m1.Description.IsNull() {
		t.Errorf("m1 description = %q, want null", m1.Description.ValueString())
	}
	if !m1.McpServer.IsNull() {
		t.Errorf("m1 mcp_server = %q, want null", m1.McpServer.ValueString())
	}
	if !m1.OrgID.IsNull() {
		t.Errorf("m1 org_id = %q, want null", m1.OrgID.ValueString())
	}
	if m1.Runtime.ValueString() != "openclaw" || m1.Enabled.ValueBool() {
		t.Errorf("m1 runtime/enabled = %q/%v", m1.Runtime.ValueString(), m1.Enabled.ValueBool())
	}
}

func TestListTools_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"boom"}`))
	}))
	defer srv.Close()

	c, err := client.NewClientWithResponses(srv.URL, client.WithRequestEditorFn(bearerAuth("k")))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	data := &providerData{client: c, orgID: "org-1"}

	var diags diag.Diagnostics
	if _, ok := listTools(context.Background(), data, &diags); ok {
		t.Fatal("listTools returned ok on a 500 response")
	}
	if !diags.HasError() {
		t.Fatal("expected an error diagnostic on 500")
	}
}
