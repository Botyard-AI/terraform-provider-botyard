package provider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestNativeDefault_LegacyDeleteFailsBeforeClientCall(t *testing.T) {
	for _, tc := range []struct {
		name       string
		harness    types.String
		optIn      types.Bool
		diagnostic string
	}{
		{"legacy", types.StringValue(harnessOpenClaw), types.BoolNull(), "allow_legacy_deletion"},
		{"null", types.StringNull(), types.BoolNull(), "Refresh"},
		{"unknown", types.StringUnknown(), types.BoolNull(), "Refresh"},
		{"unsupported", types.StringValue("future_harness"), types.BoolValue(true), "Refresh"},
		{"null_opt_in", types.StringNull(), types.BoolValue(true), "Refresh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := &BotResource{} // No API client: a DELETE attempt would panic.
			schemaResp := &resource.SchemaResponse{}
			r.Schema(ctx, resource.SchemaRequest{}, schemaResp)
			state := botResourceModel()
			state.Harness = tc.harness
			state.AllowLegacyDeletion = tc.optIn
			raw, d := tfsdkValueFrom(t, schemaResp.Schema, state)
			if d.HasError() {
				t.Fatal(d)
			}
			resp := &resource.DeleteResponse{}
			r.Delete(ctx, resource.DeleteRequest{State: tfsdk.State{Raw: raw, Schema: schemaResp.Schema}}, resp)
			if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), tc.diagnostic) {
				t.Fatalf("diagnostics=%v", resp.Diagnostics)
			}
		})
	}
}

func TestNativeDefault_LegacyDeletionOptInMatrix(t *testing.T) {
	for _, harness := range []struct {
		name       string
		value      types.String
		classified bool
		legacy     bool
	}{
		{"openclaw", types.StringValue(harnessOpenClaw), true, true},
		{"native", types.StringValue(harnessNative), true, false},
		{"external", types.StringValue("claude_code"), true, false},
		{"null", types.StringNull(), false, false},
		{"unknown", types.StringUnknown(), false, false},
		{"empty", types.StringValue(""), false, false},
		{"unsupported", types.StringValue("future_harness"), false, false},
	} {
		for _, option := range []struct {
			name    string
			value   types.Bool
			optedIn bool
		}{
			{"omitted", types.BoolNull(), false},
			{"false", types.BoolValue(false), false},
			{"unknown", types.BoolUnknown(), false},
			{"true", types.BoolValue(true), true},
		} {
			t.Run(harness.name+"/"+option.name, func(t *testing.T) {
				state := botResourceModel()
				state.Harness = harness.value
				state.AllowLegacyDeletion = option.value
				var diagnostics diag.Diagnostics
				allowed := allowLegacyBotDeletion(state, &diagnostics)
				want := harness.classified && (!harness.legacy || option.optedIn)
				if allowed != want || diagnostics.HasError() == want {
					t.Fatalf("allowed=%v want=%v diagnostics=%v", allowed, want, diagnostics)
				}
			})
		}
	}
}

// Untaint only the single state inside this test's private TF_ACC_TEMP_DIR.
// This is a real Terraform state operation, not a provider/test bypass.
func untaintLegacyTestBot(t *testing.T, root string) {
	t.Helper()
	var states []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == "terraform.tfstate" {
			states = append(states, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("expected one isolated test state, got %v", states)
	}
	// Use the Terraform executable selected by the test process's PATH, as
	// the local lifecycle preflight and CI do; no executable comes from state.
	cmd := exec.Command("terraform", "untaint", "-no-color", "botyard_bot.native")
	cmd.Dir = filepath.Dir(states[0])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("untaint isolated fake bot: %v: %s", err, out)
	}
}
