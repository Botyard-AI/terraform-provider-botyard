package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// PlatDev #4028b: `auto_suspend_enabled`, `runtime_privilege_mode` and
// `resources` on botyard_bot, plus the D6 (#4379) owner diagnostic on the
// owner-gated bot writes.

// botOwnerForbiddenBody is the problem body of the API's bot-owner gate
// (BotAccessService.enforce_owner -> ForbiddenError, code `forbidden`).
const botOwnerForbiddenBody = `{"type":"https://docs.botyard.io/problems/forbidden","title":"Forbidden",` +
	`"status":403,"detail":"Only a bot owner can manage this bot","instance":"/v1/orgs/org-1/bots/rt-probe",` +
	`"error_code":"forbidden"}`

// upgradeRequiredBody is a plan-feature refusal (require_plan_feature).
const upgradeRequiredBody = `{"type":"https://docs.botyard.io/problems/upgrade_required",` +
	`"title":"Plan upgrade required","status":403,` +
	`"detail":"Privileged bot runtimes requires the enterprise plan. Upgrade to continue.",` +
	`"instance":"/v1/orgs/org-1/bots/rt-probe","error_code":"upgrade_required"}`

const (
	rtBotPath = "/v1/orgs/org-1/bots/rt-probe"
	rtBotID   = "8a1b2c3d-0000-4000-8000-0000000000a8"
)

// The plan's default shape, as the API returns it (CPU in millicores, memory
// and storage in the largest even binary suffix).
var rtDefaultResources = map[string]string{
	"cpu_request": "500m", "cpu_limit": "1000m",
	"memory_request": "1Gi", "memory_limit": "2Gi", "storage": "10Gi",
}

// fakeRuntimeBotAPI serves one OpenClaw bot with the runtime settings modelled
// the way update_bot applies them: sparse, `resources` overlaid onto the
// current shape or reset by null, privileged moving a runc bot onto Kata, and
// every write owner-gated.
type fakeRuntimeBotAPI struct {
	t *testing.T

	mu           sync.Mutex
	exists       bool
	owned        bool
	autoSuspend  bool
	privilege    string
	runtimeClass string
	resources    map[string]string
	desc         *string

	createBodies []map[string]json.RawMessage
	patches      []map[string]json.RawMessage
	configPats   int
	// noPrivileged refuses `privileged` with the plan-feature 403;
	// failPatch / failConfig answer 500.
	noPrivileged, failPatch, failConfig bool
	// noResources leaves `resources` null in responses (a transient,
	// un-materialised bot).
	noResources bool
}

func newFakeRuntimeBotAPI(t *testing.T) (*fakeRuntimeBotAPI, *httptest.Server) {
	t.Helper()
	api := &fakeRuntimeBotAPI{t: t, owned: true}
	api.reset()
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return api, srv
}

// reset puts the bot in its just-created shape.
func (a *fakeRuntimeBotAPI) reset() {
	a.autoSuspend, a.privilege, a.runtimeClass = true, "standard", "runc"
	a.resources = map[string]string{}
	for k, v := range rtDefaultResources {
		a.resources[k] = v
	}
}

func (a *fakeRuntimeBotAPI) set(fn func(*fakeRuntimeBotAPI)) func() {
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		fn(a)
	}
}

// runtimeKeys are the BotUpdate keys this step owns.
var runtimeKeys = []string{"auto_suspend_enabled", "runtime_privilege_mode", "resources"}

// lastPatchRuntime renders the runtime keys of the latest PATCH, sorted, so a
// step can assert exactly what was sent ("" = none of them).
func (a *fakeRuntimeBotAPI) lastPatchRuntime() string {
	if len(a.patches) == 0 {
		return "<no PATCH>"
	}
	return runtimePart(a.patches[len(a.patches)-1])
}

func runtimePart(body map[string]json.RawMessage) string {
	var parts []string
	for _, k := range runtimeKeys {
		if v, ok := body[k]; ok {
			parts = append(parts, k+"="+string(v))
		}
	}
	return strings.Join(parts, " ")
}

