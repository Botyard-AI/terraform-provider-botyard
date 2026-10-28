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

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// PlatDev #4383: the `pack` block on botyard_bot, driven through real
// Terraform core against fakePackBotAPI.

const (
	fpBotID     = "11111111-1111-4111-8111-111111111111"
	fpInstallID = "22222222-2222-4222-8222-222222222222"
	fpPV1       = "33333333-3333-4333-8333-333333333331"
	fpPV2       = "33333333-3333-4333-8333-333333333332"
	fpConnCRM   = "44444444-4444-4444-8444-444444444441" // credential_form, metered
	fpConnMail  = "44444444-4444-4444-8444-444444444442" // oauth
	fpServerID  = "55555555-5555-4555-8555-555555555555"
	fpSetupID   = "66666666-6666-4666-8666-666666666666"
	fpPackAvURL = "https://example.test/pack-avatar.png"
)

// fakePackBotAPI models one native bot plus the packs routes P2 uses.
type fakePackBotAPI struct {
	t *testing.T

	mu         sync.Mutex
	exists     bool
	installed  bool
	foreign    bool // the bot was not created by this key: uninstall is 403
	mode       string
	avatar     string
	calls      []string
	fromPack   []byte
	configPats [][]byte
}

func newFakePackBotAPI(t *testing.T) (*fakePackBotAPI, *httptest.Server) {
	t.Helper()
	api := &fakePackBotAPI{t: t, mode: "off"}
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return api, srv
}

func (a *fakePackBotAPI) set(f func(a *fakePackBotAPI)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(a)
}

func (a *fakePackBotAPI) callLog() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func (a *fakePackBotAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, r.Method+" "+r.URL.Path)
	raw, _ := io.ReadAll(r.Body)

	const base = "/v1/orgs/org-1"
	const bot = base + "/bots/pack-probe"
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == base+"/bots/from-pack":
		a.fromPack = raw
		a.exists, a.installed = true, true
		var body struct {
			Bots []struct {
				Avatar *struct {
					AvatarURL string `json:"avatar_url"`
				} `json:"avatar"`
			} `json:"bots"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || len(body.Bots) != 1 {
			a.t.Errorf("from-pack body %s: %v", raw, err)
		}
		a.avatar = fpPackAvURL
		if av := body.Bots[0].Avatar; av != nil {
			a.avatar = av.AvatarURL
		}
		a.mode = "auto" // the pack's tool_search setting
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"bots":[{"bot":%s,"install":{"bot_pack_install_id":%q,"plan":{}},"model":{"kind":"default"}}],"setups":[]}`,
			a.botJSON(), fpInstallID)
	case r.Method == http.MethodPost && r.URL.Path == base+"/bots":
		a.exists, a.avatar = true, ""
		a.applyToolSearch(raw)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(a.botJSON()))
	case r.Method == http.MethodGet && r.URL.Path == bot && a.exists:
		_, _ = w.Write([]byte(a.botJSON()))
	case r.Method == http.MethodPatch && r.URL.Path == bot && a.exists:
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		if v, ok := body["avatar_url"]; ok {
			var s *string
			_ = json.Unmarshal(v, &s)
			a.avatar = ""
			if s != nil {
				a.avatar = *s
			}
		}
		_, _ = w.Write([]byte(a.botJSON()))
	case r.Method == http.MethodPatch && r.URL.Path == bot+"/config" && a.exists:
		a.configPats = append(a.configPats, raw)
		a.applyToolSearch(raw)
		_, _ = w.Write([]byte(a.botJSON()))
	case r.Method == http.MethodDelete && r.URL.Path == bot && a.exists:
		a.exists, a.installed = false, false
		w.Header().Del("Content-Type")
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == base+"/packs/installs":
		if got := r.URL.Query().Get("bot_ids"); got != fpBotID {
			a.t.Errorf("installs read bot_ids=%q, want %q", got, fpBotID)
		}
		installs := ""
		if a.installed {
			installs = a.installJSON()
		}
		_, _ = fmt.Fprintf(w, `{"bots":[{"bot_id":%q,"installs":[%s]}],"not_found":[]}`, fpBotID, installs)
	case r.Method == http.MethodDelete && r.URL.Path == base+"/packs/installs":
		if got := r.URL.Query().Get("bot_pack_install_ids"); got != fpInstallID {
			a.t.Errorf("uninstall ids=%q, want %q", got, fpInstallID)
		}
		if a.foreign {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Forbidden","status":403,"detail":"Only the bot's owner can uninstall its packs."}`))
			return
		}
		a.installed, a.avatar = false, ""
		_, _ = w.Write([]byte(`{"uninstalls":[]}`))
	case r.Method == http.MethodPost && r.URL.Path == base+"/packs/connections/preview":
		_, _ = fmt.Fprintf(w, `{"versions":[{"pack_version_id":%q,"connections":[%s,%s,%s]}],"not_found":[]}`, fpPV1,
			previewMcp(fpConnCRM, "credential_form", true), previewMcp(fpConnMail, "oauth", false),
			`{"kind":"vault_secret","pack_version_connection_id":"v-1","key_path":"k","required":true,"purpose":null}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"not found"}`))
	}
}

