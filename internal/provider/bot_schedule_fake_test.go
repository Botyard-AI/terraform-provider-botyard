package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// fakeScheduleAPI serves one bot's schedules the way botyard's
// routes/schedules.py does (PlatDev #4490):
//
//   - responses echo every command-action default (overlap_policy, exit_codes,
//     empty lists and maps, routing defaults), as the API's model_dump does;
//   - an interval anchor is echoed in Python isoformat (`+00:00`, never `Z`;
//     truncated to microseconds, with `.ffffff` only when nonzero);
//   - `timezone` defaults to UTC and moves to the top level of the response;
//   - a command action without the schedule_command permission is refused with
//     the API's 403 text;
//   - PATCH changes only the keys present, and `title: null` clears the title.
//
// Every create and PATCH body is recorded so tests can assert what was sent.
type fakeScheduleAPI struct {
	t *testing.T

	mu          sync.Mutex
	order       []string
	schedules   map[string]*fakeSchedule
	nextID      int
	denyCommand bool
	creates     []map[string]json.RawMessage
	patches     []map[string]json.RawMessage
}

type fakeSchedule struct {
	id, conversationID string
	title              *string
	enabled, skipBusy  bool
	catchup            string
	maxFailures        int
	// The timing as the API persists it: spec_kind, an untyped spec object and
	// the timezone beside them. Untyped because the response contract is
	// (`spec` is an object), and because a kind this provider cannot model
	// (`event`) must still round-trip through the list endpoint.
	specKind client.ScheduleSpecKind
	spec     map[string]any
	timezone string
	// The action as the API echoes it, every default filled in.
	actionKind string
	action     client.ScheduleResponse_Action
}

// specJSON renders the persisted spec with sorted keys, for assertions.
func (s *fakeSchedule) specJSON() string {
	raw, err := json.Marshal(s.spec)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(raw)
}

const (
	fakeScheduleBot  = "sched-bot"
	fakeSchedulePath = "/v1/orgs/org-1/bots/" + fakeScheduleBot + "/schedules"
)

func newFakeScheduleAPI(t *testing.T) (*fakeScheduleAPI, *httptest.Server) {
	t.Helper()
	api := &fakeScheduleAPI{t: t, schedules: map[string]*fakeSchedule{}}
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return api, srv
}

func (a *fakeScheduleAPI) set(fn func(*fakeScheduleAPI)) func() {
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		fn(a)
	}
}

// only returns the single stored schedule, failing the test if there is not one.
func (a *fakeScheduleAPI) only() *fakeSchedule {
	a.t.Helper()
	if len(a.order) != 1 {
		a.t.Fatalf("fake holds %d schedules, want 1", len(a.order))
	}
	return a.schedules[a.order[0]]
}

func (a *fakeScheduleAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer byk_test" {
		http.Error(w, `{"detail":"unauthenticated"}`, http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, fakeSchedulePath)
	if !ok {
		writeJSON(w, http.StatusNotFound, `{"detail":"Bot not found"}`)
		return
	}
	id := strings.TrimPrefix(rest, "/")
	switch {
	case id == "" && r.Method == http.MethodGet:
		items := make([]json.RawMessage, 0, len(a.order))
		for _, sid := range a.order {
			items = append(items, a.render(a.schedules[sid]))
		}
		body, _ := json.Marshal(map[string][]json.RawMessage{"schedules": items})
		writeJSON(w, http.StatusOK, string(body))
	case id == "" && r.Method == http.MethodPost:
		a.create(w, r)
	case r.Method == http.MethodGet:
		if s := a.schedules[id]; s != nil {
			writeJSON(w, http.StatusOK, string(a.render(s)))
			return
		}
		writeJSON(w, http.StatusNotFound, `{"detail":"Schedule not found"}`)
	case r.Method == http.MethodPatch:
		a.patch(w, r, id)
	case r.Method == http.MethodDelete:
		if a.schedules[id] == nil {
			writeJSON(w, http.StatusNotFound, `{"detail":"Schedule not found"}`)
			return
		}
		a.remove(id)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, `{"detail":"method"}`)
	}
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (a *fakeScheduleAPI) remove(id string) {
	delete(a.schedules, id)
	for i, sid := range a.order {
		if sid == id {
			a.order = append(a.order[:i], a.order[i+1:]...)
			return
		}
	}
}

