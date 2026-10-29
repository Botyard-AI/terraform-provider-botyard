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
//   - an interval anchor is echoed in Python isoformat (`+00:00`, never `Z`);
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
	specKind           string
	spec               json.RawMessage
	timezone           string
	action             json.RawMessage // normalized, as the API echoes it
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
		id: fmt.Sprintf("sch-%d", a.nextID), enabled: true, catchup: "skip", action: action,
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
		next.action = action
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

// setSpec persists a request timing as the API does: kind to spec_kind, the
// kind's fields to spec, and timezone (default UTC) beside them.
func (s *fakeSchedule) setSpec(raw json.RawMessage) error {
	var in struct {
		Kind            string     `json:"kind"`
		Cron            *string    `json:"cron"`
		IntervalSeconds *int       `json:"interval_seconds"`
		Anchor          *time.Time `json:"anchor"`
		Rrule           *string    `json:"rrule"`
		Timezone        *string    `json:"timezone"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	var spec any
	switch in.Kind {
	case "cron":
		spec = map[string]string{"cron": *in.Cron}
	case "interval":
		if *in.IntervalSeconds <= 0 {
			return fmt.Errorf("interval_seconds must be > 0")
		}
		m := map[string]any{"interval_seconds": *in.IntervalSeconds}
		if in.Anchor != nil {
			m["anchor"] = in.Anchor.Format("2006-01-02T15:04:05-07:00") // Python isoformat
		}
		spec = m
	case "rrule":
		spec = map[string]string{"rrule": *in.Rrule}
	default:
		return fmt.Errorf("unknown schedule kind %q", in.Kind)
	}
	s.specKind = in.Kind
	s.spec, _ = json.Marshal(spec)
	s.timezone = "UTC"
	if in.Timezone != nil {
		s.timezone = *in.Timezone
	}
	return nil
}

// normalizeFakeAction validates a request action strictly and returns it as
// the API echoes it, with every default filled in.
func normalizeFakeAction(raw json.RawMessage) (json.RawMessage, string, error) {
	var disc struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &disc); err != nil {
		return nil, "", err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	switch disc.Kind {
	case client.ScheduleActionKindPrompt:
		var p client.PromptScheduleAction
		if err := dec.Decode(&p); err != nil {
			return nil, "", err
		}
		out, err := json.Marshal(p)
		return out, disc.Kind, err
	case client.ScheduleActionKindCommand:
		var c client.CommandScheduleAction
		if err := dec.Decode(&c); err != nil {
			return nil, "", err
		}
		fillCommandDefaults(&c)
		out, err := json.Marshal(c)
		return out, disc.Kind, err
	default:
		return nil, "", fmt.Errorf("unknown action kind %q", disc.Kind)
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

func (a *fakeScheduleAPI) render(s *fakeSchedule) json.RawMessage {
	var prompt *string
	var p client.PromptScheduleAction
	if json.Unmarshal(s.action, &p) == nil && p.Kind == client.PromptScheduleActionKindPrompt {
		prompt = &p.Prompt
	}
	var next *string
	if s.enabled {
		n := "2026-11-02T09:00:00Z"
		next = &n
	}
	out, err := json.Marshal(map[string]any{
		"schedule_id": s.id, "org_id": "org-1", "bot_id": "bot-1", "conversation_id": s.conversationID,
		"title": s.title, "action": s.action, "prompt": prompt, "spec_kind": s.specKind, "spec": s.spec,
		"timezone": s.timezone, "enabled": s.enabled, "skip_if_busy": s.skipBusy,
		"catchup_policy": s.catchup, "max_consecutive_failures": s.maxFailures,
		"created_at": "2026-10-29T10:00:00Z", "updated_at": "2026-10-29T10:00:00Z", "next_fire_at": next,
	})
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