// applyToolSearch applies `config.tool_search.mode` from a create or config
// PATCH body.
func (a *fakePackBotAPI) applyToolSearch(raw []byte) {
	var body struct {
		Config struct {
			ToolSearch *struct {
				Mode string `json:"mode"`
			} `json:"tool_search"`
		} `json:"config"`
	}
	_ = json.Unmarshal(raw, &body)
	if body.Config.ToolSearch != nil {
		a.mode = body.Config.ToolSearch.Mode
	}
}

func previewMcp(id, setupKind string, metered bool) string {
	return fmt.Sprintf(`{"kind":"mcp_catalog_entry","pack_version_connection_id":%q,"required":true,"purpose":null,`+
		`"mcp_catalog_entry_id":"e-1","setup":{"kind":%q},"metered":%t,"candidates":[],"more_candidates_count":0}`,
		id, setupKind, metered)
}

func (a *fakePackBotAPI) installJSON() string {
	return fmt.Sprintf(`{"bot_pack_install_id":%q,"connections":[],"installed_at":"2026-10-28T10:00:00Z",`+
		`"installed_by_bot_id":null,"installed_by_user_id":"u-1","items":[],"name":"Support","pack_id":"pk-support",`+
		`"pack_slug":"support","pack_version_id":%q,"pending_connections":[{"pack_version_connection_id":%q,`+
		`"bot_pack_connection_setup_id":%q,"state":"needs_input"}],"updated_at":"2026-10-28T10:00:00Z","version_no":3}`,
		fpInstallID, fpPV1, fpConnCRM, fpSetupID)
}

func (a *fakePackBotAPI) botJSON() string {
	avatar := "null"
	if a.avatar != "" {
		avatar = fmt.Sprintf("%q", a.avatar)
	}
	return fmt.Sprintf(`{"id":%q,"slug":"pack-probe","org_id":"org-1","cluster_id":"c-1","name":"pack-probe",`+
		`"namespace":"bot-pack-probe","tier":"starter","runtime_class":"kata_qemu","storage_class":"cluster_default",`+
		`"runtime_privilege_mode":"standard","onboarding_state":"complete","health_status":"healthy",`+
		`"desired_state":"running","config_generation":1,"hosting_type":"hosted","harness":"botyard_native",`+
		`"avatar_url":%s,"created_at":"2026-10-28T10:00:00Z","updated_at":"2026-10-28T10:00:00Z",`+
		`"desired_config":{"bot_type":"native","model":{"provider":"botyard","model":"m"},`+
		`"tool_search":{"mode":%q,"auto_threshold_percent":40}}}`, fpBotID, avatar, a.mode)
}

func packBotConfig(endpoint, extra string) string {
	return fmt.Sprintf(`
provider "botyard" {
  endpoint = %q
  api_key  = "byk_test"
  org_id   = "org-1"
}

resource "botyard_bot" "p" {
  name    = "pack-probe"
  harness = "botyard_native"
  native_config = {
    tool_search = { mode = "auto" }
  }
%s
}
`, endpoint, extra)
}

func packBlock(pv, connections string) string {
	return fmt.Sprintf(`
  pack = {
    pack_version_id = %q
    settings = [{ path = "tool_search", choice = "pack" }]
    connections = [%s]
  }`, pv, connections)
}

const useServerConn = `{ pack_version_connection_id = "` + fpConnCRM + `", choice = "use_server", mcp_server_id = "` + fpServerID + `" }`

const addr = "botyard_bot.p"

