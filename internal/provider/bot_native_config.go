package provider

import (
	"encoding/json"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// The `native_config` block models NativeBotConfig — the second member of the
// API's BotConfig union, carried by bots whose harness is `botyard_native`.
//
// It is a SEPARATE top-level block rather than a polymorphic `config`, because
// Terraform models discriminated unions badly: one block whose valid attributes
// depend on a sibling's value gives confusing plan-time errors and no useful
// schema documentation. Two blocks that conflict with each other say exactly
// what is wrong, at plan time, in the practitioner's own terms.
//
// Two of NativeBotConfig's five fields are deliberately NOT practitioner-owned,
// and both are surfaced read-only instead. This is not an oversight — writing
// either one is a documented way to break a bot:
//
//   - `model` is DERIVED, not stored-as-declared. The API rebuilds it from the
//     bot's LLM credential links (chain position 0) and overwrites whatever a
//     patch carried, so a writable attribute here would silently do nothing and
//     then re-diff forever. This is the exact trap that killed OpenClaw's
//     `model.primary` — see the note on the deleted ModelConfigPatch in the
//     API's bot_config_patch.py: "a self-service surface that appeared to work
//     and didn't." `botyard_bot_credential_assignment` is how you set it.
//
//   - `host_policy` SELF-HEALS. The API rewrites an inert policy into the full
//     default set, so a declared value reads back as something else — a
//     permanent diff. Worse, it is a free-form dict whose keys are Rust serde
//     variant names (`exec`, not `host_exec`) parsed with `deny_unknown_fields`,
//     where one typo is a fatal daemon startup that crash-loops the pod. That
//     incident has already happened once (it took the `jack` bot down). Letting
//     a plan write this blind is not acceptable, so v1 exposes it as read-only
//     JSON and leaves ownership with the platform.
//
// Surfacing both read-only is the useful middle: you can see what the platform
// decided (and reference it in outputs) without being able to break it.

// botNativeConfigModel maps the `native_config` block.
type botNativeConfigModel struct {
	// Practitioner-owned.
	PromptTemplate types.String        `tfsdk:"prompt_template"`
	PromptRef      types.String        `tfsdk:"prompt_ref"`
	ToolSearch     *botToolSearchModel `tfsdk:"tool_search"`

	// Platform-owned, read-only (see the block comment above).
	ModelProvider  types.String `tfsdk:"model_provider"`
	ModelName      types.String `tfsdk:"model_name"`
	HostPolicyJSON types.String `tfsdk:"host_policy_json"`
}

// botToolSearchModel mirrors ToolSearchConfigPatch.
type botToolSearchModel struct {
	Mode                 types.String `tfsdk:"mode"`
	AutoThresholdPercent types.Int64  `tfsdk:"auto_threshold_percent"`
}

// botNativeConfigSchemaAttribute builds the `native_config` nested attribute.
//
// Leaf semantics follow the `config` block: Optional+Computed with
// UseStateForUnknown, so server-merged defaults land in state and an immediate
// re-plan is a clean no-op. The read-only leaves are Computed-only.
func botNativeConfigSchemaAttribute() schema.SingleNestedAttribute {
	return schema.SingleNestedAttribute{
		Optional: true,
		MarkdownDescription: "Configuration for a native (`botyard_native`) bot, applied via the config endpoint " +
			"(embedded in the create request and sent to `PATCH /config` on update). Requires " +
			"`harness = \"botyard_native\"`, and conflicts with `config`, which models the OpenClaw shape. " +
			"Only the fields you set are applied over the platform's defaults.",
		Attributes: map[string]schema.Attribute{
			"prompt_template": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Bot instructions template, rendered into the stable prefix of the system prompt. " +
					"The platform supplies a default when omitted.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"prompt_ref": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Optional managed-prompt ref naming the template source. Pins the template to a " +
					"directory-versioned prompt instead of the inline `prompt_template`.",
			},
			"tool_search": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "Defer tool schemas out of the prompt behind a search surface — the same " +
					"owner-facing setting OpenClaw bots carry.",
				Attributes: map[string]schema.Attribute{
					"mode": schema.StringAttribute{
						Optional: true,
						Computed: true,
						MarkdownDescription: "`always` (defer every turn), `auto` (defer above the threshold), or " +
							"`off` (never defer).",
						PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
					},
					"auto_threshold_percent": schema.Int64Attribute{
						Optional: true,
						Computed: true,
						MarkdownDescription: "Percentage of the context window the tool schemas must exceed before " +
							"`auto` defers them. Ignored unless `mode` is `auto`.",
						PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
					},
				},
			},

			"model_provider": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Provider of the model the broker routes this bot to. **Read-only** — the " +
					"platform derives it from the bot's LLM credential links; manage it with " +
					"`botyard_bot_credential_assignment`.",
			},
			"model_name": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Model the broker routes this bot to. **Read-only** — derived from the bot's " +
					"LLM credential links (chain position 0), not from this resource.",
			},
			"host_policy_json": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The bot's operator host policy, as JSON. **Read-only** — the platform owns " +
					"this value and normalizes it, and an invalid policy is a fatal runtime startup failure, " +
					"so it is not writable from Terraform.",
			},
		},
	}
}

