package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// PlatDev #4028a, decision D1 #4374: `suggestions_opt_out` on botyard_bot.
//
// fakeSuggestionsBotAPI serves one OpenClaw bot plus its suggestions surface:
// GET /suggestions and PUT /suggestions/opt-out, both owner-gated like the real
// routes (require_bot_owner). Flipping `owned` off models a bot the provider's
// API key did not create (D6 #4379, keep_403).
//
// The bot responses carry `suggestions_opt_out` like botyard v1.7.2256+ (#4524)
// unless `legacy` is set, which models an older API that only exposes the flag
// on the suggestions GET. `gets` counts those suggestions GETs: the current API
// must never see one on refresh.
type fakeSuggestionsBotAPI struct {
	t *testing.T

	mu      sync.Mutex
	exists  bool
	owned   bool
	optOut  bool
	desc    *string
	puts    []bool
	patches int // bot PATCH attempts, refused or not
	// failPatch makes the bot PATCH, or PATCH /config, answer 500.
	failPatch, failConfig bool
	gets                  int
	legacy                bool
}

const suggestionsBotPath = "/v1/orgs/org-1/bots/opt-probe"

func newFakeSuggestionsBotAPI(t *testing.T) (*fakeSuggestionsBotAPI, *httptest.Server) {
	t.Helper()
	api := &fakeSuggestionsBotAPI{t: t, owned: true}
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return api, srv
}

func (a *fakeSuggestionsBotAPI) set(fn func(*fakeSuggestionsBotAPI)) func() {
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		fn(a)
	}
}

// wantGets checks the number of suggestions GETs so far.
func (a *fakeSuggestionsBotAPI) wantGets(want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.gets != want {
			return fmt.Errorf("suggestions GETs = %d, want %d", a.gets, want)
		}
		return nil
	}
}

// apiGenerations runs fn against the current API and against a legacy one
// whose bot responses lack `suggestions_opt_out`.
func apiGenerations(t *testing.T, fn func(t *testing.T, legacy bool)) {
	t.Helper()
	for _, legacy := range []bool{false, true} {
		name := "current"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) { fn(t, legacy) })
	}
}

// wantPuts checks the opt-out writes the provider has made so far, in order.
func (a *fakeSuggestionsBotAPI) wantPuts(want ...bool) resource.TestCheckFunc {
	return func(*terraform.State) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if fmt.Sprint(a.puts) != fmt.Sprint(want) {
			return fmt.Errorf("opt-out PUTs = %v, want %v", a.puts, want)
		}
		return nil
	}
}

func (a *fakeSuggestionsBotAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/orgs/org-1/bots":
		a.exists, a.owned, a.optOut = true, true, false
		a.writeBot(w, http.StatusCreated)
	case r.Method == http.MethodGet && r.URL.Path == suggestionsBotPath && a.exists:
		a.writeBot(w, http.StatusOK)
	case r.Method == http.MethodPatch && r.URL.Path == suggestionsBotPath && a.exists:
		// Like the API, every write to the bot is owner-gated (require_bot_owner
		// on PATCH, PATCH /config and DELETE), not only the opt-out.
		a.patches++
		if a.forbid(w) {
			return
		}
		if a.failPatch {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"title":"injected PATCH failure","status":500}`))
			return
		}
		var body struct {
			Description *string `json:"description"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			a.t.Errorf("bot PATCH body %s: %v", raw, err)
		}
		a.desc = body.Description
		a.writeBot(w, http.StatusOK)
	case r.Method == http.MethodDelete && r.URL.Path == suggestionsBotPath && a.exists:
		if a.forbid(w) {
			return
		}
		a.exists = false
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPatch && r.URL.Path == suggestionsBotPath+"/config" && a.exists && a.failConfig:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"title":"injected config failure","status":500}`))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/orgs/org-1/packs/installs":
		// Import looks for Bot Pack installs (#38); this bot has none.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"bots":[{"bot_id":"8a1b2c3d-0000-4000-8000-000000000001","installs":[]}],"not_found":[]}`))
	case r.Method == http.MethodGet && r.URL.Path == suggestionsBotPath+"/suggestions" && a.exists:
		a.gets++
		if a.forbid(w) {
			return
		}
		a.writeSuggestions(w)
	case r.Method == http.MethodPut && r.URL.Path == suggestionsBotPath+"/suggestions/opt-out" && a.exists:
		if a.forbid(w) {
			return
		}
		var body struct {
			OptOut *bool `json:"opt_out"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil || body.OptOut == nil {
			a.t.Errorf("opt-out PUT body %s: want {\"opt_out\": bool}", raw)
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		a.puts = append(a.puts, *body.OptOut)
		a.optOut = *body.OptOut
		a.writeSuggestions(w)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"not found"}`))
	}
}

