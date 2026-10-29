package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// A real `terraform plan/apply` against a fake MCP server API that normalises
// quantities exactly as Botyard does (CPU -> millicores, bytes -> the largest
// even binary suffix). This is the only test that exercises the framework's
// handling of the custom quantity type end to end. A broken semantic equality
// shows up here as "Provider produced inconsistent result after apply" or as a
// non-empty plan after apply. Neither can be caught by the pure-function tests.
//
// It runs without TF_ACC, because the API is local. terraform-plugin-testing
// uses the terraform binary on PATH (or TF_ACC_TERRAFORM_PATH), and downloads
// one when neither is set.

const fakeOrg = "org-1"

type fakeMcpAPI struct {
	mu        sync.Mutex
	resources map[string]string // effective, normalised
	custom    bool
	patches   []map[string]json.RawMessage
	created   map[string]json.RawMessage
	desc      *string
	exists    bool
}

func newFakeMcpAPI() *fakeMcpAPI {
	f := &fakeMcpAPI{}
	f.resetResources()
	return f
}

func (f *fakeMcpAPI) resetResources() {
	f.resources = map[string]string{
		"cpu_request": "100m", "cpu_limit": "500m",
		"memory_request": "128Mi", "memory_limit": "512Mi", "storage": "1Gi",
	}
	f.custom = false
}

