package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// *** THE REGRESSION THIS WHOLE FILE EXISTS FOR ***
//
// The API's `config` is an anyOf of OpenClawConfigPatch | NativeConfigPatch with
// NO discriminator, and OpenClawConfigPatch ignores unknown keys. A native patch
// sent without `bot_type` therefore validates as an EMPTY OpenClaw patch: the
// bot is created, the config is silently dropped, HTTP 201, no error anywhere.
// There is no server-side signal for this — the only thing standing between a
// practitioner and a silently-misconfigured bot is that we emit the
// discriminator. Hence a test that asserts the literal wire byte.
func TestBuildNativeConfigPatch_AlwaysEmitsDiscriminator(t *testing.T) {
	raw := buildNativeConfigPatch(&botNativeConfigModel{
		PromptTemplate: types.StringValue("You are a helpful bot."),
	})
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if string(got["bot_type"]) != `"native"` {
		t.Fatalf("bot_type = %s, want \"native\" — without it the API silently "+
			"reads this as an empty OpenClaw patch and drops the config", got["bot_type"])
	}
	if string(got["prompt_template"]) != `"You are a helpful bot."` {
		t.Errorf("prompt_template = %s", got["prompt_template"])
	}
}

// Platform-owned fields must never be sent, however they got into state.
func TestBuildNativeConfigPatch_OmitsPlatformOwnedFields(t *testing.T) {
	raw := buildNativeConfigPatch(&botNativeConfigModel{
		PromptTemplate: types.StringValue("x"),
		ModelProvider:  types.StringValue("botyard"),
		ModelName:      types.StringValue("gpt-5.4"),
		HostPolicyJSON: types.StringValue(`{"service_owner":"native"}`),
	})
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	for _, k := range []string{"model", "model_provider", "model_name", "host_policy", "host_policy_json"} {
		if _, present := got[k]; present {
			t.Errorf("patch must not carry platform-owned key %q: %s", k, raw)
		}
	}
}

func TestBuildNativeConfigPatch_NilIsEmptyObject(t *testing.T) {
	if got := string(buildNativeConfigPatch(nil)); got != "{}" {
		t.Errorf("nil block = %s, want {}", got)
	}
}

func TestBuildToolSearchPatch(t *testing.T) {
	raw := buildNativeConfigPatch(&botNativeConfigModel{
		ToolSearch: &botToolSearchModel{
			Mode:                 types.StringValue("auto"),
			AutoThresholdPercent: types.Int64Value(40),
		},
	})
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	var ts map[string]json.RawMessage
	if err := json.Unmarshal(got["tool_search"], &ts); err != nil {
		t.Fatalf("decode tool_search: %v", err)
	}
	if string(ts["mode"]) != `"auto"` || string(ts["auto_threshold_percent"]) != "40" {
		t.Errorf("tool_search = %s", got["tool_search"])
	}
}

// --- the read path ---------------------------------------------------------

func nativeBotConfig(t *testing.T, raw string) *client.NativeBotConfig {
	t.Helper()
	nc := &client.NativeBotConfig{}
	if err := json.Unmarshal([]byte(raw), nc); err != nil {
		t.Fatalf("decode NativeBotConfig: %v", err)
	}
	return nc
}

func TestMapNativeBotConfig(t *testing.T) {
	nc := nativeBotConfig(t, `{
		"bot_type": "native",
		"model": {"provider": "botyard", "model": "gpt-5.4"},
		"prompt_template": "You are Anvil.",
		"prompt_ref": "prompts/anvil@3",
		"host_policy": {"service_owner": "native", "enabled": ["exec"]},
		"tool_search": {"mode": "auto", "auto_threshold_percent": 40}
	}`)
	cfg := &botNativeConfigModel{ToolSearch: &botToolSearchModel{}}
	mapNativeBotConfig(nc, cfg)

	if cfg.PromptTemplate.ValueString() != "You are Anvil." {
		t.Errorf("prompt_template = %q", cfg.PromptTemplate.ValueString())
	}
	if cfg.PromptRef.ValueString() != "prompts/anvil@3" {
		t.Errorf("prompt_ref = %q", cfg.PromptRef.ValueString())
	}
	if cfg.ModelProvider.ValueString() != "botyard" || cfg.ModelName.ValueString() != "gpt-5.4" {
		t.Errorf("model = %q/%q", cfg.ModelProvider.ValueString(), cfg.ModelName.ValueString())
	}
	if cfg.HostPolicyJSON.IsNull() {
		t.Error("host_policy_json should be populated when the server sent a policy")
	}
	if cfg.ToolSearch.Mode.ValueString() != "auto" || cfg.ToolSearch.AutoThresholdPercent.ValueInt64() != 40 {
		t.Errorf("tool_search = %+v", cfg.ToolSearch)
	}
}

