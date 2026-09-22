package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		Identity:         client.IdentityConfig{Emoji: strp("🤖"), Theme: strp("dark")},
		Heartbeat:        &client.HeartbeatConfig{Every: &every, AckMaxChars: &ack},
		Session:          &client.SessionConfig{WriteLockMaxHoldMs: &wlock},
	}
	cfg := &botConfigModel{
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
	// `config.model` is intentionally absent: the API's ModelConfig no longer
	// carries `primary` (it is derived from `chain`), so there is nothing for
	// mapBotConfig to refresh. See the note on botModelModel.
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
	if err := mapBotDesiredConfig(&got.DesiredConfig, cfg); err != nil {
		t.Fatalf("mapBotDesiredConfig: %v", err)
	}
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
	if err := mapBotDesiredConfig(&resp.JSON201.DesiredConfig, plan.Config); err != nil {
		t.Fatalf("mapBotDesiredConfig: %v", err)
	}
	if plan.Config.ThinkingDefault.ValueString() != "high" {
		t.Errorf("mapped thinking_default = %q", plan.Config.ThinkingDefault.ValueString())
	}
}

// desiredConfig builds a BotResponse_DesiredConfig from raw JSON, the way an
// API response is decoded.
func desiredConfig(t *testing.T, raw string) *client.BotResponse_DesiredConfig {
	t.Helper()
	var dc client.BotResponse_DesiredConfig
	if err := json.Unmarshal([]byte(raw), &dc); err != nil {
		t.Fatalf("unmarshal desired_config: %v", err)
	}
	return &dc
}

// TestMapBotDesiredConfig_OpenClawVariant proves the openclaw arm of the
// desired_config union is selected and mapped.
func TestMapBotDesiredConfig_OpenClawVariant(t *testing.T) {
	dc := desiredConfig(t, `{"bot_type":"openclaw","thinking_default":"high","identity":{"emoji":"🤖"}}`)
	cfg := &botConfigModel{Identity: &botIdentityModel{}}
	if err := mapBotDesiredConfig(dc, cfg); err != nil {
		t.Fatalf("mapBotDesiredConfig: %v", err)
	}
	if cfg.ThinkingDefault.ValueString() != "high" {
		t.Errorf("thinking_default = %q", cfg.ThinkingDefault.ValueString())
	}
	if cfg.Identity.Emoji.ValueString() != "🤖" {
		t.Errorf("identity.emoji = %q", cfg.Identity.Emoji.ValueString())
	}
}

// TestMapBotDesiredConfig_MissingDiscriminator proves a response with no
// `bot_type` (the pre-union shape) is still read as an openclaw config rather
// than failing the whole Read.
func TestMapBotDesiredConfig_MissingDiscriminator(t *testing.T) {
	dc := desiredConfig(t, `{"thinking_default":"low"}`)
	cfg := &botConfigModel{}
	if err := mapBotDesiredConfig(dc, cfg); err != nil {
		t.Fatalf("mapBotDesiredConfig: %v", err)
	}
	if cfg.ThinkingDefault.ValueString() != "low" {
		t.Errorf("thinking_default = %q", cfg.ThinkingDefault.ValueString())
	}
}

