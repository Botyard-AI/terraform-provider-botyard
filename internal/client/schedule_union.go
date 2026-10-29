package client

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Schedule unions, decoded by their discriminator (PlatDev #4490).
//
// A schedule's timing (`schedule`) and action (`action`) are both unions on
// `kind`. The generated client gives As*/From* helpers but no typed dispatch,
// and the response carries the timing in a different shape from the request:
// `spec_kind` + an untyped `spec` object + a top-level `timezone`. The decoders
// here turn both into sealed variants, so the provider dispatches with a type
// switch and never probes payload fields to guess a kind.
//
// An unknown kind is an error, never a silent default. The API already models
// an `event` timing kind that the public create/update bodies do not accept, so
// a schedule created through another surface can carry a kind this provider
// cannot represent.

// Discriminator values for a schedule's timing (`schedule.kind` / `spec_kind`).
const (
	ScheduleKindCron     = "cron"
	ScheduleKindInterval = "interval"
	ScheduleKindRrule    = "rrule"
)

// Discriminator values for a schedule's action (`action.kind`).
const (
	ScheduleActionKindPrompt  = "prompt"
	ScheduleActionKindCommand = "command"
)

// ScheduleSpec is a decoded schedule timing. Sealed: dispatch with a type
// switch on *ScheduleSpecCron / *ScheduleSpecInterval / *ScheduleSpecRrule.
type ScheduleSpec interface {
	// Kind is the `kind` discriminator value of this variant.
	Kind() string
	createUnion() (ScheduleCreate_Schedule, error)
	updateUnion() (ScheduleUpdateInput_Schedule, error)
}

// ScheduleSpecCron fires on a cron expression.
type ScheduleSpecCron struct{ Spec CronScheduleSpec }

// ScheduleSpecInterval fires on a fixed period.
type ScheduleSpecInterval struct{ Spec IntervalScheduleSpec }

// ScheduleSpecRrule fires on an RFC 5545 rule.
type ScheduleSpecRrule struct{ Spec RRuleScheduleSpec }

func (*ScheduleSpecCron) Kind() string     { return ScheduleKindCron }
func (*ScheduleSpecInterval) Kind() string { return ScheduleKindInterval }
func (*ScheduleSpecRrule) Kind() string    { return ScheduleKindRrule }

func (s *ScheduleSpecCron) createUnion() (u ScheduleCreate_Schedule, err error) {
	s.Spec.Kind = CronScheduleSpecKindCron
	return u, u.FromCronScheduleSpec(s.Spec)
}
func (s *ScheduleSpecInterval) createUnion() (u ScheduleCreate_Schedule, err error) {
	s.Spec.Kind = IntervalScheduleSpecKindInterval
	return u, u.FromIntervalScheduleSpec(s.Spec)
}
func (s *ScheduleSpecRrule) createUnion() (u ScheduleCreate_Schedule, err error) {
	s.Spec.Kind = RRuleScheduleSpecKindRrule
	return u, u.FromRRuleScheduleSpec(s.Spec)
}
func (s *ScheduleSpecCron) updateUnion() (u ScheduleUpdateInput_Schedule, err error) {
	s.Spec.Kind = CronScheduleSpecKindCron
	return u, u.FromCronScheduleSpec(s.Spec)
}
func (s *ScheduleSpecInterval) updateUnion() (u ScheduleUpdateInput_Schedule, err error) {
	s.Spec.Kind = IntervalScheduleSpecKindInterval
	return u, u.FromIntervalScheduleSpec(s.Spec)
}
func (s *ScheduleSpecRrule) updateUnion() (u ScheduleUpdateInput_Schedule, err error) {
	s.Spec.Kind = RRuleScheduleSpecKindRrule
	return u, u.FromRRuleScheduleSpec(s.Spec)
}

// ScheduleAction is a decoded schedule action. Sealed: dispatch with a type
// switch on *ScheduleActionPrompt / *ScheduleActionCommand.
type ScheduleAction interface {
	// Kind is the `kind` discriminator value of this variant.
	Kind() string
	createUnion() (ScheduleCreate_Action, error)
	updateUnion() (ScheduleUpdateInput_Action, error)
}

// ScheduleActionPrompt wakes the bot with a prompt.
type ScheduleActionPrompt struct{ Action PromptScheduleAction }

// ScheduleActionCommand runs a command on the bot's runtime.
type ScheduleActionCommand struct{ Action CommandScheduleAction }

func (*ScheduleActionPrompt) Kind() string  { return ScheduleActionKindPrompt }
func (*ScheduleActionCommand) Kind() string { return ScheduleActionKindCommand }

func (a *ScheduleActionPrompt) createUnion() (u ScheduleCreate_Action, err error) {
	a.Action.Kind = PromptScheduleActionKindPrompt
	return u, u.FromPromptScheduleAction(a.Action)
}
func (a *ScheduleActionCommand) createUnion() (u ScheduleCreate_Action, err error) {
	a.Action.Kind = CommandScheduleActionKindCommand
	return u, u.FromCommandScheduleAction(a.Action)
}
func (a *ScheduleActionPrompt) updateUnion() (u ScheduleUpdateInput_Action, err error) {
	a.Action.Kind = PromptScheduleActionKindPrompt
	return u, u.FromPromptScheduleAction(a.Action)
}
func (a *ScheduleActionCommand) updateUnion() (u ScheduleUpdateInput_Action, err error) {
	a.Action.Kind = CommandScheduleActionKindCommand
	return u, u.FromCommandScheduleAction(a.Action)
}

// DecodeScheduleSpec reads a schedule's timing from a response: `spec_kind`
// picks the variant, `spec` carries its fields and `timezone` sits beside it.
func DecodeScheduleSpec(r ScheduleResponse) (ScheduleSpec, error) {
	known := []string{ScheduleKindCron, ScheduleKindInterval, ScheduleKindRrule}
	raw, err := json.Marshal(r.Spec)
	if err != nil {
		return nil, fmt.Errorf("reading schedule spec: %w", err)
	}
	tz := r.Timezone
	switch kind := string(r.SpecKind); kind {
	case ScheduleKindCron:
		var v CronScheduleSpec
		if err := strictUnmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("decoding cron spec: %w", err)
		}
		v.Kind, v.Timezone = CronScheduleSpecKindCron, &tz
		return &ScheduleSpecCron{Spec: v}, nil
	case ScheduleKindInterval:
		var v IntervalScheduleSpec
		if err := strictUnmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("decoding interval spec: %w", err)
		}
		v.Kind, v.Timezone = IntervalScheduleSpecKindInterval, &tz
		return &ScheduleSpecInterval{Spec: v}, nil
	case ScheduleKindRrule:
		var v RRuleScheduleSpec
		if err := strictUnmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("decoding rrule spec: %w", err)
		}
		v.Kind, v.Timezone = RRuleScheduleSpecKindRrule, &tz
		return &ScheduleSpecRrule{Spec: v}, nil
	default:
		return nil, &UnknownDiscriminatorError{
			Union: "ScheduleResponse.spec", Field: "spec_kind", Value: kind, KnownValues: known,
			PresentInJSON: kind != "",
		}
	}
}