func (a *fakeRuntimeBotAPI) wantPatches(n int, lastRuntime string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if len(a.patches) != n {
			return fmt.Errorf("bot PATCHes = %d, want %d (%v)", len(a.patches), n, a.patches)
		}
		if n > 0 {
			if got := a.lastPatchRuntime(); got != lastRuntime {
				return fmt.Errorf("last PATCH runtime keys = %q, want %q", got, lastRuntime)
			}
		}
		return nil
	}
}

func (a *fakeRuntimeBotAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/orgs/org-1/bots":
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			a.t.Errorf("create body %s: %v", raw, err)
		}
		a.createBodies = append(a.createBodies, body)
		a.exists, a.owned = true, true
		a.reset()
		if v, ok := body["resources"]; ok {
			a.overlay(v)
		}
		a.writeBot(w, http.StatusCreated)
	case r.Method == http.MethodGet && r.URL.Path == rtBotPath && a.exists:
		a.writeBot(w, http.StatusOK)
	case r.Method == http.MethodPatch && r.URL.Path == rtBotPath && a.exists:
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			a.t.Errorf("PATCH body %s: %v", raw, err)
		}
		a.patches = append(a.patches, body)
		if !a.owned {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(botOwnerForbiddenBody))
			return
		}
		if a.failPatch {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"title":"injected PATCH failure","status":500}`))
			return
		}
		if v, ok := body["runtime_privilege_mode"]; ok && string(v) == `"privileged"` && a.noPrivileged {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(upgradeRequiredBody))
			return
		}
		if v, ok := body["description"]; ok {
			a.desc = nil
			_ = json.Unmarshal(v, &a.desc)
		}
		if v, ok := body["auto_suspend_enabled"]; ok {
			_ = json.Unmarshal(v, &a.autoSuspend)
		}
		if v, ok := body["runtime_privilege_mode"]; ok {
			_ = json.Unmarshal(v, &a.privilege)
			if a.privilege == "privileged" && a.runtimeClass == "runc" {
				a.runtimeClass = "kata_qemu"
			}
		}
		if v, ok := body["resources"]; ok {
			if string(v) == "null" {
				a.resources = map[string]string{}
				for k, q := range rtDefaultResources {
					a.resources[k] = q
				}
			} else {
				a.overlay(v)
			}
		}
		a.writeBot(w, http.StatusOK)
	case r.Method == http.MethodPatch && r.URL.Path == rtBotPath+"/config" && a.exists:
		a.configPats++
		if !a.owned {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(botOwnerForbiddenBody))
			return
		}
		if a.failConfig {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"title":"injected config failure","status":500}`))
			return
		}
		a.writeBot(w, http.StatusOK)
	case r.Method == http.MethodDelete && r.URL.Path == rtBotPath && a.exists:
		if !a.owned {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(botOwnerForbiddenBody))
			return
		}
		a.exists = false
		w.Header().Del("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/orgs/org-1/packs/installs":
		_, _ = fmt.Fprintf(w, `{"bots":[{"bot_id":%q,"installs":[]}],"not_found":[]}`, rtBotID)
	case r.Method == http.MethodGet && r.URL.Path == rtBotPath+"/suggestions" && a.exists:
		_, _ = w.Write([]byte(`{"skills":[],"tools":[],"opt_out":false}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"not found"}`))
	}
}