// An undeclared tool_search block must NOT be populated from server defaults —
// that is how a phantom diff gets created (same rule as the OpenClaw mapper).
func TestMapNativeBotConfig_UndeclaredToolSearchNotPopulated(t *testing.T) {
	nc := nativeBotConfig(t, `{
		"bot_type": "native",
		"model": {"model": "gpt-5.4"},
		"prompt_template": "x",
		"tool_search": {"mode": "always"}
	}`)
	cfg := &botNativeConfigModel{}
	mapNativeBotConfig(nc, cfg)
	if cfg.ToolSearch != nil {
		t.Error("undeclared tool_search must stay nil")
	}
}

// A native config with no tool_search at all must not panic: the field is a
// pointer on the response even though the block may be declared locally.
func TestMapNativeBotConfig_MissingServerToolSearch(t *testing.T) {
	nc := nativeBotConfig(t, `{"bot_type":"native","model":{"model":"m"},"prompt_template":"x"}`)
	cfg := &botNativeConfigModel{ToolSearch: &botToolSearchModel{}}
	mapNativeBotConfig(nc, cfg) // must not panic
	if !cfg.ToolSearch.Mode.IsNull() {
		t.Errorf("mode = %q, want null", cfg.ToolSearch.Mode.ValueString())
	}
}

func TestMapNativeBotConfig_AbsentHostPolicyIsNull(t *testing.T) {
	nc := nativeBotConfig(t, `{"bot_type":"native","model":{"model":"m"},"prompt_template":"x"}`)
	cfg := &botNativeConfigModel{}
	mapNativeBotConfig(nc, cfg)
	if !cfg.HostPolicyJSON.IsNull() {
		t.Errorf("host_policy_json = %q, want null", cfg.HostPolicyJSON.ValueString())
	}
}

func TestMapNativeBotConfig_NilCfgNoop(_ *testing.T) {
	mapNativeBotConfig(&client.NativeBotConfig{}, nil) // must not panic
}

// --- union routing ---------------------------------------------------------

func TestMapBotDesiredConfig_NativeRoutesToNativeBlock(t *testing.T) {
	dc := unionDC(t, `{"bot_type":"native","model":{"model":"gpt-5.4"},"prompt_template":"hello"}`)
	native := &botNativeConfigModel{}
	diags := &diag.Diagnostics{}
	mapBotDesiredConfig(dc, nil, native, diags)
	if diags.HasError() {
		t.Fatalf("unexpected diags: %v", diags.Errors())
	}
	if native.PromptTemplate.ValueString() != "hello" {
		t.Errorf("prompt_template = %q", native.PromptTemplate.ValueString())
	}
}

// Declaring native_config against an OpenClaw bot is the mirror of the existing
// config-over-native case, and must fail just as loudly.
func TestMapBotDesiredConfig_OpenClawWithNativeBlockIsRefused(t *testing.T) {
	dc := unionDC(t, `{"bot_type":"openclaw","thinking_default":"high"}`)
	diags := &diag.Diagnostics{}
	mapBotDesiredConfig(dc, nil, &botNativeConfigModel{}, diags)
	if !diags.HasError() {
		t.Fatal("an openclaw desired_config with a native_config block must error")
	}
}

// --- create body -----------------------------------------------------------

func TestBuildBotCreateBody_NativeBot(t *testing.T) {
	plan := botResourceModel()
	plan.Harness = types.StringValue("botyard_native")
	plan.HostingType = types.StringValue("hosted")
	plan.NativeConfig = &botNativeConfigModel{PromptTemplate: types.StringValue("hi")}

	body, diags := buildBotCreateBody(plan)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	got := decodeObj(t, body)
	if string(got["harness"]) != `"botyard_native"` || string(got["hosting_type"]) != `"hosted"` {
		t.Errorf("hosting pair = %s / %s", got["hosting_type"], got["harness"])
	}
	cfg := decodeSub(t, got["config"])
	if string(cfg["bot_type"]) != `"native"` {
		t.Errorf("create config.bot_type = %s, want \"native\"", cfg["bot_type"])
	}
}