func readBody(r *http.Request) (map[string]json.RawMessage, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var body map[string]json.RawMessage
	return body, json.Unmarshal(raw, &body)
}

func isNull(v json.RawMessage) bool { return v == nil || bytes.Equal(v, []byte("null")) }

func (a *fakeScheduleAPI) create(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, `{"detail":"bad json"}`)
		return
	}
	a.creates = append(a.creates, body)
	if !isNull(body["prompt"]) {
		a.t.Errorf("create sent the deprecated prompt alias: %s", body["prompt"])
	}
	if isNull(body["action"]) {
		writeJSON(w, http.StatusUnprocessableEntity, `{"detail":"Provide exactly one of 'action' or the deprecated 'prompt'."}`)
		return
	}
	action, kind, err := normalizeFakeAction(body["action"])
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf(`{"detail":%q}`, err.Error()))
		return
	}
	if kind == client.ScheduleActionKindCommand && a.denyCommand {
		writeJSON(w, http.StatusForbidden, `{"detail":"Configuring a command schedule requires the 'schedule:schedule_command.create' permission."}`)
		return
	}
	a.nextID++
	s := &fakeSchedule{
		id: fmt.Sprintf("sch-%d", a.nextID), enabled: true, catchup: "skip", actionKind: kind, action: action,
		conversationID: fmt.Sprintf("conv-auto-%d", a.nextID),
	}
	if err := s.setSpec(body["schedule"]); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf(`{"detail":%q}`, err.Error()))
		return
	}
	if err := s.apply(body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf(`{"detail":%q}`, err.Error()))
		return
	}
	a.schedules[s.id] = s
	a.order = append(a.order, s.id)
	writeJSON(w, http.StatusCreated, string(a.render(s)))
}

func (a *fakeScheduleAPI) patch(w http.ResponseWriter, r *http.Request, id string) {
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, `{"detail":"bad json"}`)
		return
	}
	a.patches = append(a.patches, body)
	if _, sent := body["prompt"]; sent {
		a.t.Errorf("PATCH sent the deprecated prompt alias")
	}
	s := a.schedules[id]
	if s == nil {
		writeJSON(w, http.StatusNotFound, `{"detail":"Schedule not found"}`)
		return
	}
	next := *s
	if raw, ok := body["action"]; ok {
		action, kind, err := normalizeFakeAction(raw)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf(`{"detail":%q}`, err.Error()))
			return
		}
		if kind == client.ScheduleActionKindCommand && a.denyCommand {
			writeJSON(w, http.StatusForbidden, `{"detail":"Configuring a command schedule requires the 'schedule:schedule_command.update' permission."}`)
			return
		}
		next.actionKind, next.action = kind, action
	}
	if raw, ok := body["schedule"]; ok {
		if err := next.setSpec(raw); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf(`{"detail":%q}`, err.Error()))
			return
		}
	}
	if raw, ok := body["title"]; ok {
		next.title = nil
		if !isNull(raw) {
			var t string
			_ = json.Unmarshal(raw, &t)
			next.title = &t
		}
	}
	if raw, ok := body["conversation_id"]; ok {
		if isNull(raw) {
			writeJSON(w, http.StatusUnprocessableEntity, `{"detail":"conversation_id is not nullable"}`)
			return
		}
		_ = json.Unmarshal(raw, &next.conversationID)
	}
	if err := next.apply(body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, fmt.Sprintf(`{"detail":%q}`, err.Error()))
		return
	}
	*s = next
	writeJSON(w, http.StatusOK, string(a.render(s)))
}

