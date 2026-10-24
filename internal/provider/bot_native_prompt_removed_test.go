package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// PlatDev #4034. Botyard PR 4045 removed `prompt_template` and `prompt_ref`
// from NativeConfigPatch and made it extra="forbid", so a v0.5.0 provider sends
// `"prompt_ref": null` on every native config write and gets a 422. Proven live
// against a local API on 2026-10-24:
//
//	POST /v1/orgs/{org}/bots  config={"bot_type":"native","prompt_ref":null,"tool_search":{"mode":"auto"}}
//	-> 422  /config/...NativeConfigPatch/prompt_ref: Extra inputs are not permitted
//
// fakeNativeBotAPI models both sides of that change. In legacy mode it behaves
// like the API before PR 4045: it accepts the prompt keys and returns a
// server-side prompt_template. In current mode it 422s on either key and
// returns neither, the way the real API does now.
type fakeNativeBotAPI struct {
	t *testing.T

	mu      sync.Mutex
	current bool
	exists  bool
	mode    string
	writes  int
	refused []string
}

const fakeLegacyPromptTemplate = "legacy server-side soul"

func newFakeNativeBotAPI(t *testing.T) (*fakeNativeBotAPI, *httptest.Server) {
	t.Helper()
	api := &fakeNativeBotAPI{t: t, mode: "off"}
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return api, srv
}

func (a *fakeNativeBotAPI) setCurrent() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.current = true
}

func (a *fakeNativeBotAPI) stats() (writes int, refused []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writes, append([]string(nil), a.refused...)
}

func (a *fakeNativeBotAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	const base = "/v1/orgs/org-1/bots"
	const bot = base + "/native-probe"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == base:
		if !a.applyConfig(w, r) {
			return
		}
		a.exists = true
		a.writeBot(w, http.StatusCreated)
	case r.Method == http.MethodGet && r.URL.Path == bot && a.exists:
		a.writeBot(w, http.StatusOK)
	case r.Method == http.MethodPatch && r.URL.Path == bot && a.exists:
		a.writeBot(w, http.StatusOK)
	case r.Method == http.MethodPatch && r.URL.Path == bot+"/config" && a.exists:
		if !a.applyConfig(w, r) {
			return
		}
		a.writeBot(w, http.StatusOK)
	case r.Method == http.MethodDelete && r.URL.Path == bot && a.exists:
		a.exists = false
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"not found"}`))
	}
}

// applyConfig validates and applies the native config patch in the request.
// It reports false after writing a 422 when the current-mode API would refuse
// the body.
func (a *fakeNativeBotAPI) applyConfig(w http.ResponseWriter, r *http.Request) bool {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		a.t.Errorf("read body: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		a.t.Errorf("decode body %s: %v", raw, err)
	}
	var cfg struct {
		ToolSearch *struct {
			Mode *string `json:"mode"`
		} `json:"tool_search"`
	}
	var keys map[string]json.RawMessage
	if c := body["config"]; len(c) > 0 {
		if err := json.Unmarshal(c, &keys); err != nil {
			a.t.Errorf("decode config %s: %v", c, err)
		}
		if err := json.Unmarshal(c, &cfg); err != nil {
			a.t.Errorf("decode config %s: %v", c, err)
		}
	}
	a.writes++
	if a.current {
		for _, k := range []string{"prompt_template", "prompt_ref"} {
			if _, sent := keys[k]; sent {
				a.refused = append(a.refused, fmt.Sprintf("%s %s sent %q", r.Method, r.URL.Path, k))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = fmt.Fprintf(w, `{"detail":"Request validation failed","errors":[{"pointer":"/config/NativeConfigPatch/%s","detail":"Extra inputs are not permitted","type":"extra_forbidden"}]}`, k)
				return false
			}
		}
	}
	if cfg.ToolSearch != nil && cfg.ToolSearch.Mode != nil {
		a.mode = *cfg.ToolSearch.Mode
	}
	return true
}

type fakeToolSearch struct {
	Mode                 string `json:"mode"`
	AutoThresholdPercent int    `json:"auto_threshold_percent"`
}

type fakeModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type fakeNativeDesiredConfig struct {
	BotType        string         `json:"bot_type"`
	Model          fakeModelRef   `json:"model"`
	ToolSearch     fakeToolSearch `json:"tool_search"`
	PromptTemplate *string        `json:"prompt_template,omitempty"`
}

type fakeNativeBot struct {
	ID                   string                  `json:"id"`
	Slug                 string                  `json:"slug"`
	OrgID                string                  `json:"org_id"`
	ClusterID            string                  `json:"cluster_id"`
	Name                 string                  `json:"name"`
	Namespace            string                  `json:"namespace"`
	Tier                 string                  `json:"tier"`
	RuntimeClass         string                  `json:"runtime_class"`
	StorageClass         string                  `json:"storage_class"`
	RuntimePrivilegeMode string                  `json:"runtime_privilege_mode"`
	OnboardingState      string                  `json:"onboarding_state"`
	HealthStatus         string                  `json:"health_status"`
	DesiredState         string                  `json:"desired_state"`
	ConfigGeneration     int                     `json:"config_generation"`
	HostingType          string                  `json:"hosting_type"`
	Harness              string                  `json:"harness"`
	CreatedAt            string                  `json:"created_at"`
	UpdatedAt            string                  `json:"updated_at"`
	DesiredConfig        fakeNativeDesiredConfig `json:"desired_config"`
}

func (a *fakeNativeBotAPI) writeBot(w http.ResponseWriter, status int) {
	dc := fakeNativeDesiredConfig{
		BotType:    "native",
		Model:      fakeModelRef{Provider: "botyard", Model: "m"},
		ToolSearch: fakeToolSearch{Mode: a.mode, AutoThresholdPercent: 40},
	}
	if !a.current {
		legacy := fakeLegacyPromptTemplate
		dc.PromptTemplate = &legacy
	}
	b, err := json.Marshal(fakeNativeBot{
		ID: "b-1", Slug: "native-probe", OrgID: "org-1", ClusterID: "c-1",
		Name: "native-probe", Namespace: "bot-native-probe", Tier: "starter",
		RuntimeClass: "kata_qemu", StorageClass: "cluster_default",
		RuntimePrivilegeMode: "standard", OnboardingState: "complete",
		HealthStatus: "healthy", DesiredState: "running", ConfigGeneration: 1,
		HostingType: "hosted", Harness: "botyard_native",
		CreatedAt: "2026-10-24T10:00:00Z", UpdatedAt: "2026-10-24T10:00:00Z",
		DesiredConfig: dc,
	})
	if err != nil {
		a.t.Errorf("encode bot: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func nativePromptTestConfig(endpoint, nativeBody string) string {
	return fmt.Sprintf(`