// strictUnmarshal refuses fields the variant does not declare. The persisted
// `spec` of a known kind carries only that kind's fields, so an extra one means
// the API changed shape and silently dropping it would hide drift.
func strictUnmarshal(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// DecodeScheduleAction dispatches a response action on its `kind` field.
func DecodeScheduleAction(a ScheduleResponse_Action) (ScheduleAction, error) {
	known := []string{ScheduleActionKindPrompt, ScheduleActionKindCommand}
	raw, err := a.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("reading schedule action: %w", err)
	}
	var disc struct {
		Kind *string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &disc); err != nil {
		return nil, fmt.Errorf("decoding schedule action kind: %w", err)
	}
	if disc.Kind == nil {
		return nil, &UnknownDiscriminatorError{Union: "ScheduleResponse.action", Field: "kind", KnownValues: known}
	}
	switch kind := *disc.Kind; kind {
	case ScheduleActionKindPrompt:
		v, err := a.AsPromptScheduleAction()
		if err != nil {
			return nil, fmt.Errorf("decoding prompt action: %w", err)
		}
		return &ScheduleActionPrompt{Action: v}, nil
	case ScheduleActionKindCommand:
		v, err := a.AsCommandScheduleAction()
		if err != nil {
			return nil, fmt.Errorf("decoding command action: %w", err)
		}
		return &ScheduleActionCommand{Action: v}, nil
	default:
		return nil, &UnknownDiscriminatorError{
			Union: "ScheduleResponse.action", Field: "kind", Value: kind, KnownValues: known, PresentInJSON: true,
		}
	}
}