func (a *fakeSuggestionsBotAPI) forbid(w http.ResponseWriter) bool {
	if a.owned {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	// The real owner gate's problem body (ForbiddenError, code `forbidden`).
	_, _ = w.Write([]byte(botOwnerForbiddenBody))
	return true
}

func (a *fakeSuggestionsBotAPI) writeSuggestions(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"skills":[],"tools":[],"opt_out":%t}`, a.optOut)
}

func (a *fakeSuggestionsBotAPI) writeBot(w http.ResponseWriter, status int) {
	desc, err := json.Marshal(a.desc)
	if err != nil {
		a.t.Errorf("encode description: %v", err)
	}
	optOut := ""
	if !a.legacy {
		optOut = fmt.Sprintf(`"suggestions_opt_out": %t,`, a.optOut)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{
		%s
		"description": %s,
		"id": "8a1b2c3d-0000-4000-8000-000000000001", "slug": "opt-probe", "org_id": "org-1", "cluster_id": "c-1",
		"name": "opt-probe", "namespace": "bot-opt-probe", "tier": "starter",
		"runtime_class": "kata_qemu", "storage_class": "cluster_default",
		"runtime_privilege_mode": "standard", "onboarding_state": "complete",
		"health_status": "healthy", "desired_state": "running", "config_generation": 1,
		"access": "open", "hosting_type": "hosted", "harness": "openclaw",
		"created_at": "2026-10-29T10:00:00Z", "updated_at": "2026-10-29T10:00:00Z",
		"desired_config": {}
	}`, optOut, desc)
}

func suggestionsTestConfig(endpoint, attr string) string {
	return fmt.Sprintf(`
provider "botyard" {
  endpoint = %q
  api_key  = "byk_test"
  org_id   = "org-1"
}

resource "botyard_bot" "b" {
  name = "opt-probe"
  %s
}
`, endpoint, attr)
}

// TestSuggestionsOptOut_Lifecycle drives real Terraform core through the D1
// acceptance criteria: an omitted flag is read, not written; true and an
// explicit false are both written and re-plan clean; removing the attribute
// leaves the server value alone; out-of-band drift against a declared value is
// planned back; import reads the flag with no diff.
//
// Against the current API every read comes from the bot responses, so the
// suggestions GET is never called (#4524); a legacy API falls back to it.
func TestSuggestionsOptOut_Lifecycle(t *testing.T) {
	apiGenerations(t, testSuggestionsOptOutLifecycle)
}

func testSuggestionsOptOutLifecycle(t *testing.T, legacy bool) {
	api, srv := newFakeSuggestionsBotAPI(t)
	api.legacy = legacy
	const addr = "botyard_bot.b"
	omitted := suggestionsTestConfig(srv.URL, "")
	optedOut := suggestionsTestConfig(srv.URL, "suggestions_opt_out = true")
	optedIn := suggestionsTestConfig(srv.URL, "suggestions_opt_out = false")

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Omitted on create: read the server default, write nothing.
				Config: omitted,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "suggestions_opt_out", "false"),
					api.wantPuts(),
				),
			},
			{
				Config: optedOut,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "suggestions_opt_out", "true"),
					api.wantPuts(true),
				),
			},
			{Config: optedOut, PlanOnly: true},
			{
				// Explicit false must reach the server, not be dropped as a zero value.
				Config: optedIn,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "suggestions_opt_out", "false"),
					api.wantPuts(true, false),
				),
			},
			{Config: optedIn, PlanOnly: true},
			{
				// Someone opts the bot out in the app: a declared false plans it back.
				PreConfig:          api.set(func(a *fakeSuggestionsBotAPI) { a.optOut = true }),
				Config:             optedIn,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: optedIn,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "suggestions_opt_out", "false"),
					api.wantPuts(true, false, false),
				),
			},
			{
				// Dropping the attribute stops managing it: no diff, no write,
				// and the state follows the server.
				PreConfig: api.set(func(a *fakeSuggestionsBotAPI) { a.optOut = true }),
				Config:    omitted,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "suggestions_opt_out", "true"),
					api.wantPuts(true, false, false),
				),
			},
			{Config: omitted, PlanOnly: true},
			{
				ResourceName:      addr,
				ImportState:       true,
				ImportStateId:     "opt-probe",
				ImportStateVerify: true,
				Config:            omitted,
			},
		},
	})
	switch gets := api.gets; {
	case !legacy && gets != 0:
		t.Fatalf("suggestions GETs = %d, want 0: the current API carries the flag on the bot", gets)
	case legacy && gets == 0:
		t.Fatal("suggestions GETs = 0: a legacy API must fall back to the suggestions GET")
	}
}