terraform {
  required_providers {
    botyard = { source = "Botyard-AI/botyard" }
  }
}

provider "botyard" {
  endpoint = %q
  api_key  = "byk_test"
  org_id   = "org-1"
}

resource "botyard_bot" "native" {
  name         = "native-probe"
  hosting_type = "hosted"
  harness      = "botyard_native"
  native_config = {
    %s
  }
}
`, endpoint, nativeBody)
}

// TestNativePromptRemoved_UpgradeFromV050 drives real Terraform core through
// the upgrade a customer makes:
//
//  1. v0.5.0 (from the Registry) against a pre-PR-4045 API writes state whose
//     native_config.prompt_template holds the server's old template;
//  2. the API drops the fields; this provider plans that state as a no-op;
//  3. a real update applies with no prompt key on the wire (the fake 422s like
//     the API if one is sent);
//  4. setting either removed attribute fails at plan time.
//
// Step 1 downloads v0.5.0 from the Registry, as plugin-testing does for any
// ExternalProviders step; the namespace override makes this build answer to the
// same provider address so the state carries over.
func TestNativePromptRemoved_UpgradeFromV050(t *testing.T) {
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", "botyard-ai")
	api, srv := newFakeNativeBotAPI(t)
	auto := nativePromptTestConfig(srv.URL, `tool_search = { mode = "auto" }`)
	always := nativePromptTestConfig(srv.URL, `tool_search = { mode = "always" }`)
	const addr = "botyard_bot.native"

	resource.UnitTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				ExternalProviders: map[string]resource.ExternalProvider{
					"botyard": {Source: "Botyard-AI/botyard", VersionConstraint: "0.5.0"},
				},
				Config: auto,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "native_config.prompt_template", fakeLegacyPromptTemplate),
					resource.TestCheckResourceAttr(addr, "native_config.tool_search.mode", "auto"),
				),
			},
			{
				PreConfig:                api.setCurrent,
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Config:                   auto,
				PlanOnly:                 true,
			},
			{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Config:                   always,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "native_config.tool_search.mode", "always"),
					resource.TestCheckNoResourceAttr(addr, "native_config.prompt_template"),
					resource.TestCheckNoResourceAttr(addr, "native_config.prompt_ref"),
					func(*terraform.State) error {
						if _, refused := api.stats(); len(refused) > 0 {
							return fmt.Errorf("provider sent removed prompt keys: %s", strings.Join(refused, "; "))
						}
						return nil
					},
				),
			},
			{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Config:                   nativePromptTestConfig(srv.URL, `prompt_template = "hi"`),
				PlanOnly:                 true,
				ExpectError:              regexp.MustCompile(`Removed by the Botyard API`),
			},
			{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Config:                   nativePromptTestConfig(srv.URL, `prompt_ref = "prompts/x@1"`),
				PlanOnly:                 true,
				ExpectError:              regexp.MustCompile(`Removed by the Botyard API`),
			},
			// Back to a valid config, so the post-test destroy can plan.
			{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Config:                   always,
				PlanOnly:                 true,
			},
		},
	})

	if writes, refused := api.stats(); len(refused) > 0 || writes < 2 {
		t.Fatalf("config writes = %d (want the v0.5.0 create plus one update), refused = %v", writes, refused)
	}
}
