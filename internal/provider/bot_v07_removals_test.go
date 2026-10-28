package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// `config.model` (the `primary { provider, model }` block) was removed from
// botyard_bot. It was inert: the API dropped `model` from OpenClawConfigPatch
// and derives the model chain from the bot's LLM credential links. These tests
// pin the two behaviors an upgrading practitioner meets:
//
//  1. A config without the block validates and plans cleanly.
//  2. Stored state written by v0.4.0 and earlier, which can still carry
//     `config.model.primary`, is handled in a defined way: the attribute is
//     dropped on upgrade and the resulting plan is a no-op.
//
// No ResourceWithUpgradeState is needed for (2), and the schema version stays
// at 0. When the stored state version equals the schema version, the framework
// decodes the raw state with tftypes' IgnoreUndefinedAttributes, which skips
// unknown keys at every nesting level (terraform-plugin-framework
// internal/fwserver/server_upgraderesourcestate.go, and terraform-plugin-go
// tftypes/value_json.go jsonUnmarshalObject). TestBotState_Legacy... proves
// that end to end through the real protocol server rather than trusting the
// framework's source.
//
// A config that still declares `model = { ... }` is NOT rejected. `config` is a
// nested attribute in object syntax, and Terraform core converts the object
// literal to the schema type by discarding undeclared keys before the provider
// is called, so the provider never sees it (verified with Terraform v1.13.3:
// `terraform validate` succeeds with no warning). That is not observable from a
// provider test; CHANGELOG.md tells practitioners to delete the block anyway.

// Protocol helpers (botProtoServer, objectFromJSON, proposedNewState, ...)
// live in bot_protocol_helpers_test.go.

func TestBotSchema_ConfigHasNoModelBlock(t *testing.T) {
	obj := botObjectType(t)
	cfg, ok := obj.AttributeTypes["config"].(tftypes.Object)
	if !ok {
		t.Fatal("config is not an object type")
	}
	if _, ok := cfg.AttributeTypes["model"]; ok {
		t.Fatal("config.model must not exist: the model chain is managed via botyard_bot_credential_assignment")
	}
	// Guard the neighbours so this test cannot pass on an accidentally empty block.
	for _, k := range []string{"system_prompt_mode", "identity", "heartbeat", "compaction", "session"} {
		if _, ok := cfg.AttributeTypes[k]; !ok {
			t.Errorf("config.%s missing", k)
		}
	}
}