// overlay applies a sparse ResourceQuantities the way the API applies a
// bot's patch (DesiredResources.with_overrides, derive_requests=True): each
// sent line-item replaces its value, and a limit sent without its request
// re-derives the request from the new limit. CPU is normalised to millicores
// as the API does, so the quantity type's value comparison is exercised.
func (a *fakeRuntimeBotAPI) overlay(raw json.RawMessage) {
	var rq client.ResourceQuantities
	if err := json.Unmarshal(raw, &rq); err != nil || rq.Resources == nil {
		a.t.Errorf("resources %s: %v", raw, err)
		return
	}
	sent := map[client.MeteredResource]bool{}
	for _, it := range *rq.Resources {
		sent[it.ResourceType] = true
		q := it.Quantity
		if kindOf(it.ResourceType) == quantityCPU && !strings.HasSuffix(q, "m") {
			if n, ok := parseQuantity(quantityCPU, q); ok {
				q = fmt.Sprintf("%dm", n)
			}
		}
		a.resources[string(it.ResourceType)] = q
	}
	if sent[client.MeteredResourceCpuLimit] && !sent[client.MeteredResourceCpuRequest] {
		limit, _ := parseQuantity(quantityCPU, a.resources["cpu_limit"])
		a.resources["cpu_request"] = fmt.Sprintf("%dm", derivedCPURequest(limit))
	}
	if sent[client.MeteredResourceMemoryLimit] && !sent[client.MeteredResourceMemoryRequest] {
		limit, _ := parseQuantity(quantityBytes, a.resources["memory_limit"])
		a.resources["memory_request"] = mebibytes(derivedMemoryRequestMiB(limit))
	}
}

// derivedCPURequest mirrors botyard_core burstable_cpu_request_millicores:
// 10% of the limit, within 250m-1000m, never above the limit.
func derivedCPURequest(limitMilli int64) int64 {
	req := (limitMilli + 9) / 10
	req = max(250, min(req, 1000))
	return min(limitMilli, req)
}

// derivedMemoryRequestMiB mirrors burstable_memory_request_bytes: 60% of the
// limit with a 1536Mi floor, never above the limit.
func derivedMemoryRequestMiB(limitBytes int64) int64 {
	limitMiB := (limitBytes + (1<<20 - 1)) >> 20
	req := max(int64(1536), (limitMiB*6+9)/10)
	return min(limitMiB, req)
}

// mebibytes renders like the API: the largest binary suffix dividing evenly.
func mebibytes(mib int64) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%dGi", mib/1024)
	}
	return fmt.Sprintf("%dMi", mib)
}

