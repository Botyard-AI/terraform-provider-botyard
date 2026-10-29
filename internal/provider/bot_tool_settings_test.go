package provider

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// tool_images is the one config block whose declared leaves are sent as an
// explicit null when unset, because null resets the field to the platform
// default. These pin that wire shape.
func TestBuildToolImagesPatch_UnsetLeavesAreExplicitNull(t *testing.T) {
	got := decodeSub(t, buildNativeConfigPatch(&botNativeConfigModel{
		ToolImages: &botToolImagesModel{
			Enabled:        types.BoolNull(),
			MaxDimensionPx: types.Int64Value(1024),
			DefaultDetail:  types.StringNull(),
		},
	}))
	ti := decodeSub(t, got["tool_images"])
	want := map[string]string{"enabled": "null", "max_dimension_px": "1024", "default_detail": "null"}
	if len(ti) != len(want) {
		t.Fatalf("tool_images = %s, want exactly %v", got["tool_images"], want)
	}
	for k, v := range want {
		if string(ti[k]) != v {
			t.Errorf("tool_images.%s = %s, want %s", k, ti[k], v)
		}
	}
}

// An empty declared block is the documented "reset everything" gesture.
func TestBuildToolImagesPatch_EmptyBlockResetsAll(t *testing.T) {
	got := decodeSub(t, buildNativeConfigPatch(&botNativeConfigModel{ToolImages: &botToolImagesModel{
		Enabled: types.BoolNull(), MaxDimensionPx: types.Int64Null(), DefaultDetail: types.StringNull(),
	}}))
	if s := string(got["tool_images"]); s != `{"default_detail":null,"enabled":null,"max_dimension_px":null}` {
		t.Fatalf("tool_images = %s", s)
	}
}

// An undeclared block is not sent at all: the API reads an omitted
// tool_images as "no change", so UI-set values survive.
func TestBuildToolImagesPatch_UndeclaredIsOmitted(t *testing.T) {
	got := decodeSub(t, buildNativeConfigPatch(&botNativeConfigModel{}))
	if v, sent := got["tool_images"]; sent {
		t.Fatalf("undeclared tool_images sent as %s", v)
	}
}

// Unknown leaves are omitted rather than nulled, so an unknown can never be
// mistaken for "reset".
func TestBuildToolImagesPatch_UnknownLeafOmitted(t *testing.T) {
	got := decodeSub(t, buildNativeConfigPatch(&botNativeConfigModel{ToolImages: &botToolImagesModel{
		Enabled: types.BoolUnknown(), MaxDimensionPx: types.Int64Null(), DefaultDetail: types.StringValue("low"),
	}}))
	ti := decodeSub(t, got["tool_images"])
	if _, sent := ti["enabled"]; sent {
		t.Errorf("unknown enabled was sent as %s", ti["enabled"])
	}
	if string(ti["default_detail"]) != `"low"` || string(ti["max_dimension_px"]) != "null" {
		t.Errorf("tool_images = %s", got["tool_images"])
	}
}

func TestMapNativeBotConfig_ToolImages(t *testing.T) {
	cases := map[string]struct {
		raw                    string
		enabled, maxPx, detail string
	}{
		"all set": {`{"model":{"model":"m"},"tool_images":{"enabled":false,"max_dimension_px":1024,"default_detail":"high"}}`,
			"false", "1024", `"high"`},
		"server nulls": {`{"model":{"model":"m"},"tool_images":{"enabled":null,"max_dimension_px":null,"default_detail":null}}`,
			"<null>", "<null>", "<null>"},
		"absent on an older API": {`{"model":{"model":"m"}}`, "<null>", "<null>", "<null>"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var nc client.NativeBotConfig
			if err := json.Unmarshal([]byte(tc.raw), &nc); err != nil {
				t.Fatal(err)
			}
			// Prior values must be overwritten, nulls included: the leaves are
			// not Computed, so state must equal what the server holds.
			cfg := &botNativeConfigModel{ToolImages: &botToolImagesModel{
				Enabled: types.BoolValue(true), MaxDimensionPx: types.Int64Value(7), DefaultDetail: types.StringValue("low"),
			}}
			mapNativeBotConfig(&nc, cfg)
			ti := cfg.ToolImages
			got := [3]string{ti.Enabled.String(), ti.MaxDimensionPx.String(), ti.DefaultDetail.String()}
			if want := [3]string{tc.enabled, tc.maxPx, tc.detail}; got != want {
				t.Errorf("tool_images = %v, want %v", got, want)
			}
		})
	}
}

func TestMapNativeBotConfig_UndeclaredToolImagesNotPopulated(t *testing.T) {
	var nc client.NativeBotConfig
	if err := json.Unmarshal([]byte(`{"model":{"model":"m"},"tool_images":{"enabled":false}}`), &nc); err != nil {
		t.Fatal(err)
	}
	cfg := &botNativeConfigModel{}
	mapNativeBotConfig(&nc, cfg)
	if cfg.ToolImages != nil {
		t.Fatalf("undeclared tool_images populated: %+v", cfg.ToolImages)
	}
}

// --- config.tool_search (OpenClaw) ------------------------------------------

func TestBuildBotConfigPatch_ToolSearch(t *testing.T) {
	got := decodeSub(t, buildBotConfigPatch(&botConfigModel{
		ToolSearch: &botToolSearchModel{Mode: types.StringValue("off"), AutoThresholdPercent: types.Int64Null()},
	}))
	if s := string(got["tool_search"]); s != `{"mode":"off"}` {
		t.Fatalf("tool_search = %s, want sparse {\"mode\":\"off\"}", s)
	}
	if got := decodeSub(t, buildBotConfigPatch(&botConfigModel{})); got["tool_search"] != nil {
		t.Fatalf("undeclared tool_search sent: %s", got["tool_search"])
	}
}

func TestMapBotConfig_ToolSearch(t *testing.T) {
	var oc client.OpenClawBotConfig
	if err := json.Unmarshal([]byte(`{"gateway_token":"g","identity":{},
		"tool_search":{"mode":"always","auto_threshold_percent":10}}`), &oc); err != nil {
		t.Fatal(err)
	}
	cfg := &botConfigModel{ToolSearch: &botToolSearchModel{}}
	mapBotConfig(&oc, cfg)
	if cfg.ToolSearch.Mode.ValueString() != "always" || cfg.ToolSearch.AutoThresholdPercent.ValueInt64() != 10 {
		t.Fatalf("tool_search = %+v", cfg.ToolSearch)
	}

	undeclared := &botConfigModel{}
	mapBotConfig(&oc, undeclared)
	if undeclared.ToolSearch != nil {
		t.Fatal("undeclared config.tool_search populated from server defaults")
	}
}
