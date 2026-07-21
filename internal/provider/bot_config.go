package provider

import (
	"encoding/json"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// Phase B models the bot's OpenClaw config (`PATCH /config`, and embedded in the
// create POST) as an optional nested `config` block on botyard_bot.
//
// Scope: the patchable OpenClawConfigPatch surface, modeled incrementally — the
// high-value scalars plus the model/identity/heartbeat/compaction/session
// objects. Deliberately DEFERRED (documented so the omission is intentional, not
// an oversight):
//   - `addons`: a list of typed addon objects carrying a free-form config map;
//     needs careful list/dynamic modeling and its own round-trip validation.
//   - `bot_type`: a discriminator whose only value is "openclaw"; exposing it
//     invites misconfiguration for no benefit.
//
// Attribute semantics: the `config` container is Optional (NOT Computed) so
// Terraform only manages config when the block is declared — a bot with
// dashboard-set config the practitioner never declared is left untouched. The
// nested objects are Optional (not Computed) too: their PRESENCE is driven by
// config, and Read only refreshes an object's leaves when the block is declared.
// Individual leaves are Optional+Computed with UseStateForUnknown so the
// server-merged OpenClaw defaults land in state and a re-apply is a clean no-op
// (mirroring Phase A's stableComputedString and the mcp_server pattern). This
// keeps nested objects free of framework "unknown value" hazards (a *struct
// cannot hold unknown) while still absorbing server defaults per-leaf.

// botConfigModel maps the `config` block. Nested objects are pointers so an
// undeclared block is a nil pointer (never an unknown object value).
type botConfigModel struct {
	SystemPromptMode types.String        `tfsdk:"system_prompt_mode"`
	ThinkingDefault  types.String        `tfsdk:"thinking_default"`
	ReasoningDefault types.String        `tfsdk:"reasoning_default"`
	Model            *botModelModel      `tfsdk:"model"`
	Identity         *botIdentityModel   `tfsdk:"identity"`
	Heartbeat        *botHeartbeatModel  `tfsdk:"heartbeat"`
	Compaction       *botCompactionModel `tfsdk:"compaction"`
	Session          *botSessionModel    `tfsdk:"session"`
}

// botModelModel mirrors ModelConfigPatch (currently just `primary`).
type botModelModel struct {
	Primary *botModelRefModel `tfsdk:"primary"`
}

// botModelRefModel mirrors ModelRef ({ provider, model }).
type botModelRefModel struct {
	Provider types.String `tfsdk:"provider"`
	Model    types.String `tfsdk:"model"`
}

// botIdentityModel mirrors the patchable IdentityConfig fields.
type botIdentityModel struct {
	Emoji types.String `tfsdk:"emoji"`
	Theme types.String `tfsdk:"theme"`
}

// botHeartbeatModel mirrors HeartbeatConfigPatch.
type botHeartbeatModel struct {
	Every            types.String         `tfsdk:"every"`
	AckMaxChars      types.Int64          `tfsdk:"ack_max_chars"`
	IncludeReasoning types.Bool           `tfsdk:"include_reasoning"`
	IsolatedSession  types.Bool           `tfsdk:"isolated_session"`
	LightContext     types.Bool           `tfsdk:"light_context"`
	Model            types.String         `tfsdk:"model"`
	Prompt           types.String         `tfsdk:"prompt"`
	ActiveHours      *botActiveHoursModel `tfsdk:"active_hours"`
}

// botActiveHoursModel mirrors ActiveHoursConfigPatch. All three fields are
// Required when the block is declared (the response models them as required).
type botActiveHoursModel struct {
	FromTime types.String `tfsdk:"from_time"`
	ToTime   types.String `tfsdk:"to_time"`
	Timezone types.String `tfsdk:"timezone"`
}

// botCompactionModel mirrors CompactionConfigPatch.
type botCompactionModel struct {
	MaxActiveTranscriptBytes types.Int64 `tfsdk:"max_active_transcript_bytes"`
	MidTurnPrecheck          types.Bool  `tfsdk:"mid_turn_precheck"`
	ReserveTokens            types.Int64 `tfsdk:"reserve_tokens"`
	ReserveTokensFloor       types.Int64 `tfsdk:"reserve_tokens_floor"`
	TimeoutSeconds           types.Int64 `tfsdk:"timeout_seconds"`
	TruncateAfterCompaction  types.Bool  `tfsdk:"truncate_after_compaction"`
}

// botSessionModel mirrors SessionConfigPatch.
type botSessionModel struct {
	WriteLockMaxHoldMs types.Int64 `tfsdk:"write_lock_max_hold_ms"`
}

// botConfigSchemaAttribute builds the `config` nested attribute. Leaves are
// Optional+Computed with UseStateForUnknown; nested objects are Optional-only.
func botConfigSchemaAttribute() schema.SingleNestedAttribute {
	optCompStr := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{
			Optional:            true,
			Computed:            true,
			MarkdownDescription: desc,
			PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		}
	}
	optCompInt := func(desc string) schema.Int64Attribute {
		return schema.Int64Attribute{
			Optional:            true,
			Computed:            true,
			MarkdownDescription: desc,
			PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
		}
	}
	optCompBool := func(desc string) schema.BoolAttribute {
		return schema.BoolAttribute{
			Optional:            true,
			Computed:            true,
			MarkdownDescription: desc,
			PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
		}
	}
	reqStr := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Required: true, MarkdownDescription: desc}
	}

	return schema.SingleNestedAttribute{
		Optional: true,
		MarkdownDescription: "OpenClaw configuration overrides for the bot, applied via the config endpoint " +
			"(embedded in the create request and sent to `PATCH /config` on update). Only the fields you " +
			"set are applied over OpenClaw's defaults; omitted fields keep their server default. This block " +
			"models the patchable config surface incrementally — `addons` and `bot_type` are not yet modeled.",
		Attributes: map[string]schema.Attribute{
			"system_prompt_mode": optCompStr("System prompt source: `botyard` (lean, default) or `openclaw`."),
			"thinking_default":   optCompStr("Default thinking budget: one of `off`, `minimal`, `low`, `medium`, `high`, `xhigh`, `adaptive`."),
			"reasoning_default":  optCompStr("Default reasoning mode: `off`, `on`, or `stream`."),
			"model": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "LLM model configuration.",
				Attributes: map[string]schema.Attribute{
					"primary": schema.SingleNestedAttribute{
						Optional:            true,
						MarkdownDescription: "Primary model reference.",
						Attributes: map[string]schema.Attribute{
							"provider": optCompStr("Provider key within `models.providers` (e.g. `botyard`). Defaults to `botyard` when omitted."),
							"model":    reqStr("Model ID within the provider (e.g. `gpt-5.4`)."),
						},
					},
				},
			},
			"identity": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Bot identity overrides.",
				Attributes: map[string]schema.Attribute{
					"emoji": optCompStr("Bot identity emoji."),
					"theme": optCompStr("Bot identity theme."),
				},
			},
			"heartbeat": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Heartbeat scheduling and behavior.",
				Attributes: map[string]schema.Attribute{
					"every":             optCompStr("Heartbeat interval: one of `0m` (disabled), `5m`, `15m`, `30m`, `1h`, `2h`, `6h`, `12h`, `24h`, `168h`."),
					"ack_max_chars":     optCompInt("Max characters after `HEARTBEAT_OK` before delivery."),
					"include_reasoning": optCompBool("Include reasoning traces in heartbeat responses."),
					"isolated_session":  optCompBool("Run each heartbeat in a fresh session with no history."),
					"light_context":     optCompBool("Use minimal context (only HEARTBEAT.md) to reduce token cost."),
					"model":             optCompStr("LLM model override for heartbeat runs."),
					"prompt":            optCompStr("Custom prompt replacing the default HEARTBEAT.md reader."),
					"active_hours": schema.SingleNestedAttribute{
						Optional:            true,
						MarkdownDescription: "Time window when heartbeats are active.",
						Attributes: map[string]schema.Attribute{
							"from_time": reqStr("Start time in `HH:MM` format (e.g. `09:00`)."),
							"to_time":   reqStr("End time in `HH:MM` format (e.g. `17:00`)."),
							"timezone":  reqStr("IANA timezone (e.g. `America/New_York`)."),
						},
					},
				},
			},
			"compaction": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Context-compaction safeguards.",
				Attributes: map[string]schema.Attribute{
					"max_active_transcript_bytes": optCompInt("Byte size that triggers proactive local compaction."),
					"mid_turn_precheck":           optCompBool("Run a tool-loop context precheck before the next model call."),
					"reserve_tokens":              optCompInt("Reply/tool headroom tokens reserved after compaction."),
					"reserve_tokens_floor":        optCompInt("Floor enforced for `reserve_tokens` (0 disables)."),
					"timeout_seconds":             optCompInt("Maximum seconds allowed for one compaction operation."),
					"truncate_after_compaction":   optCompBool("Rotate the active transcript to a compacted successor."),
				},
			},
			"session": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "Session-reliability settings.",
				Attributes: map[string]schema.Attribute{
					"write_lock_max_hold_ms": optCompInt("Watchdog force-release threshold for the session write-lock (ms)."),
				},
			},
		},
	}
}