func (a *fakeRuntimeBotAPI) writeBot(w http.ResponseWriter, status int) {
	desc, _ := json.Marshal(a.desc)
	resources := "null"
	if !a.noResources {
		var items []string
		for _, k := range []string{"cpu_request", "cpu_limit", "memory_request", "memory_limit", "storage"} {
			items = append(items, fmt.Sprintf(`{"resource_type":%q,"quantity":%q}`, k, a.resources[k]))
		}
		resources = `{"resources":[` + strings.Join(items, ",") + `]}`
	}
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{
		"description": %s, "resources": %s,
		"auto_suspend_enabled": %t, "runtime_privilege_mode": %q, "runtime_class": %q,
		"id": %q, "slug": "rt-probe", "org_id": "org-1", "cluster_id": "c-1",
		"name": "rt-probe", "namespace": "bot-rt-probe", "tier": "starter",
		"storage_class": "cluster_default", "onboarding_state": "complete",
		"health_status": "healthy", "desired_state": "running", "config_generation": 1,
		"access": "open", "hosting_type": "hosted", "harness": "openclaw",
		"created_at": "2026-10-29T10:00:00Z", "updated_at": "2026-10-29T10:00:00Z",
		"desired_config": {}
	}`, desc, resources, a.autoSuspend, a.privilege, a.runtimeClass, rtBotID)
}

func runtimeTestConfig(endpoint, attrs string) string {
	return fmt.Sprintf(`
provider "botyard" {
  endpoint = %q
  api_key  = "byk_test"
  org_id   = "org-1"
}

resource "botyard_bot" "b" {
  name = "rt-probe"
  %s
}
`, endpoint, attrs)
}

// TestBotRuntimeSettings_Lifecycle drives real Terraform core: omitted
// settings are adopted, not written; each change sends exactly its own key;
// an unrelated change sends none (they restart the bot); privileged moves
// runtime_class without an inconsistent-result error; `resources` sends only
// moved line-items, keeps the configured spelling, and resets on removal.
// Every step is followed by the framework's own empty-plan check.
func TestBotRuntimeSettings_Lifecycle(t *testing.T) {
	api, srv := newFakeRuntimeBotAPI(t)
	const addr = "botyard_bot.b"
	cfg := func(attrs string) string { return runtimeTestConfig(srv.URL, attrs) }

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: cfg(""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "auto_suspend_enabled", "true"),
					resource.TestCheckResourceAttr(addr, "runtime_privilege_mode", "standard"),
					resource.TestCheckResourceAttr(addr, "runtime_class", "runc"),
					resource.TestCheckNoResourceAttr(addr, "resources.cpu_limit"),
					api.wantPatches(0, ""),
					func(*terraform.State) error {
						if _, ok := api.createBodies[0]["resources"]; ok {
							return fmt.Errorf("create sent resources for an undeclared block: %v", api.createBodies[0])
						}
						return nil
					},
				),
			},
			{
				Config: cfg(`auto_suspend_enabled = false`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "auto_suspend_enabled", "false"),
					api.wantPatches(1, "auto_suspend_enabled=false"),
				),
			},
			{
				Config: cfg(`
  auto_suspend_enabled   = false
  runtime_privilege_mode = "privileged"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "runtime_privilege_mode", "privileged"),
					resource.TestCheckResourceAttr(addr, "runtime_class", "kata_qemu"),
					api.wantPatches(2, `runtime_privilege_mode="privileged"`),
				),
			},
			{
				Config: cfg(`
  auto_suspend_enabled   = false
  runtime_privilege_mode = "privileged"
  resources = {
    cpu_limit = "2"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "resources.cpu_limit", "2"),
					// Sending cpu_limit alone re-derived the request (10%, 250m floor).
					resource.TestCheckResourceAttr(addr, "resources.cpu_request", "250m"),
					resource.TestCheckResourceAttr(addr, "resources.storage", "10Gi"),
					api.wantPatches(3, `resources={"resources":[{"quantity":"2","resource_type":"cpu_limit"}]}`),
				),
			},
			{
				// Respelling the same CPU is a config diff, but the value has
				// not moved, so the PATCH carries no resources (no restart).
				Config: cfg(`
  auto_suspend_enabled   = false
  runtime_privilege_mode = "privileged"
  resources = {
    cpu_limit = "2000m"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "resources.cpu_limit", "2000m"),
					api.wantPatches(4, ""),
				),
			},
			{
				// An unrelated change resends none of the runtime settings.
				Config: cfg(`
  description            = "renamed"
  auto_suspend_enabled   = false
  runtime_privilege_mode = "privileged"
  resources = {
    cpu_limit = "2"
  }`),
				Check: api.wantPatches(5, ""),
			},
			{
				// The limit moves and the request is not configured: the API
				// re-derives it (2 -> 4 CPU: 250m -> 400m). The plan must not
				// promise the old 250m (inconsistent result otherwise).
				Config: cfg(`
  description            = "renamed"
  auto_suspend_enabled   = false
  runtime_privilege_mode = "privileged"
  resources = {
    cpu_limit = "4"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "resources.cpu_request", "400m"),
					api.wantPatches(6, `resources={"resources":[{"quantity":"4","resource_type":"cpu_limit"}]}`),
				),
			},
			{
				// Explicit requests: cpu_request moves; memory_request restates
				// the current 1Gi, so only cpu_request is sent.
				Config: cfg(`
  description            = "renamed"
  auto_suspend_enabled   = false
  runtime_privilege_mode = "privileged"
  resources = {
    cpu_limit      = "4"
    cpu_request    = "300m"
    memory_request = "1Gi"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "resources.cpu_request", "300m"),
					api.wantPatches(7, `resources={"resources":[{"quantity":"300m","resource_type":"cpu_request"}]}`),
				),
			},
			{
				// Both limits move with their explicit requests unchanged: the
				// requests travel with the limits, or the API would re-derive
				// them (300m -> 600m, 1Gi -> 4916Mi).
				Config: cfg(`
  description            = "renamed"
  auto_suspend_enabled   = false
  runtime_privilege_mode = "privileged"
  resources = {
    cpu_limit      = "6"
    cpu_request    = "300m"
    memory_limit   = "8Gi"
    memory_request = "1Gi"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "resources.cpu_request", "300m"),
					resource.TestCheckResourceAttr(addr, "resources.memory_request", "1Gi"),
					resource.TestCheckResourceAttr(addr, "resources.memory_limit", "8Gi"),
					api.wantPatches(8, `resources={"resources":[`+
						`{"quantity":"6","resource_type":"cpu_limit"},{"quantity":"8Gi","resource_type":"memory_limit"},`+
						`{"quantity":"300m","resource_type":"cpu_request"},{"quantity":"1Gi","resource_type":"memory_request"}]}`),
				),
			},
			{
				// Still restart-free: an unrelated change sends no resources,
				// explicit requests or not.
				Config: cfg(`
  description            = "renamed again"
  auto_suspend_enabled   = false
  runtime_privilege_mode = "privileged"
  resources = {
    cpu_limit      = "6"
    cpu_request    = "300m"
    memory_limit   = "8Gi"
    memory_request = "1Gi"
  }`),
				Check: api.wantPatches(9, ""),
			},
			{
				// Removing the block resets to the plan defaults; removing the
				// other two keeps their values (Optional+Computed).
				Config: cfg(`description = "renamed"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(addr, "resources.cpu_limit"),
					resource.TestCheckResourceAttr(addr, "auto_suspend_enabled", "false"),
					resource.TestCheckResourceAttr(addr, "runtime_privilege_mode", "privileged"),
					api.wantPatches(10, "resources=null"),
					func(*terraform.State) error {
						if api.resources["cpu_limit"] != "1000m" {
							return fmt.Errorf("cpu_limit after reset = %q", api.resources["cpu_limit"])
						}
						return nil
					},
				),
			},
		},
	})
}