// TestSuggestionsOptOut_NotBotOwner covers a bot the API key did not create
// (D6 #4379). The API refuses every write to such a bot; what the provider owes
// is that refresh and plan keep working (the current API reads the flag off the
// bot; a legacy one keeps the prior value on the suggestions GET's 403), and
// that changing the flag fails with a diagnostic that names the setting and the
// ownership rule.
func TestSuggestionsOptOut_NotBotOwner(t *testing.T) {
	apiGenerations(t, testSuggestionsOptOutNotBotOwner)
}

func testSuggestionsOptOutNotBotOwner(t *testing.T, legacy bool) {
	api, srv := newFakeSuggestionsBotAPI(t)
	api.legacy = legacy
	const addr = "botyard_bot.b"
	omitted := suggestionsTestConfig(srv.URL, "")
	disowned := api.set(func(a *fakeSuggestionsBotAPI) { a.owned = false })

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: omitted, Check: resource.TestCheckResourceAttr(addr, "suggestions_opt_out", "false")},
			{
				PreConfig: disowned,
				// A refresh-only apply. Current API: read off the bot. Legacy:
				// the 403 on the flag's GET is tolerated.
				RefreshState: true,
				Check:        resource.TestCheckResourceAttr(addr, "suggestions_opt_out", "false"),
			},
			{Config: omitted, PlanOnly: true},
			{
				Config:      suggestionsTestConfig(srv.URL, "suggestions_opt_out = true"),
				ExpectError: regexp.MustCompile(`(?s)Not the bot's owner.*suggestions_opt_out.*API key owns only the bots it\s+created`),
			},
			{
				// The failed write left the prior value in state, so the next plan
				// still shows the change rather than pretending it applied.
				Config:             suggestionsTestConfig(srv.URL, "suggestions_opt_out = true"),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Give ownership back so the post-test destroy is unconstrained.
				PreConfig: api.set(func(a *fakeSuggestionsBotAPI) { a.owned = true }),
				Config:    omitted,
				PlanOnly:  true,
			},
		},
	})
	if err := api.wantPuts()(nil); err != nil {
		t.Fatal(err)
	}
	// The refused opt-out stopped the update before the metadata PATCH, so the
	// practitioner saw the owner diagnostic, not a generic 403.
	if api.patches != 0 {
		t.Fatalf("bot PATCH attempts = %d, want 0", api.patches)
	}
}

