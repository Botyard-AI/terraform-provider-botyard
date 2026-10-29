package provider

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Regression: adding a nested config block to an EXISTING bot failed apply with
// "Provider produced inconsistent result after apply: ... was null, but now
// ...". The stock UseStateForUnknown copies the absent parent's null into the
// block's Optional+Computed leaves; the server then returns its defaults. See
// nested_plan_modifiers.go.

// planBotUpdate plans `config` against `prior` through the protocol server and
// returns the planned state.
func planBotUpdate(t *testing.T, prior, config string) tftypes.Value {
	t.Helper()
	ctx := context.Background()
	srv := botProtoServer(t)
	typ := botObjectType(t)
	priorV := objectFromJSON(t, typ, prior)
	configV := objectFromJSON(t, typ, config)
	resp, err := srv.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
		TypeName:         botTypeName,
		PriorState:       dynamicValue(t, typ, priorV),
		ProposedNewState: dynamicValue(t, typ, proposedNewState(t, botSchema(t).Attributes, configV, priorV)),
		Config:           dynamicValue(t, typ, configV),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	requireNoErrorDiags(t, "plan", resp.Diagnostics)
	planned, err := resp.PlannedState.Unmarshal(typ)
	if err != nil {
		t.Fatalf("decoding planned state: %v", err)
	}
	return planned
}

func attrAt(t *testing.T, v tftypes.Value, steps ...string) tftypes.Value {
	t.Helper()
	p := tftypes.NewAttributePath()
	for _, s := range steps {
		p = p.WithAttributeName(s)
	}
	got, _, err := tftypes.WalkAttributePath(v, p)
	if err != nil {
		t.Fatalf("walk %v: %v", steps, err)
	}
	tv, ok := got.(tftypes.Value)
	if !ok {
		t.Fatalf("%v is not a value", steps)
	}
	return tv
}

const openClawBotPrior = `{
	"id": "b-1", "slug": "oc", "name": "oc", "hosting_type": "hosted", "harness": "openclaw",
	"description": null,
	"config": {
		"system_prompt_mode": "botyard", "thinking_default": "high", "reasoning_default": "off",
		"identity": null, "heartbeat": null, "session": null,
		"compaction": %s
	}
}`

func TestPlan_ConfigCompactionAddedToExistingBot_LeavesUnknown(t *testing.T) {
	prior := fmt.Sprintf(openClawBotPrior, "null")
	config := `{"name": "oc", "config": {"thinking_default": "high", "compaction": {"reserve_tokens": 24000}}}`
	planned := planBotUpdate(t, prior, config)

	if v := attrAt(t, planned, "config", "compaction", "reserve_tokens"); !v.IsKnown() || v.IsNull() {
		t.Errorf("configured reserve_tokens = %s, want 24000", v)
	}
	// The unset Optional+Computed leaves of the NEW block must be unknown, not
	// the null copied from the absent parent.
	for _, leaf := range []string{"mid_turn_precheck", "timeout_seconds", "truncate_after_compaction"} {
		if v := attrAt(t, planned, "config", "compaction", leaf); v.IsKnown() {
			t.Errorf("config.compaction.%s planned as %s, want unknown", leaf, v)
		}
	}
	// Leaves whose parent already existed keep their prior value.
	if v := attrAt(t, planned, "config", "system_prompt_mode"); !v.Equal(tftypes.NewValue(tftypes.String, "botyard")) {
		t.Errorf("system_prompt_mode planned as %s, want prior \"botyard\"", v)
	}
}

func TestPlan_ConfigCompactionUnchanged_KeepsPriorValues(t *testing.T) {
	prior := fmt.Sprintf(openClawBotPrior, `{"max_active_transcript_bytes": 1, "mid_turn_precheck": true,
		"reserve_tokens": 24000, "reserve_tokens_floor": 2, "timeout_seconds": 3, "truncate_after_compaction": false}`)
	// A sibling change forces a non-empty plan, so MarkComputedNilsAsUnknown runs.
	config := `{"name": "oc", "config": {"thinking_default": "low", "compaction": {"reserve_tokens": 24000}}}`
	planned := planBotUpdate(t, prior, config)
	if v := attrAt(t, planned, "config", "compaction", "mid_turn_precheck"); !v.Equal(tftypes.NewValue(tftypes.Bool, true)) {
		t.Errorf("mid_turn_precheck planned as %s, want prior true", v)
	}
	if v := attrAt(t, planned, "config", "compaction", "timeout_seconds"); !v.IsKnown() {
		t.Errorf("timeout_seconds planned unknown, want prior 3")
	}
}

// The whole `config` block added to an existing bot: the parent of its
// top-level leaves is `config` itself, which is null in prior state.
func TestPlan_ConfigBlockAddedToExistingBot_LeavesUnknown(t *testing.T) {
	prior := `{"id": "b-1", "slug": "oc", "name": "oc", "hosting_type": "hosted", "harness": "openclaw", "config": null}`
	planned := planBotUpdate(t, prior, `{"name": "oc", "config": {"thinking_default": "high"}}`)
	if v := attrAt(t, planned, "config", "system_prompt_mode"); v.IsKnown() {
		t.Errorf("system_prompt_mode planned as %s, want unknown", v)
	}
}

// Real Terraform core, native bot: create without tool_search, then add it.
// The fake returns auto_threshold_percent = 40, which the stock modifier had
// planned as null.
func TestNativeToolSearch_AddedToExistingBot(t *testing.T) {
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", "botyard-ai")
	_, srv := newFakeNativeBotAPI(t)
	withTS := nativePromptTestConfig(srv.URL, `tool_search = { mode = "always" }`)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: nativePromptTestConfig(srv.URL, ``)},
			{
				Config: withTS,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("botyard_bot.native", "native_config.tool_search.mode", "always"),
					resource.TestCheckResourceAttr("botyard_bot.native", "native_config.tool_search.auto_threshold_percent", "40"),
				),
			},
			{Config: withTS, PlanOnly: true},
		},
	})
}

// config.model.primary.provider is the one nested Optional+Computed leaf that
// Read never refreshes (the API dropped `model`; mapBotConfig leaves it as
// planned). It must therefore keep the STOCK modifier: added to an existing bot
// with `provider` omitted, the plan has to hold a known null, because an
// unknown would survive into the state Update returns and fail the apply with
// "Provider returned invalid result object after apply". Found by Rivet in
// review of #40.
func TestPlan_ConfigModelAddedToExistingBot_ProviderKnown(t *testing.T) {
	prior := fmt.Sprintf(openClawBotPrior, "null")
	config := `{"name": "oc", "config": {"thinking_default": "high", "model": {"primary": {"model": "gpt-5.4"}}}}`
	planned := planBotUpdate(t, prior, config)
	if v := attrAt(t, planned, "config", "model", "primary", "provider"); !v.IsKnown() {
		t.Fatalf("config.model.primary.provider planned unknown; Read never refreshes it, so apply would fail")
	}
}
