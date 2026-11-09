package client

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Exercise the generated variants themselves, then their typed union APIs.
// Every field must survive omit/value/null without an opaque union reader.
func TestSparseConfigVariantsRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name  string
		body  string
		patch any
	}{
		{"native_empty", `{}`, &NativeConfigPatch{}},
		{"native_images", `{"tool_images":{"enabled":null,"max_dimension_px":1024,"default_detail":"high"}}`, &NativeConfigPatch{}},
		{"native_outer_null", `{"tool_images":null}`, &NativeConfigPatch{}},
		{"external_empty", `{}`, &OpenClawConfigPatch{}},
		{"external_nested", `{"thinking_default":null,"tool_search":{"mode":"always"},"heartbeat":{}}`, &OpenClawConfigPatch{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tt.body), tt.patch); err != nil {
				t.Fatal(err)
			}
			var union BotCreate_Config
			var err error
			switch patch := tt.patch.(type) {
			case *NativeConfigPatch:
				err = union.FromNativeConfigPatch(*patch)
			case *OpenClawConfigPatch:
				err = union.FromOpenClawConfigPatch(*patch)
			default:
				t.Fatalf("unsupported test variant %T", patch)
			}
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(union)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tt.body), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("sparse generated config changed: got %s, want %s", body, tt.body)
			}
		})
	}
}

func TestSparseConfigVariantsRejectIncorrectLeafTypes(t *testing.T) {
	var patch NativeConfigPatch
	if err := json.Unmarshal([]byte(`{"tool_images":{"enabled":"yes"}}`), &patch); err == nil {
		t.Fatal("generated boolean config leaf accepted a string")
	}
}