// buildBotConfigPatch renders the sparse OpenClawConfigPatch object (the value of
// the `config` key in both the create POST and the `PATCH /config` body). Only
// leaves that are known and non-null are emitted, so omitted fields are left
// untouched by the server's `patch_model` (unset != explicit null). A nil block
// yields an empty object `{}` — the Phase A behavior that merges nothing over
// OpenClaw defaults. Unmodeled keys (addons, bot_type) are never emitted.
func buildBotConfigPatch(cfg *botConfigModel) json.RawMessage {
	if cfg == nil {
		return json.RawMessage("{}")
	}
	m := map[string]json.RawMessage{}
	putStr(m, "system_prompt_mode", cfg.SystemPromptMode)
	putStr(m, "thinking_default", cfg.ThinkingDefault)
	putStr(m, "reasoning_default", cfg.ReasoningDefault)
	if v, ok := buildModelPatch(cfg.Model); ok {
		m["model"] = v
	}
	if v, ok := buildIdentityPatch(cfg.Identity); ok {
		m["identity"] = v
	}
	if v, ok := buildHeartbeatPatch(cfg.Heartbeat); ok {
		m["heartbeat"] = v
	}
	if v, ok := buildCompactionPatch(cfg.Compaction); ok {
		m["compaction"] = v
	}
	if v, ok := buildSessionPatch(cfg.Session); ok {
		m["session"] = v
	}
	return marshalObj(m)
}