// Create → no-op plan → import → remove the block (uninstall, bot kept).
func TestBotPack_Lifecycle(t *testing.T) {
	api, srv := newFakePackBotAPI(t)
	withPack := packBotConfig(srv.URL, packBlock(fpPV1, useServerConn))
	withoutPack := packBotConfig(srv.URL, "")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: withPack,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "id", fpBotID),
					resource.TestCheckResourceAttr(addr, "pack.pack_version_id", fpPV1),
					resource.TestCheckResourceAttr(addr, "pack.bot_pack_install_id", fpInstallID),
					resource.TestCheckResourceAttr(addr, "pack.pack_slug", "support"),
					resource.TestCheckResourceAttr(addr, "pack.version_no", "3"),
					resource.TestCheckResourceAttr(addr, "pack.pending_connections.#", "1"),
					resource.TestCheckResourceAttr(addr, "pack.pending_connections.0.state", "needs_input"),
					// The pack's avatar stays the pack's: avatar_url was left unset.
					resource.TestCheckNoResourceAttr(addr, "avatar_url"),
					resource.TestCheckResourceAttr(addr, "native_config.tool_search.mode", "auto"),
					func(*terraform.State) error {
						a := api
						a.mu.Lock()
						defer a.mu.Unlock()
						if !strings.Contains(string(a.fromPack), `"choice":"use_server"`) ||
							!strings.Contains(string(a.fromPack), fpServerID) {
							return fmt.Errorf("from-pack body lacks the use_server choice: %s", a.fromPack)
						}
						if strings.Contains(string(a.fromPack), `"avatar"`) {
							return fmt.Errorf("from-pack sent an avatar though avatar_url is unset: %s", a.fromPack)
						}
						if len(a.configPats) != 1 {
							return fmt.Errorf("config PATCHes = %d, want exactly 1 after from-pack", len(a.configPats))
						}
						return nil
					},
				),
			},
			{
				// The pack already set tool_search = auto: the sparse config
				// patch converges, so the next plan is empty.
				Config: withPack,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
			},
			{
				ResourceName:      addr,
				ImportState:       true,
				ImportStateId:     "pack-probe",
				ImportStateVerify: true,
				// Create-time choices are not readable back; everything else is.
				ImportStateVerifyIgnore:              []string{"pack.settings", "pack.connections", "native_config"},
				ImportStateVerifyIdentifierAttribute: "slug",
			},
			{
				Config: withoutPack,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "id", fpBotID), // updated in place, not replaced
					resource.TestCheckNoResourceAttr(addr, "pack.pack_version_id"),
					func(*terraform.State) error {
						for _, c := range api.callLog() {
							if c == "DELETE /v1/orgs/org-1/bots/pack-probe" {
								return fmt.Errorf("the bot was deleted; removing `pack` must only uninstall")
							}
						}
						for _, c := range api.callLog() {
							if c == "DELETE /v1/orgs/org-1/packs/installs" {
								return nil
							}
						}
						return fmt.Errorf("no uninstall call in %v", api.callLog())
					},
				),
			},
		},
	})
}

func TestBotPack_ChangingVersionIsAPlanError(t *testing.T) {
	_, srv := newFakePackBotAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: packBotConfig(srv.URL, packBlock(fpPV1, ""))},
			{
				Config:      packBotConfig(srv.URL, packBlock(fpPV2, "")),
				ExpectError: regexp.MustCompile(`Cannot change an installed pack version`),
			},
		},
	})
}

func TestBotPack_AddingToExistingBotIsAPlanError(t *testing.T) {
	_, srv := newFakePackBotAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: packBotConfig(srv.URL, "")},
			{
				Config:      packBotConfig(srv.URL, packBlock(fpPV1, "")),
				ExpectError: regexp.MustCompile(`Cannot add a pack to an existing bot`),
			},
		},
	})
}

// An install removed outside Terraform reads back as no pack, and the block
// still in config is then refused rather than silently re-created.
func TestBotPack_RemoteUninstall(t *testing.T) {
	api, srv := newFakePackBotAPI(t)
	cfg := packBotConfig(srv.URL, packBlock(fpPV1, ""))
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig:   func() { api.set(func(a *fakePackBotAPI) { a.installed = false }) },
				Config:      cfg,
				ExpectError: regexp.MustCompile(`Cannot add a pack to an existing bot`),
			},
		},
	})
}

func TestBotPack_UnownedBotUninstallIs403Diagnostic(t *testing.T) {
	api, srv := newFakePackBotAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: packBotConfig(srv.URL, packBlock(fpPV1, ""))},
			{
				PreConfig:   func() { api.set(func(a *fakePackBotAPI) { a.foreign = true }) },
				Config:      packBotConfig(srv.URL, ""),
				ExpectError: regexp.MustCompile(`(?s)Not allowed to uninstall this pack.*owned by the`),
			},
			{
				// Let the test's destroy succeed.
				PreConfig: func() { api.set(func(a *fakePackBotAPI) { a.foreign = false }) },
				Config:    packBotConfig(srv.URL, ""),
			},
		},
	})
}

