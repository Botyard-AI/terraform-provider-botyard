package provider

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestDirectCreateConfigExternalProjection(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config *botConfigModel
	}{
		{"omitted", nil},
		{"empty_nested", &botConfigModel{Identity: &botIdentityModel{}, Heartbeat: &botHeartbeatModel{}, Compaction: &botCompactionModel{}, Session: &botSessionModel{}, ToolSearch: &botToolSearchModel{}}},
		{"configured", &botConfigModel{
			SystemPromptMode: types.StringValue("botyard"),
			ThinkingDefault:  types.StringValue("high"),
			ReasoningDefault: types.StringValue("on"),
			Identity:         &botIdentityModel{Emoji: types.StringValue("robot"), Theme: types.StringUnknown()},
			Heartbeat: &botHeartbeatModel{
				Every: types.StringValue("0m"), AckMaxChars: types.Int64Value(0),
				IncludeReasoning: types.BoolValue(false), IsolatedSession: types.BoolValue(true),
				LightContext: types.BoolValue(false), Model: types.StringUnknown(), Prompt: types.StringNull(),
				ActiveHours: &botActiveHoursModel{FromTime: types.StringValue("09:00"), ToTime: types.StringValue("17:00"), Timezone: types.StringValue("UTC")},
			},
			Compaction: &botCompactionModel{
				MaxActiveTranscriptBytes: types.Int64Value(1024), MidTurnPrecheck: types.BoolValue(false),
				ReserveTokens: types.Int64Value(0), ReserveTokensFloor: types.Int64Value(0),
				TimeoutSeconds: types.Int64Value(60), TruncateAfterCompaction: types.BoolValue(true),
			},
			Session:    &botSessionModel{WriteLockMaxHoldMs: types.Int64Value(0)},
			ToolSearch: &botToolSearchModel{Mode: types.StringValue("always"), AutoThresholdPercent: types.Int64Value(50)},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan := botResourceModel()
			plan.Config = tt.config
			union, err := buildTypedCreateConfig(plan, client.BotHarnessClaudeCode)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(union)
			if err != nil {
				t.Fatal(err)
			}
			// The historical PATCH projection is a test oracle only. Creation
			// produces generated fields directly and never serializes this map.
			var got, want any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(buildBotConfigPatch(tt.config), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("direct generated projection changed sparse semantics: got %s, want %s", body, buildBotConfigPatch(tt.config))
			}
		})
	}
}

func TestDirectCreateConfigNativeIgnoresComputedSettings(t *testing.T) {
	plan := botResourceModel()
	plan.NativeConfig = &botNativeConfigModel{
		ModelProvider:  types.StringValue("operator-provider"),
		ModelName:      types.StringValue("operator-model"),
		HostPolicyJSON: types.StringValue(`{"service_owner":"native"}`),
		ToolSearch:     &botToolSearchModel{Mode: types.StringUnknown(), AutoThresholdPercent: types.Int64Null()},
	}
	union, err := buildTypedCreateConfig(plan, client.BotHarnessBotyardNative)
	if err != nil {
		t.Fatal(err)
	}
	variant, err := union.AsNativeConfigPatch()
	if err != nil {
		t.Fatal(err)
	}
	if variant.Model.IsSpecified() || variant.HostPolicy.IsSpecified() || variant.ToolSearch.IsSpecified() {
		t.Fatalf("unowned or unknown fields were projected: %+v", variant)
	}
	if variant.BotType == nil || string(*variant.BotType) != botTypeNative {
		t.Fatal("declared native config lost its discriminator")
	}
}