func buildModelPatch(mm *botModelModel) (json.RawMessage, bool) {
	if mm == nil || mm.Primary == nil {
		return nil, false
	}
	prim := map[string]json.RawMessage{}
	putStr(prim, "model", mm.Primary.Model)
	putStr(prim, "provider", mm.Primary.Provider)
	if len(prim) == 0 {
		return nil, false
	}
	return marshalObj(map[string]json.RawMessage{"primary": marshalObj(prim)}), true
}

func buildIdentityPatch(id *botIdentityModel) (json.RawMessage, bool) {
	if id == nil {
		return nil, false
	}
	inner := map[string]json.RawMessage{}
	putStr(inner, "emoji", id.Emoji)
	putStr(inner, "theme", id.Theme)
	if len(inner) == 0 {
		return nil, false
	}
	return marshalObj(inner), true
}

func buildHeartbeatPatch(h *botHeartbeatModel) (json.RawMessage, bool) {
	if h == nil {
		return nil, false
	}
	inner := map[string]json.RawMessage{}
	putStr(inner, "every", h.Every)
	putInt64(inner, "ack_max_chars", h.AckMaxChars)
	putBool(inner, "include_reasoning", h.IncludeReasoning)
	putBool(inner, "isolated_session", h.IsolatedSession)
	putBool(inner, "light_context", h.LightContext)
	putStr(inner, "model", h.Model)
	putStr(inner, "prompt", h.Prompt)
	if v, ok := buildActiveHoursPatch(h.ActiveHours); ok {
		inner["active_hours"] = v
	}
	if len(inner) == 0 {
		return nil, false
	}
	return marshalObj(inner), true
}

func buildActiveHoursPatch(a *botActiveHoursModel) (json.RawMessage, bool) {
	if a == nil {
		return nil, false
	}
	inner := map[string]json.RawMessage{}
	putStr(inner, "from_time", a.FromTime)
	putStr(inner, "to_time", a.ToTime)
	putStr(inner, "timezone", a.Timezone)
	if len(inner) == 0 {
		return nil, false
	}
	return marshalObj(inner), true
}

func buildCompactionPatch(c *botCompactionModel) (json.RawMessage, bool) {
	if c == nil {
		return nil, false
	}
	inner := map[string]json.RawMessage{}
	putInt64(inner, "max_active_transcript_bytes", c.MaxActiveTranscriptBytes)
	putBool(inner, "mid_turn_precheck", c.MidTurnPrecheck)
	putInt64(inner, "reserve_tokens", c.ReserveTokens)
	putInt64(inner, "reserve_tokens_floor", c.ReserveTokensFloor)
	putInt64(inner, "timeout_seconds", c.TimeoutSeconds)
	putBool(inner, "truncate_after_compaction", c.TruncateAfterCompaction)
	if len(inner) == 0 {
		return nil, false
	}
	return marshalObj(inner), true
}

func buildSessionPatch(s *botSessionModel) (json.RawMessage, bool) {
	if s == nil {
		return nil, false
	}
	inner := map[string]json.RawMessage{}
	putInt64(inner, "write_lock_max_hold_ms", s.WriteLockMaxHoldMs)
	if len(inner) == 0 {
		return nil, false
	}
	return marshalObj(inner), true
}