// NewScheduleCreate builds a create body from decoded variants. It sets
// `action` and leaves the deprecated `prompt` alias unset. The generated struct
// still writes that key, as `"prompt": null`, which the API treats as absent;
// the key is present on the wire, but never with a value.
func NewScheduleCreate(spec ScheduleSpec, action ScheduleAction) (ScheduleCreate, error) {
	s, err := spec.createUnion()
	if err != nil {
		return ScheduleCreate{}, fmt.Errorf("encoding schedule %s: %w", spec.Kind(), err)
	}
	a, err := action.createUnion()
	if err != nil {
		return ScheduleCreate{}, fmt.Errorf("encoding %s action: %w", action.Kind(), err)
	}
	return ScheduleCreate{Schedule: s, Action: &a}, nil
}

// The sparse PATCH body is the generated ScheduleUpdateInput. Every field is a
// pointer with omitempty; `title` is generated as *nullable.Nullable[string]
// (scripts/generate-client.sh TRISTATE_FIELDS), so it can be omitted, set, or
// cleared with null. Never set its deprecated `Prompt` alias: actions go in
// `Action`, through SetScheduleUpdateAction.

// SetScheduleUpdateSpec puts a new timing into an update body.
func SetScheduleUpdateSpec(in *ScheduleUpdateInput, spec ScheduleSpec) error {
	u, err := spec.updateUnion()
	if err != nil {
		return fmt.Errorf("encoding schedule %s: %w", spec.Kind(), err)
	}
	in.Schedule = &u
	return nil
}

// SetScheduleUpdateAction puts a new action into an update body.
func SetScheduleUpdateAction(in *ScheduleUpdateInput, action ScheduleAction) error {
	u, err := action.updateUnion()
	if err != nil {
		return fmt.Errorf("encoding %s action: %w", action.Kind(), err)
	}
	in.Action = &u
	return nil
}

// IsEmptyScheduleUpdate reports whether an update body would change nothing.
func IsEmptyScheduleUpdate(in ScheduleUpdateInput) bool { return in == ScheduleUpdateInput{} }

// SameScheduleSpec reports whether two timings encode to the same request
// body, so a PATCH can leave an unchanged timing out.
func SameScheduleSpec(a, b ScheduleSpec) (bool, error) {
	ua, err := a.updateUnion()
	if err != nil {
		return false, err
	}
	ub, err := b.updateUnion()
	if err != nil {
		return false, err
	}
	return sameJSON(ua, ub)
}

// SameScheduleAction reports whether two actions encode to the same request
// body, so a PATCH can leave an unchanged action out.
func SameScheduleAction(a, b ScheduleAction) (bool, error) {
	ua, err := a.updateUnion()
	if err != nil {
		return false, err
	}
	ub, err := b.updateUnion()
	if err != nil {
		return false, err
	}
	return sameJSON(ua, ub)
}

func sameJSON(a, b json.Marshaler) (bool, error) {
	ja, err := a.MarshalJSON()
	if err != nil {
		return false, err
	}
	jb, err := b.MarshalJSON()
	if err != nil {
		return false, err
	}
	return bytes.Equal(ja, jb), nil
}