// TestBotRuntimeSettings_Create: `resources` rides on the create POST; what
// BotCreate cannot carry follows in one PATCH, and only for values the new bot
// does not already hold (auto_suspend_enabled = true is the default here).
func TestBotRuntimeSettings_Create(t *testing.T) {
	api, srv := newFakeRuntimeBotAPI(t)
	const addr = "botyard_bot.b"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: runtimeTestConfig(srv.URL, `
  auto_suspend_enabled   = true
  runtime_privilege_mode = "privileged"
  resources = {
    memory_limit = "4Gi"
    storage      = "20Gi"
  }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "runtime_privilege_mode", "privileged"),
					resource.TestCheckResourceAttr(addr, "runtime_class", "kata_qemu"),
					resource.TestCheckResourceAttr(addr, "resources.memory_limit", "4Gi"),
					resource.TestCheckResourceAttr(addr, "resources.cpu_limit", "1000m"),
					api.wantPatches(1, `runtime_privilege_mode="privileged"`),
					func(*terraform.State) error {
						got := runtimePart(api.createBodies[0])
						want := `resources={"resources":[{"quantity":"4Gi","resource_type":"memory_limit"},` +
							`{"quantity":"20Gi","resource_type":"storage"}]}`
						if got != want {
							return fmt.Errorf("create runtime keys = %s, want %s", got, want)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestBotRuntimeSettings_Import: an imported bot re-plans clean when the
// configuration omits the settings or restates the current ones. An
// undeclared `resources` block is never adopted, so declaring one after an
// import plans as an addition; applying it compares with the bot's live
// resources and sends nothing when they already match (no restart).
func TestBotRuntimeSettings_Import(t *testing.T) {
	api, srv := newFakeRuntimeBotAPI(t)
	api.exists, api.autoSuspend = true, false
	const addr = "botyard_bot.b"
	importStep := func(attrs string) resource.TestStep {
		return resource.TestStep{
			Config:             runtimeTestConfig(srv.URL, attrs),
			ResourceName:       addr,
			ImportState:        true,
			ImportStateId:      "rt-probe",
			ImportStatePersist: true,
		}
	}
	settings := `
  auto_suspend_enabled   = false
  runtime_privilege_mode = "standard"`
	sized := settings + `
  resources = {
    cpu_limit = "1"
  }`

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			importStep(""),
			{Config: runtimeTestConfig(srv.URL, ""), PlanOnly: true},
		},
	})
	api.exists = true // the post-test destroy deleted it
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			importStep(settings),
			{Config: runtimeTestConfig(srv.URL, settings), PlanOnly: true},
			{Config: runtimeTestConfig(srv.URL, sized), PlanOnly: true, ExpectNonEmptyPlan: true},
			{
				Config: runtimeTestConfig(srv.URL, sized),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "resources.cpu_limit", "1"),
					resource.TestCheckResourceAttr(addr, "resources.memory_limit", "2Gi"),
					api.wantPatches(1, ""),
				),
			},
		},
	})
}

// TestBotRuntimeSettings_NotBotOwner: on a bot the key did not create, the
// owner-gated PATCH fails with the D6 diagnostic, not a generic 403, and
// nothing is recorded as applied.
func TestBotRuntimeSettings_NotBotOwner(t *testing.T) {
	api, srv := newFakeRuntimeBotAPI(t)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: runtimeTestConfig(srv.URL, "")},
			{
				PreConfig:   api.set(func(a *fakeRuntimeBotAPI) { a.owned = false }),
				Config:      runtimeTestConfig(srv.URL, `auto_suspend_enabled = false`),
				ExpectError: regexp.MustCompile(`(?s)Not the bot's owner.*refused to update bot "rt-probe".*API key owns\s+only the bots it\s+created`),
			},
			{
				Config:             runtimeTestConfig(srv.URL, `auto_suspend_enabled = false`),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				PreConfig: api.set(func(a *fakeRuntimeBotAPI) { a.owned = true }),
				Config:    runtimeTestConfig(srv.URL, ""),
				PlanOnly:  true,
			},
		},
	})
}

