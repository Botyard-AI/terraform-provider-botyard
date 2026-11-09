package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestNativeDefault_ModifyPlanUsesStateOnlyForExistingBots(t *testing.T) {
	for _, tc := range []struct {
		name        string
		existing    bool
		configured  types.String
		nativeBlock bool
		legacyBlock bool
		wantHarness string
		wantError   bool
	}{
		{name: "new default", wantHarness: harnessNative},
		{name: "new native config without harness", nativeBlock: true, wantHarness: harnessNative},
		{name: "new explicit legacy rejected", configured: types.StringValue(harnessOpenClaw), wantError: true},
		{name: "new implicit legacy config rejected", legacyBlock: true, wantError: true},
		{name: "existing implicit legacy preserved", existing: true, legacyBlock: true, wantHarness: harnessOpenClaw},
		{name: "existing explicit legacy preserved", existing: true, legacyBlock: true, configured: types.StringValue(harnessOpenClaw), wantHarness: harnessOpenClaw},
		{name: "existing legacy cannot take native config", existing: true, nativeBlock: true, wantError: true},
		{name: "configured unknown not guessed", configured: types.StringUnknown()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := &BotResource{}
			schemaResp := &resource.SchemaResponse{}
			r.Schema(ctx, resource.SchemaRequest{}, schemaResp)
			cfg := botResourceModel()
			cfg.Harness = tc.configured
			if tc.nativeBlock {
				cfg.NativeConfig = &botNativeConfigModel{}
			}
			if tc.legacyBlock {
				cfg.Config = &botConfigModel{}
			}
			plan := cfg
			if tc.configured.IsNull() {
				plan.Harness = types.StringUnknown()
			}
			plan.HostingType = types.StringUnknown()
			rawState := tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil)
			if tc.existing {
				prior := cfg
				prior.Harness = types.StringValue(harnessOpenClaw)
				prior.HostingType = types.StringValue("hosted")
				// The framework's UseStateForUnknown runs before resource ModifyPlan.
				plan.Harness = prior.Harness
				plan.HostingType = prior.HostingType
			}
			if tc.existing {
				prior := plan
				raw, d := tfsdkValueFrom(t, schemaResp.Schema, prior)
				if d.HasError() {
					t.Fatal(d)
				}
				rawState = raw
			}
			rawConfig, d := tfsdkValueFrom(t, schemaResp.Schema, cfg)
			if d.HasError() {
				t.Fatal(d)
			}
			rawPlan, d := tfsdkValueFrom(t, schemaResp.Schema, plan)
			if d.HasError() {
				t.Fatal(d)
			}
			req := resource.ModifyPlanRequest{
				Config: tfsdk.Config{Raw: rawConfig, Schema: schemaResp.Schema},
				Plan:   tfsdk.Plan{Raw: rawPlan, Schema: schemaResp.Schema},
				State:  tfsdk.State{Raw: rawState, Schema: schemaResp.Schema},
			}
			resp := &resource.ModifyPlanResponse{Plan: req.Plan}
			r.ModifyPlan(ctx, req, resp)
			if resp.Diagnostics.HasError() != tc.wantError {
				t.Fatalf("diagnostics=%v wantError=%v", resp.Diagnostics, tc.wantError)
			}
			if tc.wantError {
				return
			}
			var got BotResourceModel
			if d := resp.Plan.Get(ctx, &got); d.HasError() {
				t.Fatal(d)
			}
			if got.Harness.ValueString() != tc.wantHarness {
				t.Fatalf("harness=%s want %q", got.Harness, tc.wantHarness)
			}
			if tc.configured.IsUnknown() && !got.Harness.IsUnknown() {
				t.Fatal("configured unknown harness was replaced by a guess")
			}
		})
	}
}

func TestNativeDefault_CreateBodyNeverReliesOnOldServerDefault(t *testing.T) {
	body, diags := buildBotCreateBody(botResourceModel())
	if diags.HasError() {
		t.Fatal(diags)
	}
	got := decodeObj(t, body)
	if jsonStr(t, got["harness"]) != harnessNative || jsonStr(t, got["hosting_type"]) != "hosted" {
		t.Fatalf("body=%s", body)
	}
}

func TestNativeDefault_LegacyCreateFailsBeforeAnyClientCall(t *testing.T) {
	plan := botResourceModel()
	plan.Harness = types.StringValue(harnessOpenClaw)
	schemaResp := &resource.SchemaResponse{}
	r := &BotResource{}
	ctx := context.Background()
	r.Schema(ctx, resource.SchemaRequest{}, schemaResp)
	raw, d := tfsdkValueFrom(t, schemaResp.Schema, plan)
	if d.HasError() {
		t.Fatal(d)
	}
	resp := &resource.CreateResponse{}
	// No client: attempting any network access would panic.
	r.Create(ctx, resource.CreateRequest{Plan: tfsdk.Plan{Raw: raw, Schema: schemaResp.Schema}}, resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "Existing OpenClaw bots remain supported") {
		t.Fatalf("diagnostics=%v", resp.Diagnostics)
	}
}

func TestNativeDefault_ImmutableLegacyChangeRefusedBeforeReplacement(t *testing.T) {
	prior := botResourceModel()
	prior.Harness = types.StringValue(harnessOpenClaw)
	prior.HostingType = types.StringValue("hosted")
	plan := prior
	plan.Name = types.StringValue("Renamed bot")
	var diags diag.Diagnostics
	defaultBotCreationRuntime(&plan, prior, &prior, &diags)
	if !diags.HasError() {
		t.Fatal("legacy replacement must fail at plan, before destroying the old bot")
	}
}

func TestNativeDefault_UnknownRuntimeFailsClosedAtCreate(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*BotResourceModel)
	}{
		{"harness", func(m *BotResourceModel) { m.Harness = types.StringUnknown() }},
		{"hosting_type", func(m *BotResourceModel) { m.HostingType = types.StringUnknown() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := botResourceModel()
			tc.set(&plan)
			if body, diags := buildBotCreateBody(plan); !diags.HasError() || body != nil {
				t.Fatalf("unknown %s must fail closed: body=%s diagnostics=%v", tc.name, body, diags)
			}
		})
	}
}
