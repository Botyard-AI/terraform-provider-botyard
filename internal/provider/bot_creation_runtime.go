package provider

import (
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Creation defaults are deliberately not schema defaults: Optional+Computed
// runtime fields on existing/imported bots must keep their refreshed state.
func defaultBotCreationRuntime(plan *BotResourceModel, cfg BotResourceModel, state *BotResourceModel, diags *diag.Diagnostics) {
	if state != nil && plan.Harness.ValueString() == harnessOpenClaw {
		// A replacement into a retired runtime must fail at plan, not after
		// Terraform destroys the old bot. These are the schema's immutable
		// attributes; explicit CLI -replace remains an operator action.
		changed := (!plan.Name.IsUnknown() && !plan.Name.Equal(state.Name)) ||
			(!plan.HostingType.IsUnknown() && !plan.HostingType.Equal(state.HostingType)) ||
			!plan.Harness.Equal(state.Harness)
		if changed {
			validateNewBotRuntime(*plan, diags)
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
	if plan.Harness.ValueString() == harnessOpenClaw {
		diags.AddAttributeError(path.Root("harness"), "OpenClaw is existing-bots-only",
			"New bots use the Botyard native runtime. Existing OpenClaw bots remain supported and must not be replaced to upgrade. Remove harness = \"openclaw\" for a new bot, and use native_config instead of config.")
		return
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
