package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const containerDetailJSON = `{
  "mcp_server_id": "m-1", "org_id": "org-1", "runtime_kind": "container_image",
  "slug": "my-mcp", "name": "My MCP", "description": "desc", "transport": "streamable_http",
  "observed_state": "running", "desired_state": "running", "tool_count": 3,
  "config_generation": 2, "reconciled_generation": 2,
  "image": "ghcr.io/x:1", "port": 8080, "command": ["run"], "args": ["--flag"],
  "env_plaintext": {"A": "b"}, "env_secret_refs": {"TOKEN": "vault.token"}, "secret_file_mounts": {},
  "created_at": "2026-07-20T10:00:00Z", "updated_at": "2026-07-20T11:00:00Z"
}`

const managedDetailJSON = `{
  "mcp_server_id": "m-2", "org_id": "org-1", "runtime_kind": "managed_remote",
  "slug": "remote", "name": "Remote", "transport": "streamable_http",
  "observed_state": "running", "desired_state": "running", "tool_count": 0,
  "config_generation": 1, "reconciled_generation": 1,
  "endpoint_url": "https://example.com/mcp",
  "created_at": "2026-07-20T10:00:00Z", "updated_at": "2026-07-20T10:00:00Z"
}`

func TestDecodeMcpServerDetail(t *testing.T) {
	c, err := DecodeMcpServerDetail([]byte(containerDetailJSON))
	if err != nil {
		t.Fatalf("container decode: %v", err)
	}
	if c.Container == nil || c.Managed != nil {
		t.Fatalf("expected container variant, got %+v", c)
	}
	if c.Container.Image != "ghcr.io/x:1" || c.Container.Port != 8080 {
		t.Errorf("container image/port = %q/%d", c.Container.Image, c.Container.Port)
	}

	m, err := DecodeMcpServerDetail([]byte(managedDetailJSON))
	if err != nil {
		t.Fatalf("managed decode: %v", err)
	}
	if m.Managed == nil || m.Container != nil {
		t.Fatalf("expected managed variant, got %+v", m)
	}
	if m.Managed.EndpointUrl != "https://example.com/mcp" {
		t.Errorf("endpoint = %q", m.Managed.EndpointUrl)
	}

	if _, err := DecodeMcpServerDetail([]byte(`{"runtime_kind":"nope"}`)); err == nil {
		t.Error("expected error for unknown runtime_kind")
	}
}

func TestCreateMcpServerTyped_ContainerImage(t *testing.T) {
	var gotBody map[string]json.RawMessage
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(containerDetailJSON))
	}))
	defer srv.Close()

	c, err := NewClientWithResponses(srv.URL, WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer k")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	create, _ := json.Marshal(ContainerImageMcpServerCreate{
		RuntimeKind: ContainerImageMcpServerCreateRuntimeKind(McpRuntimeContainerImage),
		Name:        "My MCP",
		Image:       "ghcr.io/x:1",
		Port:        8080,
	})
	detail, status, body, err := c.CreateMcpServer(context.Background(), "org-1", create)
	if err != nil {
		t.Fatalf("create: %v (status %d body %s)", err, status, body)
	}
	if status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}
	if gotPath != "/v1/orgs/org-1/mcp-servers" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("auth = %q", gotAuth)
	}
	if string(gotBody["runtime_kind"]) != `"container_image"` || string(gotBody["image"]) != `"ghcr.io/x:1"` {
		t.Errorf("request body = %s / %s", gotBody["runtime_kind"], gotBody["image"])
	}
	if detail == nil || detail.Container == nil || detail.Container.McpServerId != "m-1" {
		t.Errorf("detail = %+v", detail)
	}
}

func TestGetMcpServerTyped_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"not found"}`))
	}))
	defer srv.Close()

	c, err := NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	detail, status, _, err := c.GetMcpServerTyped(context.Background(), "org-1", "missing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if status != http.StatusNotFound || detail != nil {
		t.Errorf("status=%d detail=%v, want 404/nil", status, detail)
	}
}