// J-C card 3/3 (refuse_plan_time): an OAuth connection cannot be `create`d.
func TestBotPack_OAuthCreateIsAPlanError(t *testing.T) {
	api, srv := newFakePackBotAPI(t)
	conn := `{ pack_version_connection_id = "` + fpConnMail + `", choice = "create", create = { setup = { kind = "none" } } }`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      packBotConfig(srv.URL, packBlock(fpPV1, conn)),
			ExpectError: regexp.MustCompile(`(?s)Connection needs a vendor sign-in.*use_server`),
		}},
	})
	for _, c := range api.callLog() {
		if strings.HasSuffix(c, "/bots/from-pack") {
			t.Fatalf("from-pack was called despite the plan-time refusal: %v", api.callLog())
		}
	}
}

func TestBotPack_CreateWithVaultRefsSendsCredentialForm(t *testing.T) {
	api, srv := newFakePackBotAPI(t)
	conn := `{
      pack_version_connection_id = "` + fpConnCRM + `"
      choice = "create"
      create = {
        accept_metered = true
        setup = { kind = "credential_form", vault_entry_refs = { api_key = "` + fpServerID + `" } }
      }
    }`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: packBotConfig(srv.URL, packBlock(fpPV1, conn)),
			Check: func(*terraform.State) error {
				api.mu.Lock()
				defer api.mu.Unlock()
				var body struct {
					Bots []struct {
						Connections []json.RawMessage `json:"connections"`
					} `json:"bots"`
				}
				if err := json.Unmarshal(api.fromPack, &body); err != nil {
					return err
				}
				got := string(body.Bots[0].Connections[0])
				for _, want := range []string{`"choice":"create"`, `"kind":"credential_form"`, `"api_key":"` + fpServerID + `"`, `"accept_metered":true`} {
					if !strings.Contains(got, want) {
						return fmt.Errorf("create choice %s lacks %s", got, want)
					}
				}
				return nil
			},
		}},
	})
}

func TestBotPack_MeteredCreateNeedsAcceptance(t *testing.T) {
	_, srv := newFakePackBotAPI(t)
	conn := `{ pack_version_connection_id = "` + fpConnCRM + `", choice = "create", create = { setup = { kind = "credential_form", vault_entry_refs = { api_key = "x" } } } }`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      packBotConfig(srv.URL, packBlock(fpPV1, conn)),
			ExpectError: regexp.MustCompile(`accept_metered = true`),
		}},
	})
}

func TestValidatePackConfig(t *testing.T) {
	str := types.StringValue
	cases := []struct {
		name string
		pack botPackModel
		want string // substring of the single expected error summary; "" = valid
	}{
		{"valid", botPackModel{
			Settings:    []botPackSettingModel{{Path: str("tool_search"), Choice: str("custom"), Value: str(`{"mode":"auto"}`)}},
			Connections: []botPackConnectionModel{{PackVersionConnectionID: str("c1"), Choice: str("leave_pending"), McpServerID: types.StringNull()}},
		}, ""},
		{"custom without value", botPackModel{
			Settings: []botPackSettingModel{{Path: str("tool_search"), Choice: str("custom"), Value: types.StringNull()}},
		}, "Missing setting value"},
		{"value on pack choice", botPackModel{
			Settings: []botPackSettingModel{{Path: str("tool_search"), Choice: str("pack"), Value: str("1")}},
		}, "Unexpected setting value"},
		{"invalid json", botPackModel{
			Settings: []botPackSettingModel{{Path: str("tool_search"), Choice: str("custom"), Value: str("{")}},
		}, "not JSON"},
		{"duplicate path", botPackModel{
			Settings: []botPackSettingModel{
				{Path: str("tool_search"), Choice: str("pack"), Value: types.StringNull()},
				{Path: str("tool_search"), Choice: str("keep"), Value: types.StringNull()},
			},
		}, "Duplicate pack setting"},
		{"use_server without id", botPackModel{
			Connections: []botPackConnectionModel{{PackVersionConnectionID: str("c1"), Choice: str("use_server"), McpServerID: types.StringNull()}},
		}, "do not match its choice"},
		{"id on leave_pending", botPackModel{
			Connections: []botPackConnectionModel{{PackVersionConnectionID: str("c1"), Choice: str("leave_pending"), McpServerID: str("s")}},
		}, "do not match its choice"},
		{"duplicate connection", botPackModel{
			Connections: []botPackConnectionModel{
				{PackVersionConnectionID: str("c1"), Choice: str("leave_pending"), McpServerID: types.StringNull()},
				{PackVersionConnectionID: str("c1"), Choice: str("leave_pending"), McpServerID: types.StringNull()},
			},
		}, "Duplicate pack connection"},
		{"credential_form without refs", botPackModel{
			Connections: []botPackConnectionModel{{PackVersionConnectionID: str("c1"), Choice: str("create"), McpServerID: types.StringNull(),
				Create: &botPackCreateModel{Setup: botPackCreateSetupModel{Kind: str("credential_form")}}}},
		}, "Missing vault references"},
		{"refs on none", botPackModel{
			Connections: []botPackConnectionModel{{PackVersionConnectionID: str("c1"), Choice: str("create"), McpServerID: types.StringNull(),
				Create: &botPackCreateModel{Setup: botPackCreateSetupModel{Kind: str("none"), VaultEntryRefs: map[string]types.String{"k": str("v")}}}}},
		}, "Setup fields do not match its kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var diags diag.Diagnostics
			validatePackConfig(&c.pack, &diags)
			if c.want == "" {
				if diags.HasError() {
					t.Fatalf("unexpected errors: %v", diags)
				}
				return
			}
			if diags.ErrorsCount() != 1 || !strings.Contains(diags.Errors()[0].Summary(), c.want) {
				t.Fatalf("errors = %v, want one containing %q", diags, c.want)
			}
		})
	}
}

