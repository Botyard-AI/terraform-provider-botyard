package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// decodeSub decodes a nested JSON object out of a RawMessage.
func decodeSub(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode sub-object: %v", err)
	}
	return m
}

func TestBuildBotConfigPatch_NilIsEmptyObject(t *testing.T) {
	if got := string(buildBotConfigPatch(nil)); got != "{}" {
		t.Errorf("nil config = %s, want {}", got)
	}
}

// TestBuildBotConfigPatch_ScalarsSparse proves only known, non-null scalar
// leaves are emitted — an unset (null) leaf is omitted, not sent as JSON null,
// so the server leaves it untouched.
func TestBuildBotConfigPatch_ScalarsSparse(t *testing.T) {
	cfg := &botConfigModel{
		SystemPromptMode: types.StringValue("openclaw"),
		ThinkingDefault:  types.StringValue("high"),
		ReasoningDefault: types.StringNull(), // omitted
	}
	m := decodeObj(t, buildBotConfigPatch(cfg))

	if jsonStr(t, m["system_prompt_mode"]) != "openclaw" {
		t.Errorf("system_prompt_mode = %s", m["system_prompt_mode"])
	}
	if jsonStr(t, m["thinking_default"]) != "high" {
		t.Errorf("thinking_default = %s", m["thinking_default"])
	}
	if _, ok := m["reasoning_default"]; ok {
		t.Error("unset reasoning_default must be omitted, not null")
	}
	// Unmodeled keys must never appear.
	for _, k := range []string{"addons", "bot_type", "model", "identity", "heartbeat", "compaction", "session"} {
		if _, ok := m[k]; ok {
			t.Errorf("unexpected key %q in scalar-only patch", k)
		}
	}
}

// TestBuildBotConfigPatch_ModelNestsPrimary proves the model block wraps its
// fields under `primary` and omits the unset provider.
func TestBuildBotConfigPatch_ModelNestsPrimary(t *testing.T) {
	cfg := &botConfigModel{
		Model: &botModelModel{Primary: &botModelRefModel{
			Model:    types.StringValue("gpt-5.4"),
			Provider: types.StringNull(),
		}},
	}
	m := decodeObj(t, buildBotConfigPatch(cfg))
	model := decodeSub(t, m["model"])
	primary := decodeSub(t, model["primary"])
	if jsonStr(t, primary["model"]) != "gpt-5.4" {
		t.Errorf("model = %s", primary["model"])
	}
	if _, ok := primary["provider"]; ok {
		t.Error("unset provider must be omitted")
	}
}

// TestBuildBotConfigPatch_NestedTypes proves int/bool/string leaves serialize
// with the right JSON types and that active_hours nests inside heartbeat.
func TestBuildBotConfigPatch_NestedTypes(t *testing.T) {
	cfg := &botConfigModel{
		Identity: &botIdentityModel{Emoji: types.StringValue("🤖"), Theme: types.StringNull()},
		Heartbeat: &botHeartbeatModel{
			Every:            types.StringValue("30m"),
			AckMaxChars:      types.Int64Value(300),
			IncludeReasoning: types.BoolValue(true),
			ActiveHours: &botActiveHoursModel{
				FromTime: types.StringValue("09:00"),
				ToTime:   types.StringValue("17:00"),
				Timezone: types.StringValue("America/New_York"),
			},
		},
		Compaction: &botCompactionModel{
			MaxActiveTranscriptBytes: types.Int64Value(8000000),
			MidTurnPrecheck:          types.BoolValue(false),
		},
		Session: &botSessionModel{WriteLockMaxHoldMs: types.Int64Value(300000)},
	}
	m := decodeObj(t, buildBotConfigPatch(cfg))

	id := decodeSub(t, m["identity"])
	if jsonStr(t, id["emoji"]) != "🤖" {
		t.Errorf("emoji = %s", id["emoji"])
	}
	if _, ok := id["theme"]; ok {
		t.Error("unset theme must be omitted")
	}

	hb := decodeSub(t, m["heartbeat"])
	if jsonStr(t, hb["every"]) != "30m" {
		t.Errorf("every = %s", hb["every"])
	}
	if string(hb["ack_max_chars"]) != "300" {
		t.Errorf("ack_max_chars = %s, want bare int 300", hb["ack_max_chars"])
	}
	if string(hb["include_reasoning"]) != "true" {
		t.Errorf("include_reasoning = %s, want bool true", hb["include_reasoning"])
	}
	ah := decodeSub(t, hb["active_hours"])
	if jsonStr(t, ah["from_time"]) != "09:00" || jsonStr(t, ah["timezone"]) != "America/New_York" {
		t.Errorf("active_hours = %s", hb["active_hours"])
	}

	comp := decodeSub(t, m["compaction"])
	if string(comp["max_active_transcript_bytes"]) != "8000000" {
		t.Errorf("max_active_transcript_bytes = %s", comp["max_active_transcript_bytes"])
	}
	if string(comp["mid_turn_precheck"]) != "false" {
		t.Errorf("mid_turn_precheck = %s, want bool false", comp["mid_turn_precheck"])
	}

	sess := decodeSub(t, m["session"])
	if string(sess["write_lock_max_hold_ms"]) != "300000" {
		t.Errorf("write_lock_max_hold_ms = %s", sess["write_lock_max_hold_ms"])
	}
}

