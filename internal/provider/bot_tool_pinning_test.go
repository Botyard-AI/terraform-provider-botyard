package provider

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// patchKeys decodes a sparse patch into its raw leaves, for exact-byte asserts.
func patchKeys(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode patch %s: %v", raw, err)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = string(v)
	}
	return out
}

func TestBuildToolPinningPatch_UnsetBlockSendsNothing(t *testing.T) {
	if _, ok := buildToolPinningPatch(nil); ok {
		t.Fatal("an undeclared tool_pinning block must send nothing")
	}
	empty := &botToolPinningModel{
		Limit:       types.Int64Null(),
		NeverPin:    types.ListNull(types.StringType),
		OwnerPins:   types.ListNull(types.StringType),
		TokenBudget: types.Int64Null(),
	}
	if _, ok := buildToolPinningPatch(empty); ok {
		t.Fatal("a declared block with every leaf null must send nothing: null means no change on the wire")
	}
}

func TestBuildToolPinningPatch_SendsOnlyDeclaredLeaves(t *testing.T) {
	raw, ok := buildToolPinningPatch(&botToolPinningModel{
		Limit:       types.Int64Value(10),
		NeverPin:    types.ListNull(types.StringType),
		OwnerPins:   types.ListNull(types.StringType),
		TokenBudget: types.Int64Unknown(),
	})
	if !ok {
		t.Fatal("expected a patch")
	}
	got := patchKeys(t, raw)
	if len(got) != 1 || got["limit"] != "10" {
		t.Fatalf("patch = %v, want only limit=10 (an unknown or null leaf is left unchanged)", got)
	}
}

// token_budget = 0 is the wire's clear, so it must be sent, not dropped as zero.
func TestBuildToolPinningPatch_ZeroBudgetClears(t *testing.T) {
	raw, ok := buildToolPinningPatch(&botToolPinningModel{
		Limit:       types.Int64Null(),
		NeverPin:    types.ListNull(types.StringType),
		OwnerPins:   types.ListNull(types.StringType),
		TokenBudget: types.Int64Value(0),
	})
	if !ok {
		t.Fatal("expected a patch")
	}
	if got := patchKeys(t, raw); got["token_budget"] != "0" {
		t.Fatalf("token_budget = %v, want 0 sent to clear the budget", got)
	}
}

// An empty list is a real value: [] clears the stored list, while a null list is
// no change.
func TestBuildToolPinningPatch_EmptyListClearsAndNullListIsNoChange(t *testing.T) {
	raw, ok := buildToolPinningPatch(&botToolPinningModel{
		Limit:       types.Int64Null(),
		NeverPin:    types.ListValueMust(types.StringType, nil),
		OwnerPins:   types.ListNull(types.StringType),
		TokenBudget: types.Int64Null(),
	})
	if !ok {
		t.Fatal("expected a patch")
	}
	got := patchKeys(t, raw)
	if got["never_pin"] != "[]" {
		t.Fatalf("never_pin = %v, want [] sent", got)
	}
	if _, present := got["owner_pins"]; present {
		t.Fatalf("owner_pins was null in config, so it must be omitted: %v", got)
	}
}

func TestBuildToolPinningPatch_ListsKeepTheirOrder(t *testing.T) {
	raw, ok := buildToolPinningPatch(&botToolPinningModel{
		Limit: types.Int64Null(),
		NeverPin: types.ListValueMust(types.StringType, []attr.Value{
			types.StringValue("zeta"), types.StringValue("alpha"),
		}),
		OwnerPins:   types.ListNull(types.StringType),
		TokenBudget: types.Int64Null(),
	})
	if !ok {
		t.Fatal("expected a patch")
	}
	if got := patchKeys(t, raw); got["never_pin"] != `["zeta","alpha"]` {
		t.Fatalf("never_pin = %v, want the declared order", got)
	}
}

// A declared block that the server answers with nothing reads back the API's
// defaults: limit 20, no budget, empty lists, never null.
func TestMapToolPinning_AbsentServerFieldsReadBackDefaults(t *testing.T) {
	tp := &botToolPinningModel{}
	mapToolPinning(nil, tp)

	if tp.Limit.ValueInt64() != 20 || tp.Limit.IsNull() || tp.Limit.IsUnknown() {
		t.Errorf("limit = %v, want the default 20", tp.Limit)
	}
	if tp.TokenBudget.ValueInt64() != 0 || tp.TokenBudget.IsNull() {
		t.Errorf("token_budget = %v, want 0 (no budget)", tp.TokenBudget)
	}
	for name, l := range map[string]types.List{"never_pin": tp.NeverPin, "owner_pins": tp.OwnerPins} {
		if l.IsNull() || l.IsUnknown() || len(l.Elements()) != 0 {
			t.Errorf("%s = %v, want a known empty list", name, l)
		}
	}
}

func TestMapToolPinning_MapsServerValues(t *testing.T) {
	limit, budget := 8, 4000
	never := []string{"blocked"}
	owner := []string{"pinned"}
	tp := &botToolPinningModel{}
	mapToolPinning(&client.ToolPinningConfig{
		Limit:       &limit,
		TokenBudget: &budget,
		NeverPin:    &never,
		OwnerPins:   &owner,
	}, tp)

	if tp.Limit.ValueInt64() != 8 || tp.TokenBudget.ValueInt64() != 4000 {
		t.Fatalf("limit=%v budget=%v, want 8 and 4000", tp.Limit, tp.TokenBudget)
	}
	if got := tp.NeverPin.Elements(); len(got) != 1 || got[0].String() != `"blocked"` {
		t.Errorf("never_pin = %v", tp.NeverPin)
	}
	if got := tp.OwnerPins.Elements(); len(got) != 1 || got[0].String() != `"pinned"` {
		t.Errorf("owner_pins = %v", tp.OwnerPins)
	}
}

// The native patch carries tool_pinning beside its siblings, and an undeclared
// block adds no key.
func TestBuildNativeConfigPatch_CarriesToolPinning(t *testing.T) {
	raw := buildNativeConfigPatch(&botNativeConfigModel{
		ToolPinning: &botToolPinningModel{
			Limit:       types.Int64Value(12),
			NeverPin:    types.ListNull(types.StringType),
			OwnerPins:   types.ListNull(types.StringType),
			TokenBudget: types.Int64Value(0),
		},
	})
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got["bot_type"]) != `"native"` {
		t.Fatalf("bot_type = %s", got["bot_type"])
	}
	if want := `{"limit":12,"token_budget":0}`; string(got["tool_pinning"]) != want {
		t.Fatalf("tool_pinning = %s, want %s", got["tool_pinning"], want)
	}

	bare := buildNativeConfigPatch(&botNativeConfigModel{})
	if _, ok := patchKeys(t, bare)["tool_pinning"]; ok {
		t.Fatal("an undeclared tool_pinning block must not add a key")
	}
}
