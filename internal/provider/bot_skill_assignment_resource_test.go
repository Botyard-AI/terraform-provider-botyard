package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

func TestDiffStrings(t *testing.T) {
	cases := map[string]struct {
		desired, current    []string
		wantAdd, wantRemove []string
	}{
		"empty to set":      {nil, nil, nil, nil},
		"add all from zero": {[]string{"a", "b"}, nil, []string{"a", "b"}, nil},
		"remove all":        {nil, []string{"a", "b"}, nil, []string{"a", "b"}},
		"swap one":          {[]string{"a", "c"}, []string{"a", "b"}, []string{"c"}, []string{"b"}},
		"no change":         {[]string{"a", "b"}, []string{"b", "a"}, nil, nil},
		"dedup inputs":      {[]string{"a", "a", "b"}, []string{"b", "b"}, []string{"a"}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gotAdd, gotRemove := diffStrings(tc.desired, tc.current)
			if !equalStrs(gotAdd, tc.wantAdd) {
				t.Errorf("toAdd = %v, want %v", gotAdd, tc.wantAdd)
			}
			if !equalStrs(gotRemove, tc.wantRemove) {
				t.Errorf("toRemove = %v, want %v", gotRemove, tc.wantRemove)
			}
		})
	}
}

func equalStrs(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// skillServer is a stateful fake of the bot-skills API that mirrors the real
// semantics: the PUT is *additive* (skips already-assigned) and the DELETE
// removes exactly the given IDs, returning 204 No Content with a JSON
// content-type (the empty-204 shape the delete wrapper must tolerate). The GET
// returns the full current set. It records how many times each verb was called
// so tests can assert that a converged reconcile issues no writes.
type skillServer struct {
	assigned map[string]struct{}
	puts     int
	deletes  int
}

func newSkillServer(initial ...string) *skillServer {
	s := &skillServer{assigned: map[string]struct{}{}}
	for _, id := range initial {
		s.assigned[id] = struct{}{}
	}
	return s
}

func (s *skillServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/orgs/org-1/bots/bot-1/skills" {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(s.list())
		case http.MethodPut:
			s.puts++
			var body client.BotSkillAssign
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode PUT body: %v", err)
			}
			added := make([]client.BotSkillAssignmentResponse, 0)
			for _, id := range body.SkillIds {
				if _, ok := s.assigned[id]; ok {
					continue // additive: skip already-assigned
				}
				s.assigned[id] = struct{}{}
				added = append(added, client.BotSkillAssignmentResponse{SkillId: id})
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(added)
		case http.MethodDelete:
			s.deletes++
			var body client.BotSkillIds
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode DELETE body: %v", err)
			}
			for _, id := range body.SkillIds {
				delete(s.assigned, id)
			}
			// 204 No Content with a JSON content-type + empty body — the shape
			// that trips the generated parser and exercises the wrapper.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %q", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func (s *skillServer) list() []client.BotSkillAssignmentResponse {
	out := make([]client.BotSkillAssignmentResponse, 0, len(s.assigned))
	for id := range s.assigned {
		out = append(out, client.BotSkillAssignmentResponse{SkillId: id})
	}
	return out
}

func (s *skillServer) currentIDs() []string {
	out := make([]string, 0, len(s.assigned))
	for id := range s.assigned {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func reconcileTestClient(t *testing.T, s *skillServer) *client.ClientWithResponses {
	t.Helper()
	srv := httptest.NewServer(s.handler(t))
	t.Cleanup(srv.Close)
	c, err := client.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return c
}

func TestReconcileBotSkills(t *testing.T) {
	t.Run("assign from empty", func(t *testing.T) {
		s := newSkillServer()
		c := reconcileTestClient(t, s)
		var diags diag.Diagnostics
		final := reconcileBotSkills(context.Background(), c, "org-1", "bot-1", []string{"a", "b"}, &diags)
		if diags.HasError() {
			t.Fatalf("diags: %v", diags)
		}
		if !equalStrs(final, []string{"a", "b"}) {
			t.Errorf("final = %v, want [a b]", final)
		}
		if s.puts != 1 || s.deletes != 0 {
			t.Errorf("puts/deletes = %d/%d, want 1/0", s.puts, s.deletes)
		}
	})

	t.Run("swap converges and is exclusive over out-of-band skills", func(t *testing.T) {
		// Server already has a,b plus x assigned outside Terraform; desired a,c.
		s := newSkillServer("a", "b", "x")
		c := reconcileTestClient(t, s)
		var diags diag.Diagnostics
		final := reconcileBotSkills(context.Background(), c, "org-1", "bot-1", []string{"a", "c"}, &diags)
		if diags.HasError() {
			t.Fatalf("diags: %v", diags)
		}
		if !equalStrs(final, []string{"a", "c"}) {
			t.Errorf("final = %v, want [a c]", final)
		}
		if !equalStrs(s.currentIDs(), []string{"a", "c"}) {
			t.Errorf("server set = %v, want [a c] (b and out-of-band x removed)", s.currentIDs())
		}
		if s.puts != 1 || s.deletes != 1 {
			t.Errorf("puts/deletes = %d/%d, want 1/1", s.puts, s.deletes)
		}
	})

	t.Run("no-op when already converged", func(t *testing.T) {
		s := newSkillServer("a", "b")
		c := reconcileTestClient(t, s)
		var diags diag.Diagnostics
		final := reconcileBotSkills(context.Background(), c, "org-1", "bot-1", []string{"b", "a"}, &diags)
		if diags.HasError() {
			t.Fatalf("diags: %v", diags)
		}
		if !equalStrs(final, []string{"a", "b"}) {
			t.Errorf("final = %v, want [a b]", final)
		}
		if s.puts != 0 || s.deletes != 0 {
			t.Errorf("puts/deletes = %d/%d, want 0/0 (converged reconcile writes nothing)", s.puts, s.deletes)
		}
	})

	t.Run("empty desired removes all", func(t *testing.T) {
		s := newSkillServer("a", "b")
		c := reconcileTestClient(t, s)
		var diags diag.Diagnostics
		final := reconcileBotSkills(context.Background(), c, "org-1", "bot-1", nil, &diags)
		if diags.HasError() {
			t.Fatalf("diags: %v", diags)
		}
		if len(final) != 0 {
			t.Errorf("final = %v, want empty", final)
		}
		if s.puts != 0 || s.deletes != 1 {
			t.Errorf("puts/deletes = %d/%d, want 0/1", s.puts, s.deletes)
		}
	})
}

func TestListBotSkillIDs_BotGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"title":"Not Found","status":404}`))
	}))
	defer srv.Close()
	c, err := client.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	ids, status, _, err := listBotSkillIDs(context.Background(), c, "org-1", "bot-1")
	if err != nil {
		t.Fatalf("listBotSkillIDs err: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if ids != nil {
		t.Errorf("ids = %v, want nil on non-200", ids)
	}
}