// TestBuildBotConfigPatch_EmptyNestedOmitted proves an all-unset nested block
// (or a model block with no primary) is omitted entirely, not emitted as `{}`.
func TestBuildBotConfigPatch_EmptyNestedOmitted(t *testing.T) {
	cfg := &botConfigModel{
		Model:      &botModelModel{Primary: nil},
		Heartbeat:  &botHeartbeatModel{}, // all null
		Compaction: &botCompactionModel{},
		Session:    &botSessionModel{},
		Identity:   &botIdentityModel{},
	}
	m := decodeObj(t, buildBotConfigPatch(cfg))
	if len(m) != 0 {
		t.Errorf("all-empty nested blocks must be omitted, got keys %v", keysOf(m))
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestMapBotConfig_RefreshesScalarsAndDeclaredNested proves scalars are always
// refreshed and declared nested blocks pick up server values.
func TestMapBotConfig_RefreshesScalarsAndDeclaredNested(t *testing.T) {
	spm := client.OpenClawBotConfigSystemPromptMode("openclaw")
	td := client.OpenClawBotConfigThinkingDefault("high")
	every := client.HeartbeatConfigEvery("30m")
	ack := 300
	wlock := 300000
	dc := &client.OpenClawBotConfig{
		SystemPromptMode: &spm,
		ThinkingDefault:  &td,
		Model:            &client.ModelConfig{Primary: &client.ModelRef{Model: "gpt-5.4", Provider: strp("botyard")}},
		Identity:         client.IdentityConfig{Emoji: strp("🤖"), Theme: strp("dark")},
		Heartbeat:        &client.HeartbeatConfig{Every: &every, AckMaxChars: &ack},
		Session:          &client.SessionConfig{WriteLockMaxHoldMs: &wlock},
	}
	cfg := &botConfigModel{
		Model:     &botModelModel{Primary: &botModelRefModel{}},
		Identity:  &botIdentityModel{},
		Heartbeat: &botHeartbeatModel{},
		Session:   &botSessionModel{},
	}
	mapBotConfig(dc, cfg)

	if cfg.SystemPromptMode.ValueString() != "openclaw" {
		t.Errorf("system_prompt_mode = %q", cfg.SystemPromptMode.ValueString())
	}
	if cfg.ThinkingDefault.ValueString() != "high" {
		t.Errorf("thinking_default = %q", cfg.ThinkingDefault.ValueString())
	}
	if !cfg.ReasoningDefault.IsNull() {
		t.Errorf("reasoning_default should be null (server nil), got %q", cfg.ReasoningDefault.ValueString())
	}
	if cfg.Model.Primary.Model.ValueString() != "gpt-5.4" || cfg.Model.Primary.Provider.ValueString() != "botyard" {
		t.Errorf("model primary = %+v", cfg.Model.Primary)
	}
	if cfg.Identity.Emoji.ValueString() != "🤖" || cfg.Identity.Theme.ValueString() != "dark" {
		t.Errorf("identity = %+v", cfg.Identity)
	}
	if cfg.Heartbeat.Every.ValueString() != "30m" || cfg.Heartbeat.AckMaxChars.ValueInt64() != 300 {
		t.Errorf("heartbeat = %+v", cfg.Heartbeat)
	}
	if cfg.Session.WriteLockMaxHoldMs.ValueInt64() != 300000 {
		t.Errorf("session write_lock = %d", cfg.Session.WriteLockMaxHoldMs.ValueInt64())
	}
}

// TestMapBotConfig_UndeclaredNestedNotPopulated proves an undeclared nested
// block (nil pointer) is NOT populated from server defaults — avoiding a phantom
// diff for config the practitioner never declared.
func TestMapBotConfig_UndeclaredNestedNotPopulated(t *testing.T) {
	dc := &client.OpenClawBotConfig{
		Model:    &client.ModelConfig{Primary: &client.ModelRef{Model: "gpt-5.4"}},
		Identity: client.IdentityConfig{Emoji: strp("🤖")},
	}
	cfg := &botConfigModel{} // nothing declared
	mapBotConfig(dc, cfg)
	if cfg.Model != nil {
		t.Error("undeclared model must stay nil")
	}
	if cfg.Identity != nil {
		t.Error("undeclared identity must stay nil")
	}
	if cfg.Heartbeat != nil || cfg.Compaction != nil || cfg.Session != nil {
		t.Error("undeclared nested blocks must stay nil")
	}
}

func TestMapBotConfig_NilCfgNoop(_ *testing.T) {
	// Must not panic on a nil config (config not managed).
	mapBotConfig(&client.OpenClawBotConfig{}, nil)
}

// TestBuildBotCreateBody_EmbedsConfig proves a declared config block is embedded
// in the create POST, and an omitted block yields an empty config object.
func TestBuildBotCreateBody_EmbedsConfig(t *testing.T) {
	model := botResourceModel()
	model.Config = &botConfigModel{ThinkingDefault: types.StringValue("medium")}
	body, diags := buildBotCreateBody(model)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	m := decodeObj(t, body)
	cfg := decodeSub(t, m["config"])
	if jsonStr(t, cfg["thinking_default"]) != "medium" {
		t.Errorf("embedded config thinking_default = %s", cfg["thinking_default"])
	}

	// Omitted config → empty object (Phase A behavior preserved).
	body2, _ := buildBotCreateBody(botResourceModel())
	if string(decodeObj(t, body2)["config"]) != "{}" {
		t.Errorf("omitted config must embed {} , got %s", decodeObj(t, body2)["config"])
	}
}

// cannedBotWithConfigJSON is a live bot whose desired_config carries a modeled
// field, exercising the config PATCH response round-trip.
const cannedBotWithConfigJSON = `{
  "id": "b-123", "slug": "my-bot", "org_id": "org-1", "name": "My Bot",
  "namespace": "bot-my-bot", "runtime_class": "kata_qemu",
  "storage_class": "cluster_default", "runtime_privilege_mode": "privileged",
  "onboarding_state": "none", "health_status": "healthy",
  "desired_state": "running", "config_generation": 8,
  "created_at": "2026-07-20T10:00:00Z", "updated_at": "2026-07-20T12:00:00Z",
  "desired_config": { "thinking_default": "high", "system_prompt_mode": "openclaw" }
}`

// TestBotResource_UpdateConfigRoundTrip proves updateBotConfig targets the
// slug-addressed /config path with PATCH, wraps the sparse patch under a top-level
// `config` key, and maps the merged 200 response back into the config block.
func TestBotResource_UpdateConfigRoundTrip(t *testing.T) {
	const orgID, slug = "org-1", "my-bot"
	var gotPath, gotMethod string
	var gotBodyRaw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotBodyRaw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedBotWithConfigJSON))
	}))
	defer srv.Close()

	r := &BotResource{data: &providerData{client: newBotClient(t, srv.URL, "byk_test"), orgID: orgID}}
	cfg := &botConfigModel{ThinkingDefault: types.StringValue("high")}
	diags := &diag.Diagnostics{}
	got, ok := r.updateBotConfig(context.Background(), slug, cfg, diags)
	if !ok {
		t.Fatalf("updateBotConfig failed: %v", diags)
	}

	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if want := "/v1/orgs/" + orgID + "/bots/" + slug + "/config"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	// The wire body wraps the sparse patch under `config`.
	sent := decodeObj(t, gotBodyRaw)
	inner := decodeSub(t, sent["config"])
	if jsonStr(t, inner["thinking_default"]) != "high" {
		t.Errorf("sent config.thinking_default = %s", inner["thinking_default"])
	}
	// The merged response mapped back.
	mapBotConfig(&got.DesiredConfig, cfg)
	if cfg.ThinkingDefault.ValueString() != "high" || cfg.SystemPromptMode.ValueString() != "openclaw" {
		t.Errorf("mapped config = %+v", cfg)
	}
}