// TestBotRuntimeSettings_PlanFeature403IsNotOwnership: a 403 that is a plan
// gate (`upgrade_required`) keeps the API's own explanation and is not
// misreported as an ownership problem.
func TestBotRuntimeSettings_PlanFeature403IsNotOwnership(t *testing.T) {
	api, srv := newFakeRuntimeBotAPI(t)
	api.noPrivileged = true

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: runtimeTestConfig(srv.URL, "")},
			{
				Config:      runtimeTestConfig(srv.URL, `runtime_privilege_mode = "privileged"`),
				ExpectError: regexp.MustCompile(`(?s)Unexpected response updating bot.*HTTP 403.*requires the enterprise\s+plan`),
			},
		},
	})
}

// TestBotOwnerForbidden_ConfigAndDelete: PATCH /config and DELETE are
// owner-gated too and get the same diagnostic. Driven directly: through core
// the metadata PATCH, which precedes PATCH /config, is refused first.
func TestBotOwnerForbidden_ConfigAndDelete(t *testing.T) {
	ctx := context.Background()
	api, srv := newFakeRuntimeBotAPI(t)
	api.exists, api.owned = true, false
	r := &BotResource{data: &providerData{client: newBotClient(t, srv.URL, "byk_test"), orgID: "org-1"}}

	var diags diag.Diagnostics
	if _, ok := r.updateBotConfig(ctx, "rt-probe", BotResourceModel{Config: &botConfigModel{}}, &diags); ok {
		t.Fatal("config PATCH succeeded on a bot the key does not own")
	}
	wantOwnerDiag(t, diags, `refused to update the config of bot "rt-probe"`)

	sch := botSchema(t)
	st := tfsdk.State{Schema: sch}
	if d := st.Set(ctx, &BotResourceModel{Name: types.StringValue("rt-probe"), Slug: types.StringValue("rt-probe")}); d.HasError() {
		t.Fatalf("state: %v", d)
	}
	resp := fwresource.DeleteResponse{State: st}
	r.Delete(ctx, fwresource.DeleteRequest{State: st}, &resp)
	wantOwnerDiag(t, resp.Diagnostics, `refused to delete bot "rt-probe"`)
}

