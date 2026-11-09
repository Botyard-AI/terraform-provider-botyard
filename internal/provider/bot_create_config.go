package provider

import (
	"errors"
	"fmt"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/oapi-codegen/nullable"
)

// Creation is projected directly into generated variants. The handwritten
// sparse JSON builders remain PATCH-only and are never a creation contract.
func buildTypedCreateConfig(plan BotResourceModel, harness client.BotHarness) (client.BotCreate_Config, error) {
	var union client.BotCreate_Config
	var err error
	switch harness {
	case client.BotHarnessBotyardNative:
		err = union.FromNativeConfigPatch(createNativeConfig(plan.NativeConfig))
	case client.BotHarnessClaudeCode:
		err = union.FromOpenClawConfigPatch(createExternalConfig(plan.Config))
	case client.BotHarnessOpenclaw:
		err = errors.New("OpenClaw is existing-bots-only")
	default:
		err = fmt.Errorf("unsupported new-bot harness %q", harness)
	}
	return union, err
}

func createNativeConfig(cfg *botNativeConfigModel) client.NativeConfigPatch {
	var patch client.NativeConfigPatch
	if cfg == nil {
		return patch
	}
	botType := client.NativeConfigPatchBotType(botTypeNative)
	patch.BotType = &botType
	patch.ToolSearch = createToolSearchConfig(cfg.ToolSearch)
	if images := cfg.ToolImages; images != nil {
		value := client.ToolImageConfigPatch{
			Enabled:        createConfigBool(images.Enabled),
			MaxDimensionPx: createConfigInt(images.MaxDimensionPx),
			DefaultDetail:  createConfigString[client.SparseToolImageConfigPatchDefaultDetail](images.DefaultDetail),
		}
		// Image leaves are optional-only: null resets, unknown stays omitted.
		if images.Enabled.IsNull() {
			value.Enabled.SetNull()
		}
		if images.MaxDimensionPx.IsNull() {
			value.MaxDimensionPx.SetNull()
		}
		if images.DefaultDetail.IsNull() {
			value.DefaultDetail.SetNull()
		}
		patch.ToolImages.Set(value)
	}
	// Computed model/host_policy are deliberately not projected.
	return patch
}

func createExternalConfig(cfg *botConfigModel) client.OpenClawConfigPatch {
	var patch client.OpenClawConfigPatch
	if cfg == nil {
		return patch
	}
	patch.SystemPromptMode = createConfigString[client.SparseOpenClawConfigPatchSystemPromptMode](cfg.SystemPromptMode)
	patch.ThinkingDefault = createConfigString[client.SparseOpenClawConfigPatchThinkingDefault](cfg.ThinkingDefault)
	patch.ReasoningDefault = createConfigString[client.SparseOpenClawConfigPatchReasoningDefault](cfg.ReasoningDefault)
	patch.ToolSearch = createToolSearchConfig(cfg.ToolSearch)
	if identity := cfg.Identity; identity != nil {
		value := client.IdentityConfigPatch{
			Emoji: createConfigString[string](identity.Emoji),
			Theme: createConfigString[string](identity.Theme),
		}
		if value.Emoji.IsSpecified() || value.Theme.IsSpecified() {
			patch.Identity.Set(value)
		}
	}
	if heartbeat := cfg.Heartbeat; heartbeat != nil {
		value := client.HeartbeatConfigPatch{
			Every:            createConfigString[client.SparseHeartbeatConfigPatchEvery](heartbeat.Every),
			AckMaxChars:      createConfigInt(heartbeat.AckMaxChars),
			IncludeReasoning: createConfigBool(heartbeat.IncludeReasoning),
			IsolatedSession:  createConfigBool(heartbeat.IsolatedSession),
			LightContext:     createConfigBool(heartbeat.LightContext),
			Model:            createConfigString[string](heartbeat.Model),
			Prompt:           createConfigString[string](heartbeat.Prompt),
		}
		if hours := heartbeat.ActiveHours; hours != nil {
			active := client.ActiveHoursConfigPatch{
				FromTime: createConfigString[string](hours.FromTime),
				ToTime:   createConfigString[string](hours.ToTime),
				Timezone: createConfigString[string](hours.Timezone),
			}
			if active.FromTime.IsSpecified() || active.ToTime.IsSpecified() || active.Timezone.IsSpecified() {
				value.ActiveHours.Set(active)
			}
		}
		if value.Every.IsSpecified() || value.AckMaxChars.IsSpecified() || value.IncludeReasoning.IsSpecified() ||
			value.IsolatedSession.IsSpecified() || value.LightContext.IsSpecified() || value.Model.IsSpecified() ||
			value.Prompt.IsSpecified() || value.ActiveHours.IsSpecified() {
			patch.Heartbeat.Set(value)
		}
	}
	if compaction := cfg.Compaction; compaction != nil {
		value := client.CompactionConfigPatch{
			MaxActiveTranscriptBytes: createConfigInt(compaction.MaxActiveTranscriptBytes),
			MidTurnPrecheck:          createConfigBool(compaction.MidTurnPrecheck),
			ReserveTokens:            createConfigInt(compaction.ReserveTokens),
			ReserveTokensFloor:       createConfigInt(compaction.ReserveTokensFloor),
			TimeoutSeconds:           createConfigInt(compaction.TimeoutSeconds),
			TruncateAfterCompaction:  createConfigBool(compaction.TruncateAfterCompaction),
		}
		if value.MaxActiveTranscriptBytes.IsSpecified() || value.MidTurnPrecheck.IsSpecified() ||
			value.ReserveTokens.IsSpecified() || value.ReserveTokensFloor.IsSpecified() ||
			value.TimeoutSeconds.IsSpecified() || value.TruncateAfterCompaction.IsSpecified() {
			patch.Compaction.Set(value)
		}
	}
	if session := cfg.Session; session != nil {
		value := client.SessionConfigPatch{WriteLockMaxHoldMs: createConfigInt(session.WriteLockMaxHoldMs)}
		if value.WriteLockMaxHoldMs.IsSpecified() {
			patch.Session.Set(value)
		}
	}
	return patch
}

func createToolSearchConfig(cfg *botToolSearchModel) nullable.Nullable[client.ToolSearchConfigPatch] {
	var result nullable.Nullable[client.ToolSearchConfigPatch]
	if cfg == nil {
		return result
	}
	value := client.ToolSearchConfigPatch{
		Mode:                 createConfigString[client.SparseToolSearchConfigPatchMode](cfg.Mode),
		AutoThresholdPercent: createConfigInt(cfg.AutoThresholdPercent),
	}
	if value.Mode.IsSpecified() || value.AutoThresholdPercent.IsSpecified() {
		result.Set(value)
	}
	return result
}

// Optional+computed config leaves omit null and unknown. Only image leaves
// explicitly override null to mean reset; every assignment stays statically typed.
func createConfigString[T ~string](value types.String) nullable.Nullable[T] {
	var result nullable.Nullable[T]
	if !value.IsNull() && !value.IsUnknown() {
		result.Set(T(value.ValueString()))
	}
	return result
}

func createConfigInt(value types.Int64) nullable.Nullable[int] {
	var result nullable.Nullable[int]
	if !value.IsNull() && !value.IsUnknown() {
		result.Set(int(value.ValueInt64()))
	}
	return result
}

func createConfigBool(value types.Bool) nullable.Nullable[bool] {
	var result nullable.Nullable[bool]
	if !value.IsNull() && !value.IsUnknown() {
		result.Set(value.ValueBool())
	}
	return result
}