// apply sets the scalar fields present in a create or PATCH body.
func (s *fakeSchedule) apply(body map[string]json.RawMessage) error {
	for key, dst := range map[string]any{
		"enabled": &s.enabled, "skip_if_busy": &s.skipBusy, "catchup_policy": &s.catchup,
		"max_consecutive_failures": &s.maxFailures,
	} {
		if raw, ok := body[key]; ok && !isNull(raw) {
			if err := json.Unmarshal(raw, dst); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	if raw, ok := body["title"]; ok && !isNull(raw) && s.title == nil {
		var t string
		_ = json.Unmarshal(raw, &t)
		s.title = &t
	}
	if raw, ok := body["conversation_id"]; ok && !isNull(raw) {
		_ = json.Unmarshal(raw, &s.conversationID)
	}
	return nil
}

// pythonISOFormat renders a timestamp the way the API echoes an anchor: a
// Python datetime holds microseconds, so anything finer is truncated, and
// isoformat writes `.ffffff` only when the microseconds are nonzero and the
// offset as `+00:00`, never `Z`.
func pythonISOFormat(t time.Time) string {
	t = t.Truncate(time.Microsecond)
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05-07:00")
	}
	return t.Format("2006-01-02T15:04:05.000000-07:00")
}

// setSpec persists a request timing as the API does: kind to spec_kind, the
// kind's fields to spec, and timezone (default UTC) beside them. The body is
// decoded strictly into the generated variant its discriminator names.
func (s *fakeSchedule) setSpec(raw json.RawMessage) error {
	var u client.ScheduleUpdateInput_Schedule
	if err := u.UnmarshalJSON(raw); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	kind, err := u.Discriminator()
	if err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	var tz *string
	switch client.ScheduleSpecKind(kind) {
	case client.ScheduleSpecKindCron:
		var v client.CronScheduleSpec
		if err := strictDecode(raw, &v); err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
		s.spec, tz = map[string]any{"cron": v.Cron}, v.Timezone
	case client.ScheduleSpecKindInterval:
		var v client.IntervalScheduleSpec
		if err := strictDecode(raw, &v); err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
		if v.IntervalSeconds <= 0 {
			return fmt.Errorf("interval_seconds must be > 0")
		}
		s.spec, tz = map[string]any{"interval_seconds": v.IntervalSeconds}, v.Timezone
		if v.Anchor != nil {
			s.spec["anchor"] = pythonISOFormat(*v.Anchor)
		}
	case client.ScheduleSpecKindRrule:
		var v client.RRuleScheduleSpec
		if err := strictDecode(raw, &v); err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
		s.spec, tz = map[string]any{"rrule": v.Rrule}, v.Timezone
	default:
		return fmt.Errorf("unknown schedule kind %q", kind)
	}
	s.specKind = client.ScheduleSpecKind(kind)
	s.timezone = "UTC"
	if tz != nil {
		s.timezone = *tz
	}
	return nil
}

func strictDecode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// normalizeFakeAction decodes a request action strictly into the generated
// variant its discriminator names, and returns it as the API echoes it, with
// every default filled in.
func normalizeFakeAction(raw json.RawMessage) (client.ScheduleResponse_Action, string, error) {
	var out client.ScheduleResponse_Action
	var u client.ScheduleUpdateInput_Action
	if err := u.UnmarshalJSON(raw); err != nil {
		return out, "", err
	}
	kind, err := u.Discriminator()
	if err != nil {
		return out, "", err
	}
	switch kind {
	case client.ScheduleActionKindPrompt:
		var p client.PromptScheduleAction
		if err := strictDecode(raw, &p); err != nil {
			return out, "", err
		}
		return out, kind, out.FromPromptScheduleAction(p)
	case client.ScheduleActionKindCommand:
		var c client.CommandScheduleAction
		if err := strictDecode(raw, &c); err != nil {
			return out, "", err
		}
		fillCommandDefaults(&c)
		return out, kind, out.FromCommandScheduleAction(c)
	default:
		return out, "", fmt.Errorf("unknown action kind %q", kind)
	}
}

func fillCommandDefaults(c *client.CommandScheduleAction) {
	if c.SecretEnv == nil {
		c.SecretEnv = &[]client.SecretEnvBinding{}
	}
	if c.OverridableEnv == nil {
		c.OverridableEnv = &[]string{}
	}
	if c.OverlapPolicy == nil {
		p := client.ScheduleOverlapPolicySkip
		c.OverlapPolicy = &p
	}
	if c.ExitCodes == nil {
		c.ExitCodes = &map[string]client.CommandScheduleActionExitCodes{"0": "success", "*": "failure"}
	}
	if c.ExitCodeLabels == nil {
		c.ExitCodeLabels = &map[string]string{}
	}
	if c.OnOutcome == nil {
		c.OnOutcome = &map[string]client.OutcomeRouting{}
	}
	fillActions := func(wake **client.WakeBotAction, notify **client.NotifyHumansAction, pause **bool) {
		if *wake != nil && (*wake).IncludeLogTail == nil {
			zero := 0
			(*wake).IncludeLogTail = &zero
		}
		if *notify != nil {
			if (*notify).Recipients == nil {
				(*notify).Recipients = &[]string{}
			}
			if (*notify).Mode == nil {
				m := client.ScheduleNotifyModeOnChange
				(*notify).Mode = &m
			}
		}
		if *pause == nil {
			f := false
			*pause = &f
		}
	}
	for k, r := range *c.OnOutcome {
		fillActions(&r.WakeBot, &r.NotifyHumans, &r.Pause)
		if r.Retry != nil && r.Retry.BackoffSeconds == nil {
			zero := 0
			r.Retry.BackoffSeconds = &zero
		}
		if r.Then != nil {
			fillActions(&r.Then.WakeBot, &r.Then.NotifyHumans, &r.Then.Pause)
		}
		(*c.OnOutcome)[k] = r
	}
}

// render builds the generated ScheduleResponse for a schedule. Like the API,
// it fills the deprecated `prompt` mirror for a prompt action and derives
// next_fire_at, which is null while paused.
func (a *fakeScheduleAPI) render(s *fakeSchedule) json.RawMessage {
	created := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	resp := client.ScheduleResponse{
		ScheduleId: s.id, OrgId: "org-1", BotId: "bot-1", ConversationId: s.conversationID,
		Title: s.title, Action: s.action, SpecKind: s.specKind, Spec: s.spec, Timezone: s.timezone,
		Enabled: s.enabled, SkipIfBusy: s.skipBusy, CatchupPolicy: client.ScheduleCatchupPolicy(s.catchup),
		MaxConsecutiveFailures: s.maxFailures, CreatedAt: created, UpdatedAt: created,
	}
	if s.actionKind == client.ScheduleActionKindPrompt {
		p, err := s.action.AsPromptScheduleAction()
		if err != nil {
			a.t.Fatalf("render: %v", err)
		}
		resp.Prompt = &p.Prompt
	}
	if s.enabled {
		next := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
		resp.NextFireAt = &next
	}
	out, err := json.Marshal(resp)
	if err != nil {
		a.t.Fatalf("render: %v", err)
	}
	return out
}

// wantPatchKeys checks the key set of every PATCH so far, in order.
func (a *fakeScheduleAPI) wantPatchKeys(want ...[]string) func(*terraform.State) error {
	return func(*terraform.State) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		got := make([][]string, 0, len(a.patches))
		for _, p := range a.patches {
			keys := make([]string, 0, len(p))
			for k := range p {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			got = append(got, keys)
		}
		if len(want) == 0 {
			want = [][]string{}
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("PATCH keys = %v, want %v", got, want)
		}
		return nil
	}
}

// lastPatch returns the raw value of a key in the most recent PATCH.
func (a *fakeScheduleAPI) lastPatch(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.patches) == 0 {
		return "<no patch>"
	}
	return string(a.patches[len(a.patches)-1][key])
}

func scheduleConfig(endpoint, body string) string {
	return fmt.Sprintf(`
provider "botyard" {
  endpoint = %q
  api_key  = "byk_test"
  org_id   = "org-1"
}

resource "botyard_bot_schedule" "s" {
  bot_slug = %q
%s
}
`, endpoint, fakeScheduleBot, body)
}
