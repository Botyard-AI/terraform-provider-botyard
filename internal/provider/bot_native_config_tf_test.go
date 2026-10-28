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
	// openclaw serves the bot as harness "openclaw" with an OpenClaw
	// desired_config instead of a native one.
	openclaw bool
	thinking string
	mode     string
	images   map[string]json.RawMessage
	writes   int
	refused  []string
}

const fakeLegacyPromptTemplate = "legacy server-side soul"

func newFakeNativeBotAPI(t *testing.T) (*fakeNativeBotAPI, *httptest.Server) {
	t.Helper()
	api := &fakeNativeBotAPI{t: t, mode: "off", images: map[string]json.RawMessage{}}
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return api, srv
}

// newFakeOpenClawBotAPI is the same fake serving an OpenClaw bot. It is always
// in current mode: the native prompt keys do not exist on this shape.
func newFakeOpenClawBotAPI(t *testing.T) (*fakeNativeBotAPI, *httptest.Server) {
	t.Helper()
	api, srv := newFakeNativeBotAPI(t)
	api.openclaw = true
	api.current = true
	api.mode = "auto"
	api.thinking = "medium"
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
		// Patch semantics of the real ToolImageConfigPatch: an omitted key
		// is unchanged, an explicit null resets it to the platform default.
		ToolImages map[string]json.RawMessage `json:"tool_images"`
		Thinking   *string                    `json:"thinking_default"`
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
	if a.openclaw {
		// OpenClawConfigPatch has no `model` since the API removed it; a
		// provider that still sends one is talking to a field that does
		// nothing. Record it the same way as a refused prompt key.
		if _, sent := keys["model"]; sent {
			a.refused = append(a.refused, fmt.Sprintf("%s %s sent %q", r.Method, r.URL.Path, "model"))
		}
		if cfg.Thinking != nil {
			a.thinking = *cfg.Thinking
		}
	}
	for k, v := range cfg.ToolImages {
		if string(v) == "null" {
			delete(a.images, k)
		} else {
			a.images[k] = v
		}
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
	BotType        string                     `json:"bot_type"`
	Model          fakeModelRef               `json:"model"`
	ToolSearch     fakeToolSearch             `json:"tool_search"`
	ToolImages     map[string]json.RawMessage `json:"tool_images"`
	PromptTemplate *string                    `json:"prompt_template,omitempty"`
}

type fakeNativeBot struct {
	ID                   string          `json:"id"`
	Slug                 string          `json:"slug"`
	OrgID                string          `json:"org_id"`
	ClusterID            string          `json:"cluster_id"`
	Name                 string          `json:"name"`
	Namespace            string          `json:"namespace"`
	Tier                 string          `json:"tier"`
	RuntimeClass         string          `json:"runtime_class"`
	StorageClass         string          `json:"storage_class"`
	RuntimePrivilegeMode string          `json:"runtime_privilege_mode"`
	OnboardingState      string          `json:"onboarding_state"`
	HealthStatus         string          `json:"health_status"`
	DesiredState         string          `json:"desired_state"`
	ConfigGeneration     int             `json:"config_generation"`
	HostingType          string          `json:"hosting_type"`
	Harness              string          `json:"harness"`
	CreatedAt            string          `json:"created_at"`
	UpdatedAt            string          `json:"updated_at"`
	DesiredConfig        json.RawMessage `json:"desired_config"`
}

type fakeIdentity struct {
	Emoji *string `json:"emoji"`
	Theme *string `json:"theme"`
}

type fakeOpenClawDesiredConfig struct {
	GatewayToken    string         `json:"gateway_token"`
	Identity        fakeIdentity   `json:"identity"`
	ThinkingDefault string         `json:"thinking_default"`
	ToolSearch      fakeToolSearch `json:"tool_search"`
}

func (a *fakeNativeBotAPI) writeBot(w http.ResponseWriter, status int) {
	dc := fakeNativeDesiredConfig{
		BotType:    "native",
		Model:      fakeModelRef{Provider: "botyard", Model: "m"},
		ToolSearch: fakeToolSearch{Mode: a.mode, AutoThresholdPercent: 40},
		ToolImages: map[string]json.RawMessage{"enabled": nil, "max_dimension_px": nil, "default_detail": nil},
	}
	for k, v := range a.images {
		dc.ToolImages[k] = v
	}
	if !a.current {
		legacy := fakeLegacyPromptTemplate
		dc.PromptTemplate = &legacy
	}
	harness := "botyard_native"
	dcRaw, err := json.Marshal(dc)
	if a.openclaw {
		harness = "openclaw"
		dcRaw, err = json.Marshal(fakeOpenClawDesiredConfig{
			GatewayToken:    "gw",
			ThinkingDefault: a.thinking,
			ToolSearch:      fakeToolSearch{Mode: a.mode, AutoThresholdPercent: 10},
		})
	}
	if err != nil {
		a.t.Errorf("encode desired_config: %v", err)
	}
	b, err := json.Marshal(fakeNativeBot{
		ID: "b-1", Slug: "native-probe", OrgID: "org-1", ClusterID: "c-1",
		Name: "native-probe", Namespace: "bot-native-probe", Tier: "starter",
		RuntimeClass: "kata_qemu", StorageClass: "cluster_default",
		RuntimePrivilegeMode: "standard", OnboardingState: "complete",
		HealthStatus: "healthy", DesiredState: "running", ConfigGeneration: 1,
		HostingType: "hosted", Harness: harness,
		CreatedAt: "2026-10-24T10:00:00Z", UpdatedAt: "2026-10-24T10:00:00Z",
		DesiredConfig: dcRaw,
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

func openClawTestConfig(endpoint, configBody string) string {
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
  config = {
    %s
  }
}
`, endpoint, configBody)
}

// TestNativePromptRemoved_UpgradeFromV050 drives real Terraform core through
// the upgrade a customer makes from v0.5.0, the last version that could hold a
// NON-null prompt_template in state:
//
//  1. v0.5.0 (from the Registry) against a pre-PR-4045 API writes state whose
//     native_config.prompt_template holds the server's old template;
//  2. the API drops the fields; this build, which has no prompt attributes at
//     all, loads that state and plans it as a no-op;
//  3. a real update applies with no prompt key on the wire (the fake 422s like
//     the API if one is sent);
//  4. a config that still sets either removed attribute plans as a no-op: the
//     key is silently dropped by Terraform core, never reaching the provider.
//
// Step 4 pins a regression in practitioner signal that v0.7.0 accepts by
// design (decision D3). v0.5.1 and v0.6.0 refused these attributes at plan
// time; v0.7.0 cannot, because `native_config` is a nested attribute written
// in object syntax, and core converts an object literal to the schema type by
// discarding keys the type does not declare before calling the provider. The
// upgrade notes say so. Nothing is ever sent, so the API cannot 422 either.
//
// ExternalProviders steps download from the Registry; the namespace override
// makes this build answer to the same provider address so state carries over.
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
				Config: nativePromptTestConfig(srv.URL,
					"tool_search = { mode = \"always\" }\n    prompt_template = \"hi\""),
				PlanOnly: true,
			},
			{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Config: nativePromptTestConfig(srv.URL,
					"tool_search = { mode = \"always\" }\n    prompt_ref = \"prompts/x@1\""),
				PlanOnly: true,
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

// TestNativePromptRemoved_UpgradeFromV060 is decision D3's literal case: state
// written by v0.6.0, where both prompt attributes exist in the schema and are
// null, loads in this build and plans as a no-op.
//
// The no-change apply step is the documented upgrade step, not decoration.
// Until state is rewritten by this build, it still carries the removed keys,
// and `terraform show -json` (which decodes raw state against the current
// schema WITHOUT the provider's upgrade pass) fails with
// `unsupported attribute "prompt_ref"`. plan and apply are unaffected because
// core upgrades state through the provider first. Without the step, this
// test's own post-run state read fails exactly that way.
func TestNativePromptRemoved_UpgradeFromV060(t *testing.T) {
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", "botyard-ai")
	api, srv := newFakeNativeBotAPI(t)
	api.setCurrent()
	auto := nativePromptTestConfig(srv.URL, `tool_search = { mode = "auto" }`)
	const addr = "botyard_bot.native"

	resource.UnitTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				ExternalProviders: map[string]resource.ExternalProvider{
					"botyard": {Source: "Botyard-AI/botyard", VersionConstraint: "0.6.0"},
				},
				Config: auto,
				Check: resource.ComposeAggregateTestCheckFunc(
					// Present in v0.6.0's schema, and null.
					resource.TestCheckNoResourceAttr(addr, "native_config.prompt_template"),
					resource.TestCheckNoResourceAttr(addr, "native_config.prompt_ref"),
					resource.TestCheckResourceAttr(addr, "native_config.tool_search.mode", "auto"),
				),
			},
			{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Config:                   auto,
				PlanOnly:                 true,
			},
			{
				// A no-change apply. It writes state through this build, which is
				// what clears the stale keys; the harness reads state with
				// `terraform show -json` right after, so this step fails if it did
				// not.
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Config:                   auto,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(addr, "native_config.prompt_template"),
					resource.TestCheckResourceAttr(addr, "native_config.tool_search.mode", "auto"),
				),
			},
			{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Config:                   auto,
				PlanOnly:                 true,
			},
		},
	})

	if writes, refused := api.stats(); len(refused) > 0 || writes != 1 {
		t.Fatalf("config writes = %d (want only the v0.6.0 create), refused = %v", writes, refused)
	}
}

// TestNativeToolImages_RoundTrip drives real Terraform core through the
// tool_images ownership rule: a declared block owns all three leaves, so
// dropping a leaf resets it to the platform default (null) and every apply is
// followed by a clean re-plan.
func TestNativeToolImages_RoundTrip(t *testing.T) {
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", "botyard-ai")
	api, srv := newFakeNativeBotAPI(t)
	api.setCurrent()
	const addr = "botyard_bot.native"
	full := nativePromptTestConfig(srv.URL,
		`tool_images = { enabled = false, max_dimension_px = 1024, default_detail = "low" }`)
	partial := nativePromptTestConfig(srv.URL, `tool_images = { max_dimension_px = 4000 }`)
	empty := nativePromptTestConfig(srv.URL, `tool_images = {}`)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: full,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "native_config.tool_images.enabled", "false"),
					resource.TestCheckResourceAttr(addr, "native_config.tool_images.max_dimension_px", "1024"),
					resource.TestCheckResourceAttr(addr, "native_config.tool_images.default_detail", "low"),
				),
			},
			{
				Config: partial,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(addr, "native_config.tool_images.enabled"),
					resource.TestCheckResourceAttr(addr, "native_config.tool_images.max_dimension_px", "4000"),
					resource.TestCheckNoResourceAttr(addr, "native_config.tool_images.default_detail"),
					func(*terraform.State) error {
						api.mu.Lock()
						defer api.mu.Unlock()
						if len(api.images) != 1 {
							return fmt.Errorf("server still holds %v; dropped leaves must reset", api.images)
						}
						return nil
					},
				),
			},
			{
				Config: empty,
				Check: func(*terraform.State) error {
					api.mu.Lock()
					defer api.mu.Unlock()
					if len(api.images) != 0 {
						return fmt.Errorf("server still holds %v after tool_images = {}", api.images)
					}
					return nil
				},
			},
			{
				Config:      nativePromptTestConfig(srv.URL, `tool_images = { max_dimension_px = 9000 }`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Invalid Attribute Value`),
			},
			{
				Config:      nativePromptTestConfig(srv.URL, `tool_images = { default_detail = "medium" }`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Invalid Attribute Value`),
			},
			{Config: empty, PlanOnly: true},
		},
	})
}

// TestConfigModelRemoved_UpgradeFromV060 is the `config.model` half of the
// v0.7.0 removals, through real Terraform core: v0.6.0 state that holds a
// declared `config.model.primary` loads and plans as a no-op once the block is
// deleted, a leftover block is silently dropped by core, and nothing sends
// `model` on the wire.
func TestConfigModelRemoved_UpgradeFromV060(t *testing.T) {
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", "botyard-ai")
	api, srv := newFakeOpenClawBotAPI(t)
	const addr = "botyard_bot.native"
	withModel := openClawTestConfig(srv.URL,
		"thinking_default = \"high\"\n    model = { primary = { provider = \"botyard\", model = \"gpt-5.4\" } }")
	without := openClawTestConfig(srv.URL, `thinking_default = "high"`)
	// `provider` is set because v0.6.0 itself cannot apply a model block
	// without it: the Optional+Computed leaf is never refreshed, stays
	// unknown, and core rejects the result ("Provider returned invalid result
	// object after apply"). So any working v0.6.0 state looks like this.

	resource.UnitTest(t, resource.TestCase{
		Steps: []resource.TestStep{
			{
				ExternalProviders: map[string]resource.ExternalProvider{
					"botyard": {Source: "Botyard-AI/botyard", VersionConstraint: "0.6.0"},
				},
				Config: withModel,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "config.model.primary.model", "gpt-5.4"),
					resource.TestCheckResourceAttr(addr, "config.thinking_default", "high"),
				),
			},
			{ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, Config: without, PlanOnly: true},
			// Leftover block: dropped by core before the provider sees it.
			{ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, Config: withModel, PlanOnly: true},
			{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Config:                   without,
				Check:                    resource.TestCheckNoResourceAttr(addr, "config.model.primary.model"),
			},
		},
	})

	// v0.6.0 sent `model` on create; that is the old provider, and the point
	// of the removal. This build must not have sent it again.
	if writes, refused := api.stats(); writes != 1 || len(refused) != 1 {
		t.Fatalf("config writes = %d (want only the v0.6.0 create), model sends = %v (want only v0.6.0's)", writes, refused)
	}
}

// TestConfigToolSearch_RoundTrip: `config.tool_search` on an OpenClaw bot
// applies, absorbs the server's merged threshold, and re-plans clean.
func TestConfigToolSearch_RoundTrip(t *testing.T) {
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", "botyard-ai")
	_, srv := newFakeOpenClawBotAPI(t)
	const addr = "botyard_bot.native"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: openClawTestConfig(srv.URL, `tool_search = { mode = "always" }`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "config.tool_search.mode", "always"),
					resource.TestCheckResourceAttr(addr, "config.tool_search.auto_threshold_percent", "10"),
				),
			},
			{Config: openClawTestConfig(srv.URL, `tool_search = { mode = "always" }`), PlanOnly: true},
			{
				Config: openClawTestConfig(srv.URL, `tool_search = { mode = "off" }`),
				Check:  resource.TestCheckResourceAttr(addr, "config.tool_search.mode", "off"),
			},
			{
				Config:      openClawTestConfig(srv.URL, `tool_search = { mode = "sometimes" }`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Invalid Attribute Value`),
			},
			{Config: openClawTestConfig(srv.URL, `tool_search = { mode = "off" }`), PlanOnly: true},
		},
	})
}

// TestConfigBlocks_AddedToExistingBot adds nested config blocks to a bot that
// already exists. Their Optional+Computed leaves must plan as unknown, not as
// the null copied from the absent parent: the server fills them in, and a
// planned null is then an "inconsistent result after apply". Found live on
// 2026-10-28 adding config.tool_search to an existing OpenClaw bot. The
// native and protocol-level cases are in bot_nested_block_added_test.go.
func TestConfigBlocks_AddedToExistingBot(t *testing.T) {
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", "botyard-ai")
	_, oc := newFakeOpenClawBotAPI(t)
	const addr = "botyard_bot.native"

	t.Run("openclaw config.tool_search", func(t *testing.T) {
		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{
				{Config: openClawTestConfig(oc.URL, `thinking_default = "high"`)},
				{
					Config: openClawTestConfig(oc.URL, "thinking_default = \"high\"\n    tool_search = { mode = \"always\" }"),
					Check:  resource.TestCheckResourceAttr(addr, "config.tool_search.auto_threshold_percent", "10"),
				},
				{Config: openClawTestConfig(oc.URL, "thinking_default = \"high\"\n    tool_search = { mode = \"always\" }"), PlanOnly: true},
			},
		})
	})
}