// TestMapBotDesiredConfig_PresentButEmptyDiscriminator proves a `bot_type` that
// is present but empty or null fails closed rather than being defaulted to
// openclaw. The current API always serializes a concrete bot_type, so these are
// contract violations, not the legacy pre-union shape — defaulting them would
// map a possibly-native config onto the OpenClaw surface.
//
// Regression: the generated Discriminator() returns ("", nil) for an absent key,
// an explicit null and an explicit "" alike, so the original
// `err != nil || botType == ""` check silently accepted all three.
func TestMapBotDesiredConfig_PresentButEmptyDiscriminator(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"empty string", `{"bot_type":"","thinking_default":"low"}`},
		{"explicit null", `{"bot_type":null,"thinking_default":"low"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &botConfigModel{}
			err := mapBotDesiredConfig(desiredConfig(t, tc.raw), cfg)
			if err == nil {
				t.Fatal("want an error for a present-but-empty bot_type")
			}
			if !strings.Contains(err.Error(), "empty or null `bot_type`") {
				t.Errorf("error = %q, want it to name the empty/null bot_type", err)
			}
			// Fail closed: nothing may be mapped from an invalid response.
			if !cfg.ThinkingDefault.IsNull() {
				t.Errorf("thinking_default = %q, want nothing mapped", cfg.ThinkingDefault.ValueString())
			}
		})
	}
}

// TestMapBotDesiredConfig_NullUnionRejected proves a whole-union `null` — and
// the zero-value union a server that omitted `desired_config` entirely produces
// — is rejected rather than taken for the legacy absent-key shape.
//
// Regression: botTypeDiscriminator unmarshalled the raw union into a
// map[string]json.RawMessage, and `null` unmarshals into a map as (nil, nil).
// A nil map is indistinguishable from an empty object, so `bot_type` read as
// absent and the legacy OpenClaw fallback applied — and AsOpenClawBotConfig()
// happily decodes `null` into a zero-value config, so a bot with no config at
// all would have mapped cleanly. `BotResponse.desired_config` is required by
// the schema; the legacy shape is an OBJECT lacking only the discriminator,
// never the absence of the config itself.
func TestMapBotDesiredConfig_NullUnionRejected(t *testing.T) {
	t.Run("explicit null union", func(t *testing.T) {
		cfg := &botConfigModel{}
		err := mapBotDesiredConfig(desiredConfig(t, `null`), cfg)
		if err == nil {
			t.Fatal("want an error for a null desired_config union")
		}
		if !strings.Contains(err.Error(), "no desired_config") {
			t.Errorf("error = %q, want it to name the missing desired_config", err)
		}
	})

	t.Run("omitted field yields zero-value union", func(t *testing.T) {
		// The shape a response whose `desired_config` key was absent produces:
		// the field is never assigned, so the union holds nil and marshals to
		// `null`. Decoded through a struct so this is the real path, not a
		// hand-built zero value.
		var envelope struct {
			DesiredConfig client.BotResponse_DesiredConfig `json:"desired_config"`
		}
		if err := json.Unmarshal([]byte(`{"slug":"x"}`), &envelope); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		cfg := &botConfigModel{}
		err := mapBotDesiredConfig(&envelope.DesiredConfig, cfg)
		if err == nil {
			t.Fatal("want an error for an omitted desired_config")
		}
		// Assert the *reason*, not just that something failed. Before the
		// null-union guard this case did error — but incidentally, from
		// AsOpenClawBotConfig() hitting "unexpected end of JSON input" on a nil
		// union, after the legacy fallback had already misclassified it. The
		// error must name the missing desired_config instead.
		if !strings.Contains(err.Error(), "no desired_config") {
			t.Errorf("error = %q, want it to name the missing desired_config", err)
		}
		if !cfg.ThinkingDefault.IsNull() {
			t.Errorf("thinking_default = %q, want nothing mapped", cfg.ThinkingDefault.ValueString())
		}
	})
}

// TestBotTypeDiscriminator proves the helper distinguishes the three shapes the
// generated Discriminator() collapses into one.
func TestBotTypeDiscriminator(t *testing.T) {
	for _, tc := range []struct {
		name        string
		raw         string
		wantValue   string
		wantPresent bool
	}{
		{"absent", `{"thinking_default":"low"}`, "", false},
		{"null", `{"bot_type":null}`, "", true},
		{"empty string", `{"bot_type":""}`, "", true},
		{"openclaw", `{"bot_type":"openclaw"}`, "openclaw", true},
		{"native", `{"bot_type":"native"}`, "native", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, present, err := botTypeDiscriminator(desiredConfig(t, tc.raw))
			if err != nil {
				t.Fatalf("botTypeDiscriminator: %v", err)
			}
			if value != tc.wantValue || present != tc.wantPresent {
				t.Errorf("= (%q, %v), want (%q, %v)", value, present, tc.wantValue, tc.wantPresent)
			}
		})
	}
}

// TestBotTypeDiscriminator_NonStringIsAnError proves a structurally wrong
// bot_type is reported rather than being read as absent and defaulted.
func TestBotTypeDiscriminator_NonStringIsAnError(t *testing.T) {
	if _, _, err := botTypeDiscriminator(desiredConfig(t, `{"bot_type":42}`)); err == nil {
		t.Fatal("want an error for a non-string bot_type")
	}
}

// TestMapBotDesiredConfig_NativeVariantReported proves a native bot under a
// declared `config` block is reported rather than silently mapped from a
// config surface it does not have.
func TestMapBotDesiredConfig_NativeVariantReported(t *testing.T) {
	dc := desiredConfig(t, `{"bot_type":"native","prompt_template":"x","model":"y"}`)
	cfg := &botConfigModel{}
	err := mapBotDesiredConfig(dc, cfg)
	if err == nil {
		t.Fatal("want an error for a native bot under a declared config block")
	}
	if !strings.Contains(err.Error(), "native") {
		t.Errorf("error should name the bot_type, got %v", err)
	}
}

// TestMapBotDesiredConfig_NilCfgNoop proves an unmanaged config short-circuits
// before the union is decoded (so an undeclared block never errors on a native
// bot).
func TestMapBotDesiredConfig_NilCfgNoop(t *testing.T) {
	dc := desiredConfig(t, `{"bot_type":"native","prompt_template":"x","model":"y"}`)
	if err := mapBotDesiredConfig(dc, nil); err != nil {
		t.Fatalf("nil cfg must be a no-op, got %v", err)
	}
}