// TestSuggestionsOptOut_ImportNotOwned: importing a bot the key did not
// create. The current API carries the flag on the bot GET, so import reads the
// real value; a legacy API cannot read it, so the attribute stays null. Either
// way an omitted configuration plans clean.
func TestSuggestionsOptOut_ImportNotOwned(t *testing.T) {
	apiGenerations(t, func(t *testing.T, legacy bool) {
		api, srv := newFakeSuggestionsBotAPI(t)
		api.exists, api.owned, api.optOut, api.legacy = true, false, true, legacy
		const addr = "botyard_bot.b"
		want := "true"
		if legacy {
			want = "" // unreadable without ownership
		}

		resource.UnitTest(t, resource.TestCase{
			ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
			Steps: []resource.TestStep{
				{
					ResourceName:       addr,
					ImportState:        true,
					ImportStateId:      "opt-probe",
					ImportStatePersist: true,
					Config:             suggestionsTestConfig(srv.URL, ""),
					ImportStateCheck: func(states []*terraform.InstanceState) error {
						if len(states) != 1 {
							return fmt.Errorf("imported %d instances, want 1", len(states))
						}
						if v := states[0].Attributes["suggestions_opt_out"]; v != want {
							return fmt.Errorf("suggestions_opt_out = %q, want %q", v, want)
						}
						return nil
					},
				},
				{Config: suggestionsTestConfig(srv.URL, ""), PlanOnly: true},
				{
					PreConfig: api.set(func(a *fakeSuggestionsBotAPI) { a.owned = true }),
					Config:    suggestionsTestConfig(srv.URL, ""),
					PlanOnly:  true,
				},
			},
		})
		if !legacy {
			if err := api.wantGets(0)(nil); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// TestSuggestionsOptOut_UpdateCheckpointsAppliedFlag pins Update's partial
// state when the opt-out PUT succeeds and a later write fails. It calls Update
// the way the framework does (resp.State seeded with the PRIOR state) and
// inspects the returned state directly: through Terraform core, the next
// plan's refresh would re-read the flag and mask a lost update.
func TestSuggestionsOptOut_UpdateCheckpointsAppliedFlag(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inject     func(a *fakeSuggestionsBotAPI)
		withConfig bool
		errSummary string
	}{
		{"metadata PATCH fails", func(a *fakeSuggestionsBotAPI) { a.failPatch = true }, false, "Unexpected response updating bot"},
		{"config PATCH fails", func(a *fakeSuggestionsBotAPI) { a.failConfig = true }, true, "Unexpected response updating bot config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			api, srv := newFakeSuggestionsBotAPI(t)
			api.exists = true
			tc.inject(api)

			r := &BotResource{data: &providerData{client: newBotClient(t, srv.URL, "byk_test"), orgID: "org-1"}}
			sch := botSchema(t)

			prior := BotResourceModel{
				Name:              types.StringValue("opt-probe"),
				Slug:              types.StringValue("opt-probe"),
				ID:                types.StringValue("8a1b2c3d-0000-4000-8000-000000000001"),
				Description:       types.StringNull(),
				SuggestionsOptOut: types.BoolValue(false),
			}
			plan := prior
			plan.Description = types.StringValue("pending")
			plan.SuggestionsOptOut = types.BoolValue(true)
			if tc.withConfig {
				plan.Config = &botConfigModel{}
			}

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
				t.Fatal("Update succeeded; want the injected failure")
			}
			found := false
			for _, d := range resp.Diagnostics.Errors() {
				found = found || d.Summary() == tc.errSummary
			}
			if !found {
				t.Fatalf("diagnostics %v lack %q", resp.Diagnostics.Errors(), tc.errSummary)
			}

			var got BotResourceModel
			if d := resp.State.Get(ctx, &got); d.HasError() {
				t.Fatalf("returned state: %v", d)
			}
			if !got.SuggestionsOptOut.Equal(types.BoolValue(true)) {
				t.Errorf("returned suggestions_opt_out = %s, want true: the PUT succeeded", got.SuggestionsOptOut)
			}
			if !got.Description.IsNull() {
				t.Errorf("returned description = %s, want the prior null: the plan's value was never recorded as applied", got.Description)
			}
			if err := api.wantPuts(true)(nil); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestSuggestionsOptOut_UpdateFailedOptOutReturnsPrior: when the opt-out PUT
// itself is refused (a bot the key does not own), Update returns the prior
// state untouched, so the flag keeps its known value rather than being
// nulled, and nothing else is written. Refresh-free, like the test above: a
// 403 refresh keeps whatever was saved, so through core a lost value would
// stay lost without any visible diff.
func TestSuggestionsOptOut_UpdateFailedOptOutReturnsPrior(t *testing.T) {
	ctx := context.Background()
	api, srv := newFakeSuggestionsBotAPI(t)
	api.exists, api.owned = true, false

	r := &BotResource{data: &providerData{client: newBotClient(t, srv.URL, "byk_test"), orgID: "org-1"}}
	sch := botSchema(t)
	prior := BotResourceModel{
		Name:              types.StringValue("opt-probe"),
		Slug:              types.StringValue("opt-probe"),
		ID:                types.StringValue("8a1b2c3d-0000-4000-8000-000000000001"),
		Description:       types.StringNull(),
		SuggestionsOptOut: types.BoolValue(false),
	}
	plan := prior
	plan.SuggestionsOptOut = types.BoolValue(true)

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

	if !resp.Diagnostics.HasError() || resp.Diagnostics.Errors()[0].Summary() != "Not the bot's owner" {
		t.Fatalf("diagnostics = %v, want the owner error first", resp.Diagnostics.Errors())
	}
	var got BotResourceModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatalf("returned state: %v", d)
	}
	if !got.SuggestionsOptOut.Equal(types.BoolValue(false)) {
		t.Errorf("returned suggestions_opt_out = %s, want the prior false", got.SuggestionsOptOut)
	}
	if api.patches != 0 {
		t.Errorf("bot PATCH attempts = %d, want 0", api.patches)
	}
}