func normalise(resourceType, q string) string {
	kind := quantityBytes
	if strings.HasPrefix(resourceType, "cpu_") {
		kind = quantityCPU
	}
	n, ok := parseQuantity(kind, q)
	if !ok {
		return q
	}
	if kind == quantityCPU {
		return fmt.Sprintf("%dm", n)
	}
	for _, s := range []struct {
		suffix string
		mult   int64
	}{{"Pi", 1 << 50}, {"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}} {
		if n != 0 && n%s.mult == 0 {
			return fmt.Sprintf("%d%s", n/s.mult, s.suffix)
		}
	}
	return fmt.Sprintf("%d", n)
}

// applyResources applies a sparse patch (or null) the way the API does.
func (f *fakeMcpAPI) applyResources(raw json.RawMessage) {
	if raw == nil {
		return
	}
	if string(raw) == "null" {
		f.resetResources()
		return
	}
	var rq struct {
		Resources []struct {
			ResourceType string `json:"resource_type"`
			Quantity     string `json:"quantity"`
		} `json:"resources"`
	}
	_ = json.Unmarshal(raw, &rq)
	for _, it := range rq.Resources {
		f.resources[it.ResourceType] = normalise(it.ResourceType, it.Quantity)
	}
	f.custom = true
}

func (f *fakeMcpAPI) detail() []byte {
	items := []map[string]string{}
	for _, k := range []string{"cpu_request", "cpu_limit", "memory_request", "memory_limit", "storage"} {
		items = append(items, map[string]string{"resource_type": k, "quantity": f.resources[k]})
	}
	d := map[string]any{
		"runtime_kind": "container_image", "mcp_server_id": "m-1", "org_id": fakeOrg,
		"slug": "runner", "name": "Runner", "description": f.desc, "transport": "streamable_http",
		"image": "ghcr.io/x:1", "port": 8080, "command": nil, "args": nil,
		"composio_connection_id": nil, "composio_connection_state": nil, "credential_sidecar_image": nil,
		"last_error": nil, "mcp_catalog_entry_id": nil,
		"desired_state": "running", "observed_state": "running", "tool_count": 0, "config_generation": 1,
		"created_at": "2026-10-24T00:00:00Z", "updated_at": "2026-10-24T00:00:00Z",
		"access": "restricted", "resources": map[string]any{"resources": items},
		"resources_customized": f.custom,
	}
	out, _ := json.Marshal(d)
	return out
}

func (f *fakeMcpAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		base := "/v1/orgs/" + fakeOrg + "/mcp-servers"
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == base:
			_ = json.Unmarshal(body, &f.created)
			f.applyResources(f.created["resources"])
			if d, ok := f.created["description"]; ok && string(d) != "null" {
				var s string
				_ = json.Unmarshal(d, &s)
				f.desc = &s
			}
			f.exists = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(f.detail())
		case r.Method == http.MethodGet && r.URL.Path == base+"/m-1":
			if !f.exists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"detail":"not found"}`))
				return
			}
			_, _ = w.Write(f.detail())
		case r.Method == http.MethodPatch && r.URL.Path == base+"/m-1":
			var patch map[string]json.RawMessage
			_ = json.Unmarshal(body, &patch)
			f.patches = append(f.patches, patch)
			f.applyResources(patch["resources"])
			if d, ok := patch["description"]; ok {
				if string(d) == "null" {
					f.desc = nil
				} else {
					var s string
					_ = json.Unmarshal(d, &s)
					f.desc = &s
				}
			}
			_, _ = w.Write(f.detail())
		case r.Method == http.MethodDelete && r.URL.Path == base+"/m-1":
			f.exists = false
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (f *fakeMcpAPI) lastPatch() map[string]json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.patches) == 0 {
		return nil
	}
	return f.patches[len(f.patches)-1]
}

func (f *fakeMcpAPI) patchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.patches)
}

func mcpConfig(endpoint, description, resources string) string {
	desc := ""
	if description != "" {
		desc = fmt.Sprintf("description = %q", description)
	}
	return fmt.Sprintf(`
provider "botyard" {
  endpoint = %q
  api_key  = "test"
  org_id   = %q
}

resource "botyard_mcp_server" "runner" {
  runtime_kind = "container_image"
  name         = "Runner"
  image        = "ghcr.io/x:1"
  port         = 8080
  %s
  %s
}
`, endpoint, fakeOrg, desc, resources)
}

func TestMcpServerResources_Lifecycle(t *testing.T) {
	f := newFakeMcpAPI()
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	var patchesBefore int
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// 1. Create with spellings the API normalises ("2" -> "2000m",
			//    "2048Mi" -> "2Gi"). The configured spelling must survive and the
			//    post-apply plan must be empty (the harness checks that).
			{
				Config: mcpConfig(srv.URL, "", `resources = {
    cpu_limit    = "2"
    memory_limit = "2048Mi"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("botyard_mcp_server.runner", "resources.cpu_limit", "2"),
					resource.TestCheckResourceAttr("botyard_mcp_server.runner", "resources.memory_limit", "2048Mi"),
					// Unset line-items adopt the server's values.
					resource.TestCheckResourceAttr("botyard_mcp_server.runner", "resources.storage", "1Gi"),
					func(_ *terraform.State) error {
						got := f.created["resources"]
						if !strings.Contains(string(got), `"cpu_limit"`) || strings.Contains(string(got), `"storage"`) {
							return fmt.Errorf("create resources = %s, want only the configured line-items", got)
						}
						patchesBefore = f.patchCount()
						return nil
					},
				),
			},
			// 2. An unrelated change must not resend resources: that would
			//    restart the pod.
			{
				Config: mcpConfig(srv.URL, "renamed", `resources = {
    cpu_limit    = "2"
    memory_limit = "2048Mi"
  }`),
				Check: func(_ *terraform.State) error {
					if f.patchCount() != patchesBefore+1 {
						return fmt.Errorf("patches = %d, want exactly one", f.patchCount()-patchesBefore)
					}
					if raw, present := f.lastPatch()["resources"]; present {
						return fmt.Errorf("a description-only apply sent resources = %s", raw)
					}
					return nil
				},
			},
			// 3. Change one quantity: only that line-item is sent.
			{
				Config: mcpConfig(srv.URL, "renamed", `resources = {
    cpu_limit    = "2"
    memory_limit = "2048Mi"
    storage      = "10Gi"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("botyard_mcp_server.runner", "resources.storage", "10Gi"),
					func(_ *terraform.State) error {
						got := decodeItems(t, f.lastPatch()["resources"])
						if len(got) != 1 || got["storage"] != "10Gi" {
							return fmt.Errorf("patch resources = %v, want only storage", got)
						}
						return nil
					},
				),
			},
			// 4. Drift: someone resizes the server in the app. A refresh-only
			//    plan must show it.
			{
				PreConfig: func() {
					f.mu.Lock()
					f.resources["cpu_limit"] = "1000m"
					f.mu.Unlock()
				},
				Config: mcpConfig(srv.URL, "renamed", `resources = {
    cpu_limit    = "2"
    memory_limit = "2048Mi"
    storage      = "10Gi"
  }`),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			// 5. Re-apply converges the drift back.
			{
				Config: mcpConfig(srv.URL, "renamed", `resources = {
    cpu_limit    = "2"
    memory_limit = "2048Mi"
    storage      = "10Gi"
  }`),
				Check: func(_ *terraform.State) error {
					got := decodeItems(t, f.lastPatch()["resources"])
					if len(got) != 1 || got["cpu_limit"] != "2" {
						return fmt.Errorf("drift-repair patch = %v, want only cpu_limit", got)
					}
					return nil
				},
			},
			// 6. Removing the block resets the server to the platform default.
			{
				Config: mcpConfig(srv.URL, "renamed", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("botyard_mcp_server.runner", "resources.cpu_limit"),
					func(_ *terraform.State) error {
						if raw := f.lastPatch()["resources"]; string(raw) != "null" {
							return fmt.Errorf("removed block sent resources = %s, want null", raw)
						}
						if f.custom {
							return fmt.Errorf("server still customised after reset")
						}
						return nil
					},
				),
			},
			// 7. Undeclared: a size set in the app is left alone and plans clean.
			{
				PreConfig: func() {
					f.mu.Lock()
					f.resources["storage"] = "20Gi"
					f.custom = true
					f.mu.Unlock()
				},
				Config:   mcpConfig(srv.URL, "renamed", ""),
				PlanOnly: true,
			},
			// 8. Declaring the block on the existing server: the unset
			//    line-items adopt the server's values. With the stock
			//    UseStateForUnknown they planned null (their absent parent's
			//    null) and the apply failed with "inconsistent result".
			{
				Config: mcpConfig(srv.URL, "renamed", `resources = {
    cpu_limit = "2"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("botyard_mcp_server.runner", "resources.cpu_limit", "2"),
					resource.TestCheckResourceAttr("botyard_mcp_server.runner", "resources.storage", "20Gi"),
				),
			},
		},
	})
}