// REGRESSION: an OpenClaw bot's create body must be byte-identical to what it
// was before native support existed — no hosting pair keys, no bot_type.
func TestBuildBotCreateBody_OpenClawUnchanged(t *testing.T) {
	plan := botResourceModel()
	plan.Config = &botConfigModel{ThinkingDefault: types.StringValue("high")}

	body, diags := buildBotCreateBody(plan)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	got := decodeObj(t, body)
	for _, k := range []string{"hosting_type", "harness"} {
		if _, present := got[k]; present {
			t.Errorf("undeclared %q must be omitted so the API applies its default, got %s", k, got[k])
		}
	}
	if cfg := decodeSub(t, got["config"]); len(cfg) != 1 || string(cfg["thinking_default"]) != `"high"` {
		t.Errorf("openclaw config payload changed: %s", got["config"])
	}
}

// --- plan-time validation --------------------------------------------------

// validateBotConfig drives ValidateConfig through the real framework path so
// the assertions cover what a practitioner actually hits at plan time.
func validateBotConfig(t *testing.T, model BotResourceModel) diag.Diagnostics {
	t.Helper()
	r := &BotResource{}
	schemaResp := &resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", schemaResp.Diagnostics.Errors())
	}

	raw, diags := tfsdkValueFrom(t, schemaResp.Schema, model)
	if diags.HasError() {
		t.Fatalf("build config value: %v", diags.Errors())
	}
	resp := &resource.ValidateConfigResponse{}
	r.ValidateConfig(context.Background(),
		resource.ValidateConfigRequest{Config: tfsdk.Config{Raw: raw, Schema: schemaResp.Schema}}, resp)
	return resp.Diagnostics
}

func tfsdkValueFrom(t *testing.T, s schema.Schema, model BotResourceModel) (tftypes.Value, diag.Diagnostics) {
	t.Helper()
	state := tfsdk.State{Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil), Schema: s}
	diags := state.Set(context.Background(), model)
	return state.Raw, diags
}

func TestValidateConfig(t *testing.T) {
	openclawCfg := &botConfigModel{ThinkingDefault: types.StringValue("high")}
	nativeCfg := &botNativeConfigModel{PromptTemplate: types.StringValue("x")}

	cases := []struct {
		name      string
		mutate    func(*BotResourceModel)
		wantError bool
	}{
		{"openclaw bot, config block, no harness", func(m *BotResourceModel) {
			m.Config = openclawCfg
		}, false},
		{"openclaw bot, config block, explicit harness", func(m *BotResourceModel) {
			m.Harness = types.StringValue(harnessOpenClaw)
			m.Config = openclawCfg
		}, false},
		{"native bot, native block", func(m *BotResourceModel) {
			m.Harness = types.StringValue(harnessNative)
			m.NativeConfig = nativeCfg
		}, false},
		{"identity only, no blocks", func(m *BotResourceModel) {
			m.Harness = types.StringValue(harnessNative)
		}, false},
		{"both blocks declared", func(m *BotResourceModel) {
			m.Harness = types.StringValue(harnessNative)
			m.Config = openclawCfg
			m.NativeConfig = nativeCfg
		}, true},
		// The silent-drop case: without an explicit harness the API creates an
		// OpenClaw bot and quietly discards the native config.
		{"native block, no harness", func(m *BotResourceModel) {
			m.NativeConfig = nativeCfg
		}, true},
		{"native block, openclaw harness", func(m *BotResourceModel) {
			m.Harness = types.StringValue(harnessOpenClaw)
			m.NativeConfig = nativeCfg
		}, true},
		{"config block, native harness", func(m *BotResourceModel) {
			m.Harness = types.StringValue(harnessNative)
			m.Config = openclawCfg
		}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := botResourceModel()
			tc.mutate(&model)
			diags := validateBotConfig(t, model)
			if got := diags.HasError(); got != tc.wantError {
				t.Fatalf("HasError() = %v, want %v (diags: %v)", got, tc.wantError, diags)
			}
		})
	}
}

