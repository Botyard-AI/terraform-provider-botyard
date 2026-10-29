package client

import (
	"encoding/json"
	"errors"
	"github.com/oapi-codegen/nullable"
	"strings"
	"testing"
)

func TestDecodeScheduleSpec(t *testing.T) {
	r := ScheduleResponse{SpecKind: ScheduleSpecKindInterval, Timezone: "Europe/Copenhagen",
		Spec: map[string]interface{}{"interval_seconds": 900, "anchor": "2026-01-01T09:00:00+00:00"}}
	spec, err := DecodeScheduleSpec(r)
	if err != nil {
		t.Fatal(err)
	}
	iv, ok := spec.(*ScheduleSpecInterval)
	if !ok || iv.Spec.IntervalSeconds != 900 || iv.Spec.Anchor == nil || *iv.Spec.Timezone != "Europe/Copenhagen" {
		t.Fatalf("decoded %#v", spec)
	}

	// A kind the provider cannot model is a typed error, not a default.
	_, err = DecodeScheduleSpec(ScheduleResponse{SpecKind: ScheduleSpecKindEvent, Spec: map[string]interface{}{"event": "x"}})
	var unknown *UnknownDiscriminatorError
	if !errors.As(err, &unknown) || unknown.Value != "event" {
		t.Fatalf("event kind: err = %v", err)
	}

	// A field the kind does not declare means the API changed shape.
	_, err = DecodeScheduleSpec(ScheduleResponse{SpecKind: ScheduleSpecKindCron, Spec: map[string]interface{}{"cron": "* * * * *", "jitter": 5}})
	if err == nil || !strings.Contains(err.Error(), "jitter") {
		t.Fatalf("extra spec field: err = %v", err)
	}
}

func TestDecodeScheduleAction(t *testing.T) {
	var a ScheduleResponse_Action
	if err := json.Unmarshal([]byte(`{"kind":"command","command":"./x","exit_codes":{"0":"success"}}`), &a); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeScheduleAction(a)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := got.(*ScheduleActionCommand); !ok || c.Action.Command != "./x" {
		t.Fatalf("decoded %#v", got)
	}
	if err := json.Unmarshal([]byte(`{"kind":"webhook"}`), &a); err != nil {
		t.Fatal(err)
	}
	var unknown *UnknownDiscriminatorError
	if _, err := DecodeScheduleAction(a); !errors.As(err, &unknown) || unknown.Value != "webhook" {
		t.Fatalf("unknown kind: err = %v", err)
	}
}

// The create body sets `action` with its discriminator and never a non-null
// deprecated `prompt`.
func TestNewScheduleCreate(t *testing.T) {
	tz := "UTC"
	body, err := NewScheduleCreate(&ScheduleSpecCron{Spec: CronScheduleSpec{Cron: "0 9 * * *", Timezone: &tz}},
		&ScheduleActionPrompt{Action: PromptScheduleAction{Prompt: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	var got map[string]json.RawMessage
	_ = json.Unmarshal(raw, &got)
	if string(got["prompt"]) != "null" {
		t.Fatalf("prompt = %s, want null", got["prompt"])
	}
	if string(got["action"]) != `{"kind":"prompt","prompt":"hi"}` {
		t.Fatalf("action = %s", got["action"])
	}
	if string(got["schedule"]) != `{"cron":"0 9 * * *","kind":"cron","timezone":"UTC"}` {
		t.Fatalf("schedule = %s", got["schedule"])
	}
}

// The generated update body sends only the keys set on it: an unset title is
// omitted, a cleared one is an explicit null, and the deprecated prompt alias
// never appears unless set.
func TestScheduleUpdateSparse(t *testing.T) {
	enabled := true
	cleared := nullable.NewNullNullable[string]()
	named := nullable.NewNullableWithValue("daily")
	for name, tc := range map[string]struct {
		in   ScheduleUpdateInput
		want string
	}{
		"empty":       {ScheduleUpdateInput{}, `{}`},
		"enable only": {ScheduleUpdateInput{Enabled: &enabled}, `{"enabled":true}`},
		"clear title": {ScheduleUpdateInput{Title: &cleared}, `{"title":null}`},
		"set title":   {ScheduleUpdateInput{Title: &named}, `{"title":"daily"}`},
	} {
		raw, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != tc.want {
			t.Errorf("%s: %s, want %s", name, raw, tc.want)
		}
		if (name == "empty") != IsEmptyScheduleUpdate(tc.in) {
			t.Errorf("%s: IsEmptyScheduleUpdate = %v", name, IsEmptyScheduleUpdate(tc.in))
		}
	}
}

// SetScheduleUpdateAction never routes a prompt through the deprecated alias.
func TestScheduleUpdateActionNotAlias(t *testing.T) {
	var in ScheduleUpdateInput
	if err := SetScheduleUpdateAction(&in, &ScheduleActionPrompt{Action: PromptScheduleAction{Kind: "prompt", Prompt: "hi"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"action":{"kind":"prompt","prompt":"hi"}}` {
		t.Fatalf("got %s", raw)
	}
}
