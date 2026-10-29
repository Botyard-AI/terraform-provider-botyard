package provider

import (
	"encoding/json"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
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
//
// `prompt_template` and `prompt_ref` were removed in v0.7.0 (PlatDev #4376).
// The API dropped both (PlatDev #3684) and NativeConfigPatch is extra="forbid",
// so v0.5.1 and v0.6.0 already refused them at plan time and read them back as
// null. The schema version stays 0: state written by an earlier version is
// decoded with the framework's IgnoreUndefinedAttributes, which drops the
// stale keys (see TestBotState_RemovedNativePromptFieldsDroppedOnUpgrade).
type botNativeConfigModel struct {
	// Practitioner-owned.
	ToolSearch *botToolSearchModel `tfsdk:"tool_search"`
	ToolImages *botToolImagesModel `tfsdk:"tool_images"`

	// Platform-owned, read-only (see the block comment above).
	ModelProvider  types.String `tfsdk:"model_provider"`
	ModelName      types.String `tfsdk:"model_name"`
	HostPolicyJSON types.String `tfsdk:"host_policy_json"`
}

// botToolImagesModel mirrors ToolImageConfigPatch.
//
// Unlike tool_search, every target field is nullable, null MEANS "platform
// default" on the API side, and an explicit null in a patch resets the field.
// So the leaves are Optional but NOT Computed: once the block is declared it
// is authoritative for all three leaves. A leaf left out is sent as null and
// reads back as null, so a re-plan is a clean no-op, and removing a leaf
// really returns it to the default instead of silently keeping the last
// applied value the way an Optional+Computed leaf would.
type botToolImagesModel struct {
	Enabled        types.Bool   `tfsdk:"enabled"`
	MaxDimensionPx types.Int64  `tfsdk:"max_dimension_px"`
	DefaultDetail  types.String `tfsdk:"default_detail"`
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
			"tool_search": toolSearchSchemaAttribute(),
			"tool_images": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "How images returned by the bot's tools are shown to the model. Once this " +
					"block is declared it owns all three fields: a field you leave out is sent as null, which " +
					"resets it to the platform default. Removing the whole block leaves the last applied values " +
					"in place, because the API treats an omitted `tool_images` as \"no change\". To return to " +
					"the defaults, apply an empty block, `tool_images = {}`, first.",
				Attributes: map[string]schema.Attribute{
					"enabled": schema.BoolAttribute{
						Optional: true,
						MarkdownDescription: "Show images returned by tools to the model. `false` replaces each " +
							"image with a short text notice. Unset means enabled.",
					},
					"max_dimension_px": schema.Int64Attribute{
						Optional: true,
						MarkdownDescription: "Longest edge, in pixels, a tool image is downscaled to before the " +
							"model sees it, from 1 to 8000. Unset means the platform default (2000). Values above " +
							"2000 only take effect while a request holds 20 images or fewer.",
						Validators: []validator.Int64{int64validator.Between(1, 8000)},
					},
					"default_detail": schema.StringAttribute{
						Optional: true,
						MarkdownDescription: "Detail level for a tool image whose MCP server does not request one: " +
							"`auto`, `low`, `high` or `original`. Unset leaves it to the model provider's default.",
						Validators: []validator.String{stringvalidator.OneOf("auto", "low", "high", "original")},
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
// That is true of Botyard APIs before PlatDev #3897. From #3897 the bot's
// runtime profile picks the shape and the key is ignored, and from #3898 it is
// removed from the contract but still accepted and dropped on input. It stays
// here so this provider works against every one of those API versions.
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
	if v, ok := buildToolSearchPatch(cfg.ToolSearch); ok {
		m["tool_search"] = v
	}
	if v, ok := buildToolImagesPatch(cfg.ToolImages); ok {
		m["tool_images"] = v
	}
	// `model` and `host_policy` are never emitted — they are platform-owned.
	return marshalObj(m)
}

// buildToolImagesPatch renders a declared tool_images block with all three
// keys present. An unset leaf is sent as an explicit null, which the API reads
// as "reset to the platform default" (see botToolImagesModel). An unknown leaf
// is omitted, leaving that field unchanged, and is never nulled. These leaves
// are Optional-only, so by apply time a configured value is known; the branch
// only guards a defensive caller. (Apply can still carry unknown COMPUTED
// values elsewhere; this reasoning is specific to configured leaves.)
func buildToolImagesPatch(ti *botToolImagesModel) (json.RawMessage, bool) {
	if ti == nil {
		return nil, false
	}
	inner := map[string]json.RawMessage{}
	putBoolOrNull(inner, "enabled", ti.Enabled)
	putInt64OrNull(inner, "max_dimension_px", ti.MaxDimensionPx)
	putStrOrNull(inner, "default_detail", ti.DefaultDetail)
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
	mapToolSearch(nc.ToolSearch, cfg.ToolSearch)

	// tool_images leaves are not Computed, so a declared block takes the
	// server's values as they are, nulls included. A response without
	// tool_images means every field is at its default.
	if cfg.ToolImages != nil {
		var ti client.ToolImageConfig
		if nc.ToolImages != nil {
			ti = *nc.ToolImages
		}
		cfg.ToolImages.Enabled = boolPtrToBool(ti.Enabled)
		cfg.ToolImages.MaxDimensionPx = intPtrToInt64(ti.MaxDimensionPx)
		cfg.ToolImages.DefaultDetail = enumPtrToStr(ti.DefaultDetail)
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