func wantOwnerDiag(t *testing.T, diags diag.Diagnostics, detail string) {
	t.Helper()
	for _, d := range diags.Errors() {
		if d.Summary() == "Not the bot's owner" && strings.Contains(d.Detail(), detail) &&
			strings.Contains(d.Detail(), "Only a bot owner can manage this bot") {
			return
		}
	}
	t.Fatalf("diagnostics %v lack the owner error containing %q", diags.Errors(), detail)
}

func TestIsBotOwnerForbidden(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{403, botOwnerForbiddenBody, true},
		{403, upgradeRequiredBody, false},
		{403, `{"detail":"no code"}`, false},
		{403, `not json`, false},
		{500, botOwnerForbiddenBody, false},
	} {
		if got := isBotOwnerForbidden(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("isBotOwnerForbidden(%d, %s) = %t, want %t", tc.status, tc.body, got, tc.want)
		}
	}
}

func rtQuantity(kind quantityKind, s string) quantityValue {
	return quantityValue{StringValue: basetypes.NewStringValue(s), kind: kind}
}

func rtResources(cpuLimit string) *mcpServerResourcesModel {
	return &mcpServerResourcesModel{
		CPURequest:    rtQuantity(quantityCPU, "500m"),
		CPULimit:      rtQuantity(quantityCPU, cpuLimit),
		MemoryRequest: rtQuantity(quantityBytes, "1Gi"),
		MemoryLimit:   rtQuantity(quantityBytes, "2Gi"),
		Storage:       rtQuantity(quantityBytes, "10Gi"),
	}
}

func rtPrior() BotResourceModel {
	return BotResourceModel{
		Name:                 types.StringValue("rt-probe"),
		Slug:                 types.StringValue("rt-probe"),
		ID:                   types.StringValue(rtBotID),
		Description:          types.StringNull(),
		SuggestionsOptOut:    types.BoolValue(false),
		AutoSuspendEnabled:   types.BoolValue(true),
		RuntimePrivilegeMode: types.StringValue("standard"),
		RuntimeClass:         types.StringValue("runc"),
		Resources:            rtResources("1000m"),
	}
}

// TestBotRuntimeSettings_UpdateCheckpointsAppliedSettings: when the config
// PATCH fails after the bot PATCH applied the runtime settings, the returned
// state holds them. The framework seeds the update response with the PRIOR
// state, so without the checkpoint the next apply would resend them and
// restart the bot again. Refresh-free: a refresh would mask the saved state.
func TestBotRuntimeSettings_UpdateCheckpointsAppliedSettings(t *testing.T) {
	ctx := context.Background()
	api, srv := newFakeRuntimeBotAPI(t)
	api.exists, api.failConfig = true, true
	r := &BotResource{data: &providerData{client: newBotClient(t, srv.URL, "byk_test"), orgID: "org-1"}}
	sch := botSchema(t)

	prior := rtPrior()
	plan := prior
	plan.Description = types.StringValue("pending")
	plan.AutoSuspendEnabled = types.BoolValue(false)
	plan.RuntimePrivilegeMode = types.StringValue("privileged")
	plan.RuntimeClass = types.StringUnknown()
	plan.Resources = rtResources("2")
	plan.Config = &botConfigModel{}

	priorState := tfsdk.State{Schema: sch}
	if d := priorState.Set(ctx, &prior); d.HasError() {
		t.Fatalf("prior state: %v", d)
	}
	planned := tfsdk.Plan{Schema: sch}
	if d := planned.Set(ctx, &plan); d.HasError() {
		t.Fatalf("plan: %v", d)
	}
	resp := fwresource.UpdateResponse{State: priorState}
	r.Update(ctx, fwresource.UpdateRequest{Plan: planned, State: priorState}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Update succeeded; want the injected config failure")
	}

	var got BotResourceModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("returned state: %v", d)
	}
	if !got.AutoSuspendEnabled.Equal(types.BoolValue(false)) {
		t.Errorf("auto_suspend_enabled = %s, want the applied false", got.AutoSuspendEnabled)
	}
	if got.RuntimePrivilegeMode.ValueString() != "privileged" || got.RuntimeClass.ValueString() != "kata_qemu" {
		t.Errorf("privilege/class = %s/%s, want the applied privileged/kata_qemu", got.RuntimePrivilegeMode, got.RuntimeClass)
	}
	if got.Resources == nil || got.Resources.CPULimit.ValueString() != "2" {
		t.Errorf("resources = %+v, want the applied cpu_limit 2", got.Resources)
	}
	if !got.Description.IsNull() {
		t.Errorf("description = %s, want the prior null: only the runtime settings are checkpointed", got.Description)
	}
	if api.configPats != 1 {
		t.Errorf("config PATCHes = %d, want 1", api.configPats)
	}
}