func TestCreateChoiceDiagnostics(t *testing.T) {
	at := path.Root("pack").AtName("connections").AtListIndex(0)
	conn := func(kind string, accept bool) botPackConnectionModel {
		return botPackConnectionModel{Choice: types.StringValue("create"), Create: &botPackCreateModel{
			AcceptMetered: types.BoolValue(accept), Setup: botPackCreateSetupModel{Kind: types.StringValue(kind)},
		}}
	}
	cases := []struct {
		name string
		c    botPackConnectionModel
		pv   connectionPreview
		want string
	}{
		{"ok", conn("none", false), connectionPreview{setupKind: "none"}, ""},
		{"oauth", conn("none", false), connectionPreview{setupKind: "oauth"}, "vendor sign-in"},
		{"unavailable", conn("none", false), connectionPreview{setupKind: "unavailable"}, "cannot be created"},
		{"kind mismatch", conn("none", false), connectionPreview{setupKind: "setup_form"}, "Wrong setup kind"},
		{"metered", conn("none", false), connectionPreview{setupKind: "none", metered: true}, "Metered"},
		{"metered accepted", conn("none", true), connectionPreview{setupKind: "none", metered: true}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := createChoiceDiagnostics(at, c.c, c.pv)
			if c.want == "" {
				if d.HasError() {
					t.Fatalf("unexpected: %v", d)
				}
				return
			}
			if d.ErrorsCount() != 1 || !strings.Contains(d.Errors()[0].Summary(), c.want) {
				t.Fatalf("diags = %v, want %q", d, c.want)
			}
		})
	}
}

// Bots written by released providers, which had no `pack` attribute, plan as
// a no-op under this build: `pack` reads back null and no installs call is
// made for a bot without a pack.
func TestBotPack_StateUpgradeFromReleases(t *testing.T) {
	for _, version := range []string{"0.5.1", "0.6.0"} {
		t.Run(version, func(t *testing.T) {
			t.Setenv("TF_ACC_PROVIDER_NAMESPACE", "botyard-ai")
			api, srv := newFakePackBotAPI(t)
			cfg := `
terraform {
  required_providers {
    botyard = { source = "Botyard-AI/botyard" }
  }
}
` + packBotConfig(srv.URL, "")
			resource.UnitTest(t, resource.TestCase{
				Steps: []resource.TestStep{
					{
						ExternalProviders: map[string]resource.ExternalProvider{
							"botyard": {Source: "Botyard-AI/botyard", VersionConstraint: version},
						},
						Config: cfg,
					},
					{
						ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
						Config:                   cfg,
						ConfigPlanChecks: resource.ConfigPlanChecks{
							PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
						},
						Check: func(*terraform.State) error {
							for _, c := range api.callLog() {
								if strings.Contains(c, "/packs/") {
									return fmt.Errorf("a bot without a pack read pack installs: %v", api.callLog())
								}
							}
							return nil
						},
					},
				},
			})
		})
	}
}