// buildNativeConfigPatch renders the sparse NativeConfigPatch object (the value
// of the `config` key in both the create POST and the `PATCH /config` body).
//
// *** IT ALWAYS EMITS `bot_type`, AND THAT IS LOAD-BEARING. ***
//
// The API's config field is an `anyOf` of OpenClawConfigPatch | NativeConfigPatch
// with NO discriminator, and each member carries its own `const` bot_type. Omit
// the discriminator and every field here is an unknown key to OpenClawConfigPatch
// — which ignores extras — so the payload validates as an EMPTY OpenClaw patch:
// HTTP 201, config silently dropped, no error anywhere. Sending `bot_type` is
// what makes the OpenClaw branch fail to validate and the native branch win.
//
// A nil block yields `{}`, matching the OpenClaw path: merge nothing over the
// platform's defaults. Note `{}` alone selects OpenClaw, which is correct for a
// bot that declared no native config.
func buildNativeConfigPatch(cfg *botNativeConfigModel) json.RawMessage {
	if cfg == nil {
		return json.RawMessage("{}")
	}
	m := map[string]json.RawMessage{
		"bot_type": json.RawMessage(`"` + botTypeNative + `"`),
	}
	putStr(m, "prompt_template", cfg.PromptTemplate)
	putStr(m, "prompt_ref", cfg.PromptRef)
	if v, ok := buildToolSearchPatch(cfg.ToolSearch); ok {
		m["tool_search"] = v
	}
	// `model` and `host_policy` are never emitted — they are platform-owned.
	return marshalObj(m)
}

func buildToolSearchPatch(ts *botToolSearchModel) (json.RawMessage, bool) {
	if ts == nil {
		return nil, false
	}
	inner := map[string]json.RawMessage{}
	putStr(inner, "mode", ts.Mode)
	putInt64(inner, "auto_threshold_percent", ts.AutoThresholdPercent)
	if len(inner) == 0 {
		return nil, false
	}
	return marshalObj(inner), true
}

// mapNativeBotConfig refreshes the declared `native_config` block from the API's
// NativeBotConfig. A nil cfg is a no-op (the block was not declared), mirroring
// mapBotConfig: state is never populated for a block the practitioner did not
// write, so undeclared config never becomes a phantom diff.
func mapNativeBotConfig(nc *client.NativeBotConfig, cfg *botNativeConfigModel) {
	if cfg == nil {
		return
	}
	cfg.PromptTemplate = types.StringValue(nc.PromptTemplate)
	cfg.PromptRef = strPtrToStr(nc.PromptRef)

	// Read-only projections. Always set, so they are never unknown in state.
	// `provider` is optional on the wire (it defaults to "botyard" server-side),
	// so it arrives as a pointer.
	cfg.ModelProvider = strPtrToStr(nc.Model.Provider)
	cfg.ModelName = types.StringValue(nc.Model.Model)
	cfg.HostPolicyJSON = hostPolicyToJSON(nc.HostPolicy)

	// Only refresh tool_search when the block is declared — an undeclared
	// nested block stays nil rather than materializing server defaults. The
	// server side is a pointer too (the field is optional in the response), so
	// both halves are guarded.
	if cfg.ToolSearch != nil && nc.ToolSearch != nil {
		cfg.ToolSearch.Mode = enumPtrToStr(nc.ToolSearch.Mode)
		cfg.ToolSearch.AutoThresholdPercent = intPtrToInt64(nc.ToolSearch.AutoThresholdPercent)
	}
}

// hostPolicyToJSON renders the free-form host policy as compact JSON for the
// read-only attribute. An absent policy is null rather than "null" or "{}", so
// "the platform set no policy" is distinguishable from "the policy is empty".
func hostPolicyToJSON(hp *map[string]interface{}) types.String {
	if hp == nil {
		return types.StringNull()
	}
	b, err := json.Marshal(*hp)
	if err != nil {
		// Unreachable for a decoded JSON object, but never panic on a value the
		// server chose: a null here is strictly better than crashing a plan.
		return types.StringNull()
	}
	return types.StringValue(string(b))
}
