package provider

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Keep existing byte-level wire assertions while production uses the generated
// request and typed client directly. No handwritten BotCreate shape lives here.
func buildBotCreateBody(plan BotResourceModel) ([]byte, diag.Diagnostics) {
	request, diags := buildBotCreateRequest(plan)
	if diags.HasError() {
		return nil, diags
	}
	body, err := json.Marshal(request)
	if err != nil {
		diags.AddError("Error encoding bot", err.Error())
	}
	return body, diags
}

func TestNativeDefault_GeneratedCreateRequestPreservesSparseWire(t *testing.T) {
	for _, external := range []bool{false, true} {
		name := "native"
		if external {
			name = "external"
		}
		t.Run(name, func(t *testing.T) {
			plan := botResourceModel()
			hosting, harness := "hosted", harnessNative
			if external {
				hosting, harness = "self_hosted", "claude_code"
				plan.HostingType = types.StringValue(hosting)
				plan.Harness = types.StringValue(harness)
			}
			body, diags := buildBotCreateBody(plan)
			if diags.HasError() {
				t.Fatal(diags)
			}
			got := decodeObj(t, body)
			if jsonStr(t, got["hosting_type"]) != hosting || jsonStr(t, got["harness"]) != harness {
				t.Fatalf("runtime body=%s", body)
			}
			for _, key := range []string{"credentials", "resources", "files", "skill_ids", "tool_ids"} {
				if _, present := got[key]; present {
					t.Fatalf("undeclared %s must be omitted, body=%s", key, body)
				}
			}
			if string(got["description"]) != "null" || string(got["avatar_url"]) != "null" {
				t.Fatalf("metadata nulls lost: %s", body)
			}
			cfg := decodeObj(t, got["config"])
			if len(cfg) != 0 {
				t.Fatalf("undeclared config must stay empty: %s", body)
			}
		})
	}
}

func TestNativeDefault_GeneratedCreateRequestKeepsConfigNullAndOmission(t *testing.T) {
	plan := botResourceModel()
	plan.NativeConfig = &botNativeConfigModel{ToolImages: &botToolImagesModel{
		Enabled: types.BoolNull(), MaxDimensionPx: types.Int64Value(1024), DefaultDetail: types.StringUnknown(),
	}}
	body, diags := buildBotCreateBody(plan)
	if diags.HasError() {
		t.Fatal(diags)
	}
	cfg := decodeObj(t, decodeObj(t, body)["config"])
	if jsonStr(t, cfg["bot_type"]) != "native" {
		t.Fatalf("config=%s", body)
	}
	for _, key := range []string{"model", "host_policy", "tool_search"} {
		if _, present := cfg[key]; present {
			t.Fatalf("unowned %s must be omitted: %s", key, body)
		}
	}
	images := decodeObj(t, cfg["tool_images"])
	if string(images["enabled"]) != "null" || string(images["max_dimension_px"]) != "1024" {
		t.Fatalf("image reset/value semantics changed: %s", body)
	}
	if _, present := images["default_detail"]; present {
		t.Fatalf("unknown image leaf must stay omitted: %s", body)
	}
}