// mapBotConfig refreshes the modeled config leaves from the server's merged
// desired_config. The top-level scalars are Optional+Computed, so they are always
// refreshed. Nested objects are refreshed only when the practitioner declared the
// block (the pointer is non-nil), so an undeclared block is never populated from
// server defaults (which would manifest as a phantom diff). A nil cfg (config not
// managed) is a no-op.
func mapBotConfig(dc *client.OpenClawBotConfig, cfg *botConfigModel) {
	if cfg == nil {
		return
	}
	cfg.SystemPromptMode = enumPtrToStr(dc.SystemPromptMode)
	cfg.ThinkingDefault = enumPtrToStr(dc.ThinkingDefault)
	cfg.ReasoningDefault = enumPtrToStr(dc.ReasoningDefault)

	if cfg.Model != nil && cfg.Model.Primary != nil && dc.Model != nil && dc.Model.Primary != nil {
		cfg.Model.Primary.Model = types.StringValue(dc.Model.Primary.Model)
		cfg.Model.Primary.Provider = strPtrToStr(dc.Model.Primary.Provider)
	}
	if cfg.Identity != nil {
		cfg.Identity.Emoji = strPtrToStr(dc.Identity.Emoji)
		cfg.Identity.Theme = strPtrToStr(dc.Identity.Theme)
	}
	if cfg.Heartbeat != nil && dc.Heartbeat != nil {
		h, s := cfg.Heartbeat, dc.Heartbeat
		h.Every = enumPtrToStr(s.Every)
		h.AckMaxChars = intPtrToInt64(s.AckMaxChars)
		h.IncludeReasoning = boolPtrToBool(s.IncludeReasoning)
		h.IsolatedSession = boolPtrToBool(s.IsolatedSession)
		h.LightContext = boolPtrToBool(s.LightContext)
		h.Model = strPtrToStr(s.Model)
		h.Prompt = strPtrToStr(s.Prompt)
		if h.ActiveHours != nil && s.ActiveHours != nil {
			h.ActiveHours.FromTime = types.StringValue(s.ActiveHours.FromTime)
			h.ActiveHours.ToTime = types.StringValue(s.ActiveHours.ToTime)
			h.ActiveHours.Timezone = types.StringValue(s.ActiveHours.Timezone)
		}
	}
	if cfg.Compaction != nil && dc.Compaction != nil {
		c, s := cfg.Compaction, dc.Compaction
		c.MaxActiveTranscriptBytes = intPtrToInt64(s.MaxActiveTranscriptBytes)
		c.MidTurnPrecheck = boolPtrToBool(s.MidTurnPrecheck)
		c.ReserveTokens = intPtrToInt64(s.ReserveTokens)
		c.ReserveTokensFloor = intPtrToInt64(s.ReserveTokensFloor)
		c.TimeoutSeconds = intPtrToInt64(s.TimeoutSeconds)
		c.TruncateAfterCompaction = boolPtrToBool(s.TruncateAfterCompaction)
	}
	if cfg.Session != nil && dc.Session != nil {
		cfg.Session.WriteLockMaxHoldMs = intPtrToInt64(dc.Session.WriteLockMaxHoldMs)
	}
}

// --- small JSON + conversion helpers (config-specific) ---

// putStr / putInt64 / putBool add a key to a sparse patch map only when the
// attribute is known and non-null, so unset leaves are omitted (not sent as
// null) and left untouched by the server merge.
func putStr(m map[string]json.RawMessage, key string, v types.String) {
	if v.IsNull() || v.IsUnknown() {
		return
	}
	if b, err := json.Marshal(v.ValueString()); err == nil {
		m[key] = b
	}
}

func putInt64(m map[string]json.RawMessage, key string, v types.Int64) {
	if v.IsNull() || v.IsUnknown() {
		return
	}
	if b, err := json.Marshal(v.ValueInt64()); err == nil {
		m[key] = b
	}
}

func putBool(m map[string]json.RawMessage, key string, v types.Bool) {
	if v.IsNull() || v.IsUnknown() {
		return
	}
	if b, err := json.Marshal(v.ValueBool()); err == nil {
		m[key] = b
	}
}

// marshalObj marshals a sparse map of pre-validated RawMessages; the inputs are
// always valid JSON, so an error is not expected — fall back to `{}` defensively.
func marshalObj(m map[string]json.RawMessage) json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// enumPtrToStr converts a generated string-enum pointer to a Terraform string
// (null when the pointer is nil).
func enumPtrToStr[T ~string](p *T) types.String {
	if p == nil {
		return types.StringNull()
	}
	return types.StringValue(string(*p))
}

// boolPtrToBool converts an optional bool to a Terraform bool (null when nil).
func boolPtrToBool(p *bool) types.Bool {
	if p == nil {
		return types.BoolNull()
	}
	return types.BoolValue(*p)
}
