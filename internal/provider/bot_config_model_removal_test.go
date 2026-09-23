package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
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

const botTypeName = "botyard_bot"

func botProtoServer(t *testing.T) tfprotov6.ProviderServer {
	t.Helper()
	srv, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatalf("provider server: %v", err)
	}
	return srv
}

func botSchema(t *testing.T) schema.Schema {
	t.Helper()
	schemaResp := &resource.SchemaResponse{}
	(&BotResource{}).Schema(context.Background(), resource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", schemaResp.Diagnostics.Errors())
	}
	return schemaResp.Schema
}

func botObjectType(t *testing.T) tftypes.Object {
	t.Helper()
	obj, ok := botSchema(t).Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("bot schema is not an object type")
	}
	return obj
}

// objectFromJSON decodes JSON into a value of typ the way Terraform state is
// decoded: absent attributes become null. Unlike the framework's upgrade path
// it does NOT ignore undefined attributes, so a test fixture that names an
// attribute the schema lacks fails loudly instead of being silently dropped.
func objectFromJSON(t *testing.T, typ tftypes.Type, raw string) tftypes.Value {
	t.Helper()
	v, err := tftypes.ValueFromJSONWithOpts([]byte(raw), typ, tftypes.ValueFromJSONOpts{})
	if err != nil {
		t.Fatalf("decoding fixture: %v", err)
	}
	return v
}

// proposedNewState mirrors Terraform core's objchange.ProposedNew for the
// attribute kinds this resource uses: a value set in config wins; a null config
// value inherits the prior value only for a Computed attribute; a declared
// single-nested object is merged attribute by attribute. Building it from
// config and prior state, rather than passing prior state through, is what lets
// the no-op assertion below fail when config and state disagree.
func proposedNewState(t *testing.T, attrs map[string]schema.Attribute, config, prior tftypes.Value) tftypes.Value {
	t.Helper()
	if config.IsNull() {
		return config
	}
	var cfg, old map[string]tftypes.Value
	if err := config.As(&cfg); err != nil {
		t.Fatalf("proposed: config: %v", err)
	}
	if !prior.IsNull() {
		if err := prior.As(&old); err != nil {
			t.Fatalf("proposed: prior: %v", err)
		}
	}
	out := make(map[string]tftypes.Value, len(cfg))
	for name, cv := range cfg {
		pv, hasPrior := old[name]
		if !hasPrior {
			pv = tftypes.NewValue(cv.Type(), nil)
		}
		switch a := attrs[name].(type) {
		case schema.SingleNestedAttribute:
			if cv.IsNull() && a.IsComputed() {
				out[name] = pv
			} else {
				out[name] = proposedNewState(t, a.Attributes, cv, pv)
			}
		default:
			if cv.IsNull() && attrs[name].IsComputed() {
				out[name] = pv
			} else {
				out[name] = cv
			}
		}
	}
	return tftypes.NewValue(config.Type(), out)
}

func dynamicValue(t *testing.T, typ tftypes.Type, v tftypes.Value) *tfprotov6.DynamicValue {
	t.Helper()
	dv, err := tfprotov6.NewDynamicValue(typ, v)
	if err != nil {
		t.Fatalf("dynamic value: %v", err)
	}
	return &dv
}

func requireNoErrorDiags(t *testing.T, step string, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	for _, d := range diags {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			t.Fatalf("%s: unexpected error diagnostic: %s: %s", step, d.Summary, d.Detail)
		}
	}
}

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