// A create with a `config` block that sets neighbouring fields but no model
// block passes ValidateResourceConfig and PlanResourceChange.
func TestBotPlan_ConfigWithoutModelBlockPlansCleanly(t *testing.T) {
	ctx := context.Background()
	srv := botProtoServer(t)
	typ := botObjectType(t)

	config := objectFromJSON(t, typ, `{
		"name": "Research Assistant",
		"config": {
			"thinking_default": "high",
			"identity": {"emoji": "🔬", "theme": "dark"}
		}
	}`)

	vResp, err := srv.ValidateResourceConfig(ctx, &tfprotov6.ValidateResourceConfigRequest{
		TypeName: botTypeName,
		Config:   dynamicValue(t, typ, config),
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	requireNoErrorDiags(t, "validate", vResp.Diagnostics)

	pResp, err := srv.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
		TypeName:         botTypeName,
		PriorState:       dynamicValue(t, typ, tftypes.NewValue(typ, nil)),
		ProposedNewState: dynamicValue(t, typ, config),
		Config:           dynamicValue(t, typ, config),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	requireNoErrorDiags(t, "plan", pResp.Diagnostics)
	if pResp.PlannedState == nil {
		t.Fatal("plan returned no planned state")
	}
	planned, err := pResp.PlannedState.Unmarshal(typ)
	if err != nil {
		t.Fatalf("decoding planned state: %v", err)
	}
	var attrs map[string]tftypes.Value
	if err := planned.As(&attrs); err != nil {
		t.Fatalf("planned state: %v", err)
	}
	var name string
	if err := attrs["name"].As(&name); err != nil || name != "Research Assistant" {
		t.Errorf("planned name = %q (err %v)", name, err)
	}
}

// legacyBotState is the shape v0.4.0 wrote for a bot whose practitioner had
// declared `config.model.primary`. Everything except `model` is valid today.
const legacyBotState = `{
	"id": "bot_123",
	"slug": "research-assistant",
	"name": "Research Assistant",
	"hosting_type": "hosted",
	"harness": "openclaw",
	"config": {
		"system_prompt_mode": "botyard",
		"thinking_default": "high",
		"reasoning_default": null,
		"model": {"primary": {"provider": "botyard", "model": "gpt-5.4"}},
		"identity": {"emoji": "🔬", "theme": "dark"},
		"heartbeat": null,
		"compaction": null,
		"session": null
	}
}`

func TestBotState_LegacyModelBlockIsDroppedOnUpgrade(t *testing.T) {
	ctx := context.Background()
	srv := botProtoServer(t)
	typ := botObjectType(t)

	// Positive control: the fixture really carries the legacy attribute, and the
	// current schema really does not accept it under a strict decode. Without
	// this, a typo in the fixture would make the test pass vacuously.
	var probe struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal([]byte(legacyBotState), &probe); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if _, ok := probe.Config["model"]; !ok {
		t.Fatal("fixture must carry config.model")
	}
	if _, err := tftypes.ValueFromJSONWithOpts([]byte(legacyBotState), typ, tftypes.ValueFromJSONOpts{}); err == nil ||
		!strings.Contains(err.Error(), `unsupported attribute "model"`) {
		t.Fatalf("strict decode should reject config.model, got err = %v", err)
	}

	uResp, err := srv.UpgradeResourceState(ctx, &tfprotov6.UpgradeResourceStateRequest{
		TypeName: botTypeName,
		Version:  0,
		RawState: &tfprotov6.RawState{JSON: []byte(legacyBotState)},
	})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	requireNoErrorDiags(t, "upgrade", uResp.Diagnostics)
	if uResp.UpgradedState == nil {
		t.Fatal("upgrade returned no state")
	}
	upgraded, err := uResp.UpgradedState.Unmarshal(typ)
	if err != nil {
		t.Fatalf("decoding upgraded state: %v", err)
	}

	// Every other value survives the upgrade unchanged.
	stripped := strings.Replace(legacyBotState,
		`"model": {"primary": {"provider": "botyard", "model": "gpt-5.4"}},`, "", 1)
	want := objectFromJSON(t, typ, stripped)
	if !upgraded.Equal(want) {
		t.Fatalf("upgraded state differs from the legacy state minus config.model:\n got: %s\nwant: %s", upgraded, want)
	}

	// The practitioner deletes the block, as the upgrade note says, and leaves
	// the rest of their config as it was. The plan must be a no-op: no diff, no
	// replacement.
	config := objectFromJSON(t, typ, `{
		"name": "Research Assistant",
		"config": {
			"thinking_default": "high",
			"identity": {"emoji": "🔬", "theme": "dark"}
		}
	}`)
	pResp, err := srv.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
		TypeName:         botTypeName,
		PriorState:       uResp.UpgradedState,
		ProposedNewState: dynamicValue(t, typ, proposedNewState(t, botSchema(t).Attributes, config, upgraded)),
		Config:           dynamicValue(t, typ, config),
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	requireNoErrorDiags(t, "plan", pResp.Diagnostics)
	planned, err := pResp.PlannedState.Unmarshal(typ)
	if err != nil {
		t.Fatalf("decoding planned state: %v", err)
	}
	if !planned.Equal(upgraded) {
		t.Errorf("plan after upgrade is not a no-op:\n got: %s\nwant: %s", planned, upgraded)
	}
	if len(pResp.RequiresReplace) != 0 {
		t.Errorf("plan after upgrade requires replacement of %v", pResp.RequiresReplace)
	}
}

// legacyNativeBotState is the shape v0.6.0 wrote for a native bot: both
// removed prompt attributes are present, normally null. v0.5.0 could also have
// stored the server's old template in prompt_template, so the test covers a
// non-null value too. Every other attribute is valid today.
func legacyNativeBotState(promptTemplate string) string {
	return `{
	"id": "bot_456",
	"slug": "native-probe",
	"name": "native-probe",
	"hosting_type": "hosted",
	"harness": "botyard_native",
	"native_config": {
		"prompt_template": ` + promptTemplate + `,
		"prompt_ref": null,
		"tool_search": {"mode": "auto", "auto_threshold_percent": 40},
		"model_provider": "botyard",
		"model_name": "m",
		"host_policy_json": null
	}
}`
}

func TestBotState_RemovedNativePromptFieldsDroppedOnUpgrade(t *testing.T) {
	for name, tmpl := range map[string]string{
		"v0.6.0 null":         `null`,
		"v0.5.0 non-null":     `"legacy server-side soul"`,
		"v0.5.1 empty string": `""`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			srv := botProtoServer(t)
			typ := botObjectType(t)
			legacy := legacyNativeBotState(tmpl)

			// Positive control: a strict decode must reject the fixture, so it
			// really carries an attribute the schema no longer has.
			if _, err := tftypes.ValueFromJSONWithOpts([]byte(legacy), typ, tftypes.ValueFromJSONOpts{}); err == nil ||
				!strings.Contains(err.Error(), `unsupported attribute "prompt_`) {
				t.Fatalf("strict decode should reject native_config.prompt_*, got err = %v", err)
			}

			uResp, err := srv.UpgradeResourceState(ctx, &tfprotov6.UpgradeResourceStateRequest{
				TypeName: botTypeName,
				Version:  0,
				RawState: &tfprotov6.RawState{JSON: []byte(legacy)},
			})
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			requireNoErrorDiags(t, "upgrade", uResp.Diagnostics)
			upgraded, err := uResp.UpgradedState.Unmarshal(typ)
			if err != nil {
				t.Fatalf("decoding upgraded state: %v", err)
			}

			stripped := strings.Replace(strings.Replace(legacy,
				`"prompt_template": `+tmpl+`,`, "", 1), `"prompt_ref": null,`, "", 1)
			want := objectFromJSON(t, typ, stripped)
			if !upgraded.Equal(want) {
				t.Fatalf("upgraded state differs from the legacy state minus prompt_*:\n got: %s\nwant: %s", upgraded, want)
			}

			config := objectFromJSON(t, typ, `{
				"name": "native-probe",
				"hosting_type": "hosted",
				"harness": "botyard_native",
				"native_config": {"tool_search": {"mode": "auto"}}
			}`)
			pResp, err := srv.PlanResourceChange(ctx, &tfprotov6.PlanResourceChangeRequest{
				TypeName:         botTypeName,
				PriorState:       uResp.UpgradedState,
				ProposedNewState: dynamicValue(t, typ, proposedNewState(t, botSchema(t).Attributes, config, upgraded)),
				Config:           dynamicValue(t, typ, config),
			})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			requireNoErrorDiags(t, "plan", pResp.Diagnostics)
			planned, err := pResp.PlannedState.Unmarshal(typ)
			if err != nil {
				t.Fatalf("decoding planned state: %v", err)
			}
			if !planned.Equal(upgraded) {
				t.Errorf("plan after upgrade is not a no-op:\n got: %s\nwant: %s", planned, upgraded)
			}
			if len(pResp.RequiresReplace) != 0 {
				t.Errorf("plan after upgrade requires replacement of %v", pResp.RequiresReplace)
			}
		})
	}
}

func TestBotSchema_NativeConfigHasNoPromptAttributes(t *testing.T) {
	nc, ok := botObjectType(t).AttributeTypes["native_config"].(tftypes.Object)
	if !ok {
		t.Fatal("native_config is not an object type")
	}
	for _, k := range []string{"prompt_template", "prompt_ref"} {
		if _, ok := nc.AttributeTypes[k]; ok {
			t.Errorf("native_config.%s must not exist (removed in v0.7.0)", k)
		}
	}
	for _, k := range []string{"tool_search", "tool_images", "model_name"} {
		if _, ok := nc.AttributeTypes[k]; !ok {
			t.Errorf("native_config.%s missing", k)
		}
	}
}