// TestBotRuntimeSettings_PartialCreate: the bot exists once the POST
// succeeds, so a failed follow-up PATCH still records it, with what the bot
// actually holds (not the configured values), and with no unknowns even when
// the response carried no resources.
func TestBotRuntimeSettings_PartialCreate(t *testing.T) {
	ctx := context.Background()
	api, srv := newFakeRuntimeBotAPI(t)
	api.failPatch, api.noResources = true, true
	r := &BotResource{data: &providerData{client: newBotClient(t, srv.URL, "byk_test"), orgID: "org-1"}}
	sch := botSchema(t)

	plan := BotResourceModel{
		Name:                 types.StringValue("rt-probe"),
		Description:          types.StringNull(),
		AvatarURL:            types.StringNull(),
		HostingType:          types.StringUnknown(),
		Harness:              types.StringUnknown(),
		SuggestionsOptOut:    types.BoolValue(true),
		AutoSuspendEnabled:   types.BoolValue(false),
		RuntimePrivilegeMode: types.StringUnknown(),
		Resources: &mcpServerResourcesModel{
			CPURequest:    quantityValue{StringValue: basetypes.NewStringUnknown(), kind: quantityCPU},
			CPULimit:      rtQuantity(quantityCPU, "2"),
			MemoryRequest: quantityValue{StringValue: basetypes.NewStringUnknown(), kind: quantityBytes},
			MemoryLimit:   quantityValue{StringValue: basetypes.NewStringUnknown(), kind: quantityBytes},
			Storage:       quantityValue{StringValue: basetypes.NewStringUnknown(), kind: quantityBytes},
		},
	}
	unknownComputed(&plan)
	planned := tfsdk.Plan{Schema: sch}
	if d := planned.Set(ctx, &plan); d.HasError() {
		t.Fatalf("plan: %v", d)
	}
	resp := fwresource.CreateResponse{State: tfsdk.State{Schema: sch}}
	r.Create(ctx, fwresource.CreateRequest{Plan: planned}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("Create succeeded; want the injected PATCH failure")
	}
	var got BotResourceModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("state: %v", d)
	}
	if got.Slug.ValueString() != "rt-probe" {
		t.Fatalf("slug = %s: the created bot was not recorded", got.Slug)
	}
	if !got.AutoSuspendEnabled.Equal(types.BoolValue(true)) {
		t.Errorf("auto_suspend_enabled = %s, want the bot's true: false was never applied", got.AutoSuspendEnabled)
	}
	if !got.SuggestionsOptOut.IsNull() {
		t.Errorf("suggestions_opt_out = %s, want null: it was never written", got.SuggestionsOptOut)
	}
	if got.Resources == nil || !got.Resources.CPURequest.IsNull() || got.Resources.CPULimit.ValueString() != "2" {
		t.Errorf("resources = %+v, want unknown leaves nulled and the POSTed cpu_limit kept", got.Resources)
	}
}

// unknownComputed marks the server-owned attributes unknown, as a create plan
// has them.
func unknownComputed(m *BotResourceModel) {
	for _, s := range []*types.String{&m.ID, &m.Slug, &m.OrgID, &m.Namespace, &m.RuntimeClass, &m.StorageClass,
		&m.Access, &m.DesiredState, &m.HealthStatus, &m.OnboardingState, &m.CreatedAt, &m.UpdatedAt} {
		*s = types.StringUnknown()
	}
	m.DurableRootOwnsHome = types.BoolUnknown()
	m.ConfigGeneration = types.Int64Unknown()
}