// --- clearing a removed prompt_ref (Rivet, PR #24 round 1) -----------------

// REGRESSION: `prompt_ref` is Optional WITHOUT Computed, so removing it from
// the config plans as an explicit null rather than resolving to the prior state
// value. A sparse patch that merely OMITS the key means "no change" to the API
// (core's patch_model applies only what is in model_fields_set), so the old ref
// survives the apply, the refresh restores it over the planned null, and the
// practitioner gets a diff that can never converge — set, remove, and the
// resource is permanently out of sync with no way to clear the field.
func TestBuildNativeConfigPatch_RemovedPromptRefIsExplicitlyCleared(t *testing.T) {
	raw := buildNativeConfigPatch(&botNativeConfigModel{
		PromptTemplate: types.StringValue("x"),
		PromptRef:      types.StringNull(), // removed from the config
	})
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	v, present := got["prompt_ref"]
	if !present {
		t.Fatal("a removed prompt_ref must be sent as an explicit null, not omitted: " +
			"omission means \"no change\", so the server keeps the old ref and the plan never converges")
	}
	if string(v) != "null" {
		t.Fatalf("prompt_ref = %s, want null", v)
	}
}

// The full set -> remove -> refresh lifecycle, which is where the bug actually
// bites: state must end up null, matching the plan, rather than being restored
// to the stale server value.
func TestNativeConfigLifecycle_SetThenRemovePromptRefConverges(t *testing.T) {
	// 1. Declared with a ref. The patch carries it.
	declared := &botNativeConfigModel{
		PromptTemplate: types.StringValue("x"),
		PromptRef:      types.StringValue("prompts/anvil@3"),
	}
	set := decodeSub(t, buildNativeConfigPatch(declared))
	if string(set["prompt_ref"]) != `"prompts/anvil@3"` {
		t.Fatalf("prompt_ref = %s", set["prompt_ref"])
	}

	// 2. Practitioner removes it: the planned value is null.
	removed := &botNativeConfigModel{
		PromptTemplate: types.StringValue("x"),
		PromptRef:      types.StringNull(),
	}
	cleared := decodeSub(t, buildNativeConfigPatch(removed))
	if string(cleared["prompt_ref"]) != "null" {
		t.Fatalf("clearing patch prompt_ref = %s, want null", cleared["prompt_ref"])
	}

	// 3. The API honours the null, so the refreshed config carries no ref, and
	//    the mapped state is null — equal to the plan, hence convergence.
	nc := nativeBotConfig(t, `{"bot_type":"native","model":{"model":"m"},"prompt_template":"x"}`)
	mapNativeBotConfig(nc, removed)
	if !removed.PromptRef.IsNull() {
		t.Fatalf("post-refresh prompt_ref = %q, want null", removed.PromptRef.ValueString())
	}
}

// An UNKNOWN prompt_ref (e.g. interpolated from another resource that has not
// been created yet) must be OMITTED, never nulled — "not resolved yet" is not
// "clear it". Getting this wrong would silently wipe the field on any config
// that computes the ref.
func TestBuildNativeConfigPatch_UnknownPromptRefIsOmittedNotCleared(t *testing.T) {
	raw := buildNativeConfigPatch(&botNativeConfigModel{
		PromptTemplate: types.StringValue("x"),
		PromptRef:      types.StringUnknown(),
	})
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if v, present := got["prompt_ref"]; present {
		t.Fatalf("unknown prompt_ref must be omitted, got %s", v)
	}
}

// prompt_template must NEVER be nulled. It is Optional+Computed so it does not
// plan to null, but the deeper reason is that NativeBotConfig.prompt_template
// is a non-nullable `str` and NativeConfigPatch carries no validator discarding
// a null for it (unlike OpenClaw's _treat_null_system_prompt_mode_as_unset).
// An explicit null would be applied by patch_model onto a field that cannot
// hold it, then dropped at persistence — one value live, another on reload.
func TestBuildNativeConfigPatch_NullPromptTemplateIsOmittedNotNulled(t *testing.T) {
	raw := buildNativeConfigPatch(&botNativeConfigModel{PromptTemplate: types.StringNull()})
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if v, present := got["prompt_template"]; present {
		t.Fatalf("prompt_template must be omitted when null, never sent as null; got %s", v)
	}
}
