package client

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Exercise each known generated variant with its own typed constructor.
func TestSparseNativeConfigVariantRoundTrip(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"empty", `{}`},
		{"images", `{"tool_images":{"enabled":null,"max_dimension_px":1024,"default_detail":"high"}}`},
		{"outer_null", `{"tool_images":null}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var patch NativeConfigPatch
			if err := json.Unmarshal([]byte(tt.body), &patch); err != nil {
				t.Fatal(err)
			}
			var union BotCreate_Config
			if err := union.FromNativeConfigPatch(patch); err != nil {
				t.Fatal(err)
			}
			assertSparseConfigJSON(t, union, tt.body)
		})
	}
}

func TestSparseExternalConfigVariantRoundTrip(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"empty", `{}`},
		{"nested", `{"thinking_default":null,"tool_search":{"mode":"always"},"heartbeat":{}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var patch OpenClawConfigPatch
			if err := json.Unmarshal([]byte(tt.body), &patch); err != nil {
				t.Fatal(err)
			}
			var union BotCreate_Config
			if err := union.FromOpenClawConfigPatch(patch); err != nil {
				t.Fatal(err)
			}
			assertSparseConfigJSON(t, union, tt.body)
		})
	}
}

func assertSparseConfigJSON(t *testing.T, union BotCreate_Config, expected string) {
	t.Helper()
	body, err := json.Marshal(union)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any // Arbitrary JSON values, not a closed config-variant union.
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sparse generated config changed: got %s, want %s", body, expected)
	}
}

func TestSparseConfigVariantsRejectIncorrectLeafTypes(t *testing.T) {
	var patch NativeConfigPatch
	if err := json.Unmarshal([]byte(`{"tool_images":{"enabled":"yes"}}`), &patch); err == nil {
		t.Fatal("generated boolean config leaf accepted a string")
	}
}
