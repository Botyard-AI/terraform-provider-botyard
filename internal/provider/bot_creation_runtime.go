package provider

import (
	"context"
	"fmt"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const legacyBotPlanKey = "botyard_existing_openclaw"

// Core replans replacements with null prior state but carries PlannedPrivate
// from the first plan into PriorPrivate. Preserve the legacy identity across
// that boundary so lifecycle-triggered replacements cannot look like new bots.
// Ordinary destroy plans return before this hook and remain supported.
func protectLegacyBotReplacement(ctx context.Context, state *BotResourceModel, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if state != nil {
		if resp.Private != nil {
			var marker []byte
			if state.Harness.ValueString() == harnessOpenClaw {
				marker = []byte("true")
			}
			resp.Diagnostics.Append(resp.Private.SetKey(ctx, legacyBotPlanKey, marker)...)
		}
		return
	}
	if req.Private == nil {
		return
	}
	marker, diags := req.Private.GetKey(ctx, legacyBotPlanKey)
	resp.Diagnostics.Append(diags...)
	if len(marker) != 0 {
		resp.Diagnostics.AddError("OpenClaw is existing-bots-only",
			"Terraform requested replacement of an existing OpenClaw bot. Replacement is refused before deletion, including lifecycle-triggered replacements. Existing bot updates and ordinary deletion remain supported; create a separate native bot instead.")
	}
}

// Creation defaults are deliberately not schema defaults: Optional+Computed
// runtime fields on existing/imported bots must keep their refreshed state.
func defaultBotCreationRuntime(plan *BotResourceModel, cfg BotResourceModel, state *BotResourceModel, diags *diag.Diagnostics) {
	if state != nil && state.Harness.ValueString() == harnessOpenClaw {
		// Protect the existing bot, regardless of the requested replacement's
		// runtime. Unknown immutable values can also force replacement, so
		// fail closed until they can be shown to equal the existing state.
		// Core-triggered replacement is also checked through private plan data.
		changed := !plan.Name.Equal(state.Name) ||
			!plan.HostingType.Equal(state.HostingType) ||
			!plan.Harness.Equal(state.Harness)
		if changed {
			diags.AddError("OpenClaw is existing-bots-only",
				"This change could replace an existing OpenClaw bot. Its name, hosting_type, and harness must remain unchanged and known at plan time. Existing OpenClaw bots remain supported; create a separate native bot instead of replacing this bot.")
			return
		}
	}
	if state == nil {
		if cfg.HostingType.IsNull() {
			plan.HostingType = types.StringValue("hosted")
		}
		if cfg.Harness.IsNull() {
			plan.Harness = types.StringValue(harnessNative)
		}
		validateNewBotRuntime(*plan, diags)
		return // validation above already checked the new runtime's config
	}
	if !plan.Harness.IsNull() && !plan.Harness.IsUnknown() {
		validateBotRuntimeConfig(*plan, diags)
	}
}

func validateNewBotRuntime(plan BotResourceModel, diags *diag.Diagnostics) {
	harness := plan.Harness.ValueString()
	if plan.Harness.IsNull() {
		harness = harnessNative
	}
	if !plan.Harness.IsUnknown() {
		switch client.BotHarness(harness) {
		case client.BotHarnessBotyardNative, client.BotHarnessClaudeCode:
			// Each supported creation harness has an explicit generated variant.
		case client.BotHarnessOpenclaw:
			diags.AddAttributeError(path.Root("harness"), "OpenClaw is existing-bots-only",
				"New bots use the Botyard native runtime. Existing OpenClaw bots remain supported and must not be replaced to upgrade. Remove harness = \"openclaw\" for a new bot, and use native_config instead of config.")
			return
		default:
			diags.AddAttributeError(path.Root("harness"), "Unsupported new-bot harness",
				fmt.Sprintf("Cannot create a bot with harness %q. Supported creation harnesses are botyard_native and claude_code; an unhandled harness must not inherit another runtime's config shape.", harness))
			return
		}
	}
	validateBotRuntimeConfig(plan, diags)
}

func validateBotRuntimeConfig(plan BotResourceModel, diags *diag.Diagnostics) {
	harness := plan.Harness.ValueString()
	if plan.Harness.IsUnknown() {
		return // a configured runtime expression will be known at apply
	}
	if plan.Harness.IsNull() {
		harness = harnessNative // only a new create may use this fallback
	}
	if plan.NativeConfig != nil && harness != harnessNative {
		diags.AddAttributeError(path.Root("native_config"), "Config block does not match the bot's harness",
			"native_config describes the Botyard native runtime, not this bot's harness. Existing OpenClaw bots use config; their runtime is not changed by an upgrade.")
	}
	if plan.Config != nil && harness == harnessNative {
		diags.AddAttributeError(path.Root("config"), "Native bots use native_config",
			"config is the legacy configuration shape. New hosted bots use native_config. Existing OpenClaw bots may keep config unchanged; do not replace them to upgrade.")
	}
}

// Unknown configured values are valid during planning, but must never be
// treated as omitted at the create boundary: doing so could silently select
// native and discard a practitioner's runtime or config choice.
func validateConcreteBotCreationRuntime(plan BotResourceModel, diags *diag.Diagnostics) {
	if plan.Harness.IsUnknown() {
		diags.AddAttributeError(path.Root("harness"), "Bot runtime is unknown", "The bot runtime must be known before creation; a planned unknown cannot use the native default.")
	}
	if plan.HostingType.IsUnknown() {
		diags.AddAttributeError(path.Root("hosting_type"), "Bot hosting type is unknown", "The hosting type must be known before creation; a planned unknown cannot use the hosted default.")
	}
	if !diags.HasError() {
		validateNewBotRuntime(plan, diags)
	}
}
