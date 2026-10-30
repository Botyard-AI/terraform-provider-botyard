package provider

// The tfsdk model of botyard_bot_schedule and its conversions (PlatDev #4490).
//
// A schedule's timing and action are both unions on `kind`. In HCL each
// variant is its own attribute: exactly one of `cron`, `interval` or `rrule`,
// and exactly one of `prompt` or `command`. The tfsdk structs below are
// serialization DTOs only. decodeScheduleSpec / decodeScheduleAction turn them
// into the sealed client.ScheduleSpec / client.ScheduleAction variants at the
// boundary, and every wire body is built from those variants, never from the
// DTOs. The reverse direction (setFromResponse) dispatches on the decoded
// response variant with a type switch.

import (
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// --- serialization DTOs (tfsdk) ---------------------------------------------

type botScheduleModel struct {
	ID                     types.String           `tfsdk:"id"`
	BotSlug                types.String           `tfsdk:"bot_slug"`
	Title                  types.String           `tfsdk:"title"`
	ConversationID         types.String           `tfsdk:"conversation_id"`
	Enabled                types.Bool             `tfsdk:"enabled"`
	SkipIfBusy             types.Bool             `tfsdk:"skip_if_busy"`
	CatchupPolicy          types.String           `tfsdk:"catchup_policy"`
	MaxConsecutiveFailures types.Int64            `tfsdk:"max_consecutive_failures"`
	Cron                   *scheduleCronModel     `tfsdk:"cron"`
	Interval               *scheduleIntervalModel `tfsdk:"interval"`
	Rrule                  *scheduleRruleModel    `tfsdk:"rrule"`
	Prompt                 types.String           `tfsdk:"prompt"`
	Command                *scheduleCommandModel  `tfsdk:"command"`
}

type scheduleCronModel struct {
	Cron     types.String `tfsdk:"cron"`
	Timezone types.String `tfsdk:"timezone"`
}

type scheduleIntervalModel struct {
	IntervalSeconds types.Int64  `tfsdk:"interval_seconds"`
	Anchor          types.String `tfsdk:"anchor"`
	Timezone        types.String `tfsdk:"timezone"`
}

type scheduleRruleModel struct {
	Rrule    types.String `tfsdk:"rrule"`
	Timezone types.String `tfsdk:"timezone"`
}

type scheduleCommandModel struct {
	Command          types.String                    `tfsdk:"command"`
	Cwd              types.String                    `tfsdk:"cwd"`
	TimeoutSeconds   types.Int64                     `tfsdk:"timeout_seconds"`
	SecretEnv        []scheduleSecretEnvModel        `tfsdk:"secret_env"`
	OverridableEnv   []types.String                  `tfsdk:"overridable_env"`
	ConcurrencyGroup types.String                    `tfsdk:"concurrency_group"`
	OverlapPolicy    types.String                    `tfsdk:"overlap_policy"`
	LogPath          types.String                    `tfsdk:"log_path"`
	OutputPath       types.String                    `tfsdk:"output_path"`
	ExitCodes        map[string]types.String         `tfsdk:"exit_codes"`
	ExitCodeLabels   map[string]types.String         `tfsdk:"exit_code_labels"`
	Window           *scheduleWindowModel            `tfsdk:"window"`
	OnOutcome        map[string]scheduleRoutingModel `tfsdk:"on_outcome"`
}

type scheduleSecretEnvModel struct {
	Name          types.String `tfsdk:"name"`
	SecretKeyPath types.String `tfsdk:"secret_key_path"`
}

type scheduleWindowModel struct {
	DurationSeconds types.Int64 `tfsdk:"duration_seconds"`
	RecheckSeconds  types.Int64 `tfsdk:"recheck_seconds"`
}

// scheduleRoutingModel is one on_outcome entry. Its action fields are the same
// as scheduleActionsModel's; `retry` and `then` exist only at this level.
type scheduleRoutingModel struct {
	Retry        *scheduleRetryModel   `tfsdk:"retry"`
	Then         *scheduleActionsModel `tfsdk:"then"`
	WakeBot      *scheduleWakeBotModel `tfsdk:"wake_bot"`
	NotifyHumans *scheduleNotifyModel  `tfsdk:"notify_humans"`
	Pause        types.Bool            `tfsdk:"pause"`
}

type scheduleActionsModel struct {
	WakeBot      *scheduleWakeBotModel `tfsdk:"wake_bot"`
	NotifyHumans *scheduleNotifyModel  `tfsdk:"notify_humans"`
	Pause        types.Bool            `tfsdk:"pause"`
}

type scheduleRetryModel struct {
	Max            types.Int64 `tfsdk:"max"`
	BackoffSeconds types.Int64 `tfsdk:"backoff_seconds"`
}

type scheduleWakeBotModel struct {
	Message        types.String `tfsdk:"message"`
	IncludeLogTail types.Int64  `tfsdk:"include_log_tail"`
}

type scheduleNotifyModel struct {
	Recipients []types.String `tfsdk:"recipients"`
	Mode       types.String   `tfsdk:"mode"`
}

// --- DTO -> sealed variants -------------------------------------------------

// decodeScheduleSpec returns the one timing variant the model declares. The
// schema's ExactlyOneOf validator makes "none" or "several" unreachable from a
// valid configuration; the checks here keep a bug from sending a wrong body.
func decodeScheduleSpec(m *botScheduleModel) (client.ScheduleSpec, diag.Diagnostics) {
	var diags diag.Diagnostics
	var specs []client.ScheduleSpec
	if c := m.Cron; c != nil {
		specs = append(specs, &client.ScheduleSpecCron{Spec: client.CronScheduleSpec{
			Cron: c.Cron.ValueString(), Timezone: c.Timezone.ValueStringPointer(),
		}})
	}
	if iv := m.Interval; iv != nil {
		spec := client.IntervalScheduleSpec{
			IntervalSeconds: int(iv.IntervalSeconds.ValueInt64()), Timezone: iv.Timezone.ValueStringPointer(),
		}
		if !iv.Anchor.IsNull() {
			at, err := time.Parse(time.RFC3339, iv.Anchor.ValueString())
			if err != nil {
				diags.AddError("Invalid interval anchor", err.Error())
				return nil, diags
			}
			spec.Anchor = &at
		}
		specs = append(specs, &client.ScheduleSpecInterval{Spec: spec})
	}
	if r := m.Rrule; r != nil {
		specs = append(specs, &client.ScheduleSpecRrule{Spec: client.RRuleScheduleSpec{
			Rrule: r.Rrule.ValueString(), Timezone: r.Timezone.ValueStringPointer(),
		}})
	}
	if len(specs) != 1 {
		diags.AddError("Invalid schedule timing",
			fmt.Sprintf("Exactly one of cron, interval or rrule must be set; got %d. This is a bug in the provider.", len(specs)))
		return nil, diags
	}
	return specs[0], diags
}

// decodeScheduleAction returns the one action variant the model declares.
func decodeScheduleAction(m *botScheduleModel) (client.ScheduleAction, diag.Diagnostics) {
	var diags diag.Diagnostics
	switch {
	case !m.Prompt.IsNull() && m.Command == nil:
		return &client.ScheduleActionPrompt{Action: client.PromptScheduleAction{Prompt: m.Prompt.ValueString()}}, diags
	case m.Prompt.IsNull() && m.Command != nil:
		return &client.ScheduleActionCommand{Action: m.Command.wire()}, diags
	default:
		diags.AddError("Invalid schedule action",
			"Exactly one of prompt or command must be set. This is a bug in the provider.")
		return nil, diags
	}
}

// wire builds the command action body. Collections are always sent, empty
// rather than absent, so the plan and the state encode identically.
func (c *scheduleCommandModel) wire() client.CommandScheduleAction {
	secretEnv := make([]client.SecretEnvBinding, 0, len(c.SecretEnv))
	for _, s := range c.SecretEnv {
		secretEnv = append(secretEnv, client.SecretEnvBinding{
			Name: s.Name.ValueString(), SecretKeyPath: s.SecretKeyPath.ValueString(),
		})
	}
	exitCodes := make(map[string]client.CommandScheduleActionExitCodes, len(c.ExitCodes))
	for k, v := range c.ExitCodes {
		exitCodes[k] = client.CommandScheduleActionExitCodes(v.ValueString())
	}
	onOutcome := make(map[string]client.OutcomeRouting, len(c.OnOutcome))
	for k, r := range c.OnOutcome {
		onOutcome[k] = r.wire()
	}
	overlap := client.ScheduleOverlapPolicy(c.OverlapPolicy.ValueString())
	labels := stringMapValues(c.ExitCodeLabels)
	overridable := stringValues(c.OverridableEnv)
	out := client.CommandScheduleAction{
		Command:          c.Command.ValueString(),
		Cwd:              c.Cwd.ValueStringPointer(),
		TimeoutSeconds:   int64ToIntPtr(c.TimeoutSeconds),
		SecretEnv:        &secretEnv,
		OverridableEnv:   &overridable,
		ConcurrencyGroup: c.ConcurrencyGroup.ValueStringPointer(),
		OverlapPolicy:    &overlap,
		LogPath:          c.LogPath.ValueStringPointer(),
		OutputPath:       c.OutputPath.ValueStringPointer(),
		ExitCodes:        &exitCodes,
		ExitCodeLabels:   &labels,
		OnOutcome:        &onOutcome,
	}
	if w := c.Window; w != nil {
		out.Window = &client.CommandWindow{
			DurationSeconds: int(w.DurationSeconds.ValueInt64()),
			RecheckSeconds:  int(w.RecheckSeconds.ValueInt64()),
		}
	}
	return out
}

func (r scheduleRoutingModel) wire() client.OutcomeRouting {
	out := client.OutcomeRouting{
		WakeBot:      r.WakeBot.wire(),
		NotifyHumans: r.NotifyHumans.wire(),
		Pause:        r.Pause.ValueBoolPointer(),
	}
	if r.Retry != nil {
		out.Retry = &client.RetryPolicy{
			Max:            int(r.Retry.Max.ValueInt64()),
			BackoffSeconds: int64ToIntPtr(r.Retry.BackoffSeconds),
		}
	}
	if t := r.Then; t != nil {
		out.Then = &client.OutcomeActions{
			WakeBot:      t.WakeBot.wire(),
			NotifyHumans: t.NotifyHumans.wire(),
			Pause:        t.Pause.ValueBoolPointer(),
		}
	}
	return out
}

func (w *scheduleWakeBotModel) wire() *client.WakeBotAction {
	if w == nil {
		return nil
	}
	return &client.WakeBotAction{Message: w.Message.ValueString(), IncludeLogTail: int64ToIntPtr(w.IncludeLogTail)}
}

func (n *scheduleNotifyModel) wire() *client.NotifyHumansAction {
	if n == nil {
		return nil
	}
	recipients := stringValues(n.Recipients)
	mode := client.ScheduleNotifyMode(n.Mode.ValueString())
	return &client.NotifyHumansAction{Recipients: &recipients, Mode: &mode}
}

// --- response -> DTO --------------------------------------------------------

// setFromResponse overwrites the model with a schedule read from the API.
// `prior` is the plan or state the read follows, used only to keep the
// practitioner's spelling of an interval anchor when it names the same instant.
// Create and Update pass the receiver itself as `prior`, so everything read
// from it is captured before the model is touched.
func (m *botScheduleModel) setFromResponse(r client.ScheduleResponse, prior *botScheduleModel) diag.Diagnostics {
	var diags diag.Diagnostics
	priorAnchor := types.StringNull()
	if prior != nil && prior.Interval != nil {
		priorAnchor = prior.Interval.Anchor
	}
	spec, err := client.DecodeScheduleSpec(r)
	if err != nil {
		diags.AddError("Unsupported schedule timing",
			fmt.Sprintf("Schedule %s cannot be managed by this provider version: %s", r.ScheduleId, err))
		return diags
	}
	action, err := client.DecodeScheduleAction(r.Action)
	if err != nil {
		diags.AddError("Unsupported schedule action",
			fmt.Sprintf("Schedule %s cannot be managed by this provider version: %s", r.ScheduleId, err))
		return diags
	}

	m.ID = types.StringValue(r.ScheduleId)
	m.Title = types.StringPointerValue(r.Title)
	m.ConversationID = types.StringValue(r.ConversationId)
	m.Enabled = types.BoolValue(r.Enabled)
	m.SkipIfBusy = types.BoolValue(r.SkipIfBusy)
	m.CatchupPolicy = types.StringValue(string(r.CatchupPolicy))
	m.MaxConsecutiveFailures = types.Int64Value(int64(r.MaxConsecutiveFailures))

	m.Cron, m.Interval, m.Rrule = nil, nil, nil
	switch s := spec.(type) {
	case *client.ScheduleSpecCron:
		m.Cron = &scheduleCronModel{Cron: types.StringValue(s.Spec.Cron), Timezone: types.StringPointerValue(s.Spec.Timezone)}
	case *client.ScheduleSpecInterval:
		m.Interval = &scheduleIntervalModel{
			IntervalSeconds: types.Int64Value(int64(s.Spec.IntervalSeconds)),
			Anchor:          anchorValue(s.Spec.Anchor, priorAnchor),
			Timezone:        types.StringPointerValue(s.Spec.Timezone),
		}
	case *client.ScheduleSpecRrule:
		m.Rrule = &scheduleRruleModel{Rrule: types.StringValue(s.Spec.Rrule), Timezone: types.StringPointerValue(s.Spec.Timezone)}
	default:
		diags.AddError("Unsupported schedule timing", fmt.Sprintf("Unhandled timing variant %T. This is a bug in the provider.", spec))
		return diags
	}

	m.Prompt, m.Command = types.StringNull(), nil
	switch a := action.(type) {
	case *client.ScheduleActionPrompt:
		m.Prompt = types.StringValue(a.Action.Prompt)
	case *client.ScheduleActionCommand:
		m.Command = commandFromWire(a.Action)
	default:
		diags.AddError("Unsupported schedule action", fmt.Sprintf("Unhandled action variant %T. This is a bug in the provider.", action))
	}
	return diags
}

// anchorValue keeps the prior spelling of an anchor naming the same instant.
// The API echoes the anchor in Python isoformat (`+00:00`, never `Z`), so
// adopting its spelling would turn `2026-01-01T09:00:00Z` into a permanent
// diff against configuration.
func anchorValue(got *time.Time, prior types.String) types.String {
	if got == nil {
		return types.StringNull()
	}
	if !prior.IsNull() && !prior.IsUnknown() {
		if was, err := time.Parse(time.RFC3339, prior.ValueString()); err == nil && was.Equal(*got) {
			return prior
		}
	}
	return types.StringValue(got.Format(time.RFC3339Nano))
}

func commandFromWire(a client.CommandScheduleAction) *scheduleCommandModel {
	out := &scheduleCommandModel{
		Command:          types.StringValue(a.Command),
		Cwd:              types.StringPointerValue(a.Cwd),
		TimeoutSeconds:   intPtrToInt64(a.TimeoutSeconds),
		SecretEnv:        []scheduleSecretEnvModel{},
		OverridableEnv:   stringsToValues(derefSlice(a.OverridableEnv)),
		ConcurrencyGroup: types.StringPointerValue(a.ConcurrencyGroup),
		OverlapPolicy:    types.StringValue(string(client.ScheduleOverlapPolicySkip)),
		LogPath:          types.StringPointerValue(a.LogPath),
		OutputPath:       types.StringPointerValue(a.OutputPath),
		ExitCodes:        map[string]types.String{},
		ExitCodeLabels:   map[string]types.String{},
		OnOutcome:        map[string]scheduleRoutingModel{},
	}
	if a.OverlapPolicy != nil {
		out.OverlapPolicy = types.StringValue(string(*a.OverlapPolicy))
	}
	for _, s := range derefSlice(a.SecretEnv) {
		out.SecretEnv = append(out.SecretEnv, scheduleSecretEnvModel{
			Name: types.StringValue(s.Name), SecretKeyPath: types.StringValue(s.SecretKeyPath),
		})
	}
	if a.ExitCodes != nil {
		for k, v := range *a.ExitCodes {
			out.ExitCodes[k] = types.StringValue(string(v))
		}
	}
	if a.ExitCodeLabels != nil {
		for k, v := range *a.ExitCodeLabels {
			out.ExitCodeLabels[k] = types.StringValue(v)
		}
	}
	if w := a.Window; w != nil {
		out.Window = &scheduleWindowModel{
			DurationSeconds: types.Int64Value(int64(w.DurationSeconds)),
			RecheckSeconds:  types.Int64Value(int64(w.RecheckSeconds)),
		}
	}
	if a.OnOutcome != nil {
		for k, r := range *a.OnOutcome {
			out.OnOutcome[k] = routingFromWire(r)
		}
	}
	return out
}

func routingFromWire(r client.OutcomeRouting) scheduleRoutingModel {
	out := scheduleRoutingModel{
		WakeBot:      wakeBotFromWire(r.WakeBot),
		NotifyHumans: notifyFromWire(r.NotifyHumans),
		Pause:        boolOrFalse(r.Pause),
	}
	if r.Retry != nil {
		out.Retry = &scheduleRetryModel{
			Max:            types.Int64Value(int64(r.Retry.Max)),
			BackoffSeconds: intOrZero(r.Retry.BackoffSeconds),
		}
	}
	if t := r.Then; t != nil {
		out.Then = &scheduleActionsModel{
			WakeBot:      wakeBotFromWire(t.WakeBot),
			NotifyHumans: notifyFromWire(t.NotifyHumans),
			Pause:        boolOrFalse(t.Pause),
		}
	}
	return out
}

func wakeBotFromWire(w *client.WakeBotAction) *scheduleWakeBotModel {
	if w == nil {
		return nil
	}
	return &scheduleWakeBotModel{Message: types.StringValue(w.Message), IncludeLogTail: intOrZero(w.IncludeLogTail)}
}

func notifyFromWire(n *client.NotifyHumansAction) *scheduleNotifyModel {
	if n == nil {
		return nil
	}
	mode := types.StringValue(string(client.ScheduleNotifyModeOnChange))
	if n.Mode != nil {
		mode = types.StringValue(string(*n.Mode))
	}
	return &scheduleNotifyModel{Recipients: stringsToValues(derefSlice(n.Recipients)), Mode: mode}
}

// --- small value helpers ----------------------------------------------------

func intOrZero(v *int) types.Int64 {
	if v == nil {
		return types.Int64Value(0)
	}
	return types.Int64Value(int64(*v))
}

func boolOrFalse(v *bool) types.Bool {
	return types.BoolValue(v != nil && *v)
}

func derefSlice[T any](v *[]T) []T {
	if v == nil {
		return nil
	}
	return *v
}

func stringValues(vs []types.String) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.ValueString())
	}
	return out
}

func stringsToValues(vs []string) []types.String {
	out := make([]types.String, 0, len(vs))
	for _, v := range vs {
		out = append(out, types.StringValue(v))
	}
	return out
}

func stringMapValues(m map[string]types.String) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v.ValueString()
	}
	return out
}