// TestBotResource_UpdateConfigUnexpectedStatus proves a non-200 config PATCH
// records a diagnostic and returns ok=false (so the caller aborts).
func TestBotResource_UpdateConfigUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":"bad enum"}`))
	}))
	defer srv.Close()

	r := &BotResource{data: &providerData{client: newBotClient(t, srv.URL, "byk_test"), orgID: "org-1"}}
	diags := &diag.Diagnostics{}
	_, ok := r.updateBotConfig(context.Background(), "my-bot", &botConfigModel{ThinkingDefault: types.StringValue("nope")}, diags)
	if ok {
		t.Error("expected ok=false on 422")
	}
	if !diags.HasError() {
		t.Error("expected a diagnostic on 422")
	}
}

// TestBotResource_CreateWithConfigRoundTrip proves the create POST embeds the
// config and the 201 desired_config maps back into the declared block.
func TestBotResource_CreateWithConfigRoundTrip(t *testing.T) {
	const orgID = "org-1"
	var gotBodyRaw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBodyRaw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(cannedBotWithConfigJSON))
	}))
	defer srv.Close()

	c := newBotClient(t, srv.URL, "byk_test")
	plan := botResourceModel()
	plan.Config = &botConfigModel{ThinkingDefault: types.StringValue("high")}
	body, diags := buildBotCreateBody(plan)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	resp, err := c.CreateBotV1OrgsOrgIdBotsPostWithBodyWithResponse(
		context.Background(), orgID, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("CreateBot: %v", err)
	}
	if resp.JSON201 == nil {
		t.Fatalf("JSON201 nil (status %d)", resp.StatusCode())
	}
	sentCfg := decodeSub(t, decodeObj(t, gotBodyRaw)["config"])
	if jsonStr(t, sentCfg["thinking_default"]) != "high" {
		t.Errorf("create body config = %s", decodeObj(t, gotBodyRaw)["config"])
	}
	mapBotConfig(&resp.JSON201.DesiredConfig, plan.Config)
	if plan.Config.ThinkingDefault.ValueString() != "high" {
		t.Errorf("mapped thinking_default = %q", plan.Config.ThinkingDefault.ValueString())
	}
}
