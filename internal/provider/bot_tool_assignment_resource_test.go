package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

func TestToolIDsFrom(t *testing.T) {
	links := []client.BotToolAssignmentResponse{{ToolId: "c"}, {ToolId: "a"}, {ToolId: "b"}}
	got := toolIDsFrom(&links)
	if !equalStrs(got, []string{"a", "b", "c"}) {
		t.Errorf("toolIDsFrom = %v, want sorted [a b c]", got)
	}
	empty := []client.BotToolAssignmentResponse{}
	if got := toolIDsFrom(&empty); len(got) != 0 {
		t.Errorf("toolIDsFrom(empty) = %v, want empty", got)
	}
}

// toolServer is a stateful fake of the bot-tools API: PUT replaces the whole set
// (returning the full resulting list), GET returns the current set, and DELETE
// removes the given IDs returning 204 No Content with a JSON content-type (the
// empty-204 shape the delete wrapper must tolerate). It counts writes so a
// converged apply can be asserted to issue exactly one replace.
type toolServer struct {
	assigned map[string]struct{}
	puts     int
	deletes  int
}

func newToolServer(initial ...string) *toolServer {
	s := &toolServer{assigned: map[string]struct{}{}}
	for _, id := range initial {
		s.assigned[id] = struct{}{}
	}
	return s
}

func (s *toolServer) list() []client.BotToolAssignmentResponse {
	out := make([]client.BotToolAssignmentResponse, 0, len(s.assigned))
	for id := range s.assigned {
		out = append(out, client.BotToolAssignmentResponse{ToolId: id})
	}
	return out
}

func (s *toolServer) currentIDs() []string {
	out := make([]string, 0, len(s.assigned))
	for id := range s.assigned {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (s *toolServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/orgs/org-1/bots/bot-1/tools" {
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
			var body client.BotToolAssignRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode PUT body: %v", err)
			}
			s.assigned = map[string]struct{}{}
			for _, id := range body.ToolIds {
				s.assigned[id] = struct{}{}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(s.list())
		case http.MethodDelete:
			s.deletes++
			var body client.BotToolIds
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode DELETE body: %v", err)
			}
			for _, id := range body.ToolIds {
				delete(s.assigned, id)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected method %q", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func toolResource(t *testing.T, s *toolServer) *BotToolAssignmentResource {
	t.Helper()
	srv := httptest.NewServer(s.handler(t))
	t.Cleanup(srv.Close)
	c, err := client.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return &BotToolAssignmentResource{data: &providerData{client: c, orgID: "org-1"}}
}

func toolModel(t *testing.T, ids []string) BotToolAssignmentResourceModel {
	t.Helper()
	set, d := types.SetValueFrom(context.Background(), types.StringType, ids)
	if d.HasError() {
		t.Fatalf("build set: %v", d)
	}
	return BotToolAssignmentResourceModel{BotSlug: types.StringValue("bot-1"), ToolIDs: set}
}

func modelToolIDs(t *testing.T, m BotToolAssignmentResourceModel) []string {
	t.Helper()
	var diags diag.Diagnostics
	ids := setToStrSlice(context.Background(), m.ToolIDs, &diags)
	if diags.HasError() {
		t.Fatalf("read set: %v", diags)
	}
	sort.Strings(ids)
	return ids
}

func TestBotToolReplace(t *testing.T) {
	t.Run("replace from empty", func(t *testing.T) {
		s := newToolServer()
		r := toolResource(t, s)
		m := toolModel(t, []string{"a", "b"})
		var diags diag.Diagnostics
		r.replace(context.Background(), &m, &diags)
		if diags.HasError() {
			t.Fatalf("diags: %v", diags)
		}
		if !equalStrs(modelToolIDs(t, m), []string{"a", "b"}) {
			t.Errorf("model tool_ids = %v, want [a b]", modelToolIDs(t, m))
		}
		if m.ID.ValueString() != "bot-1" {
			t.Errorf("id = %q, want bot-1", m.ID.ValueString())
		}
		if !equalStrs(s.currentIDs(), []string{"a", "b"}) {
			t.Errorf("server = %v, want [a b]", s.currentIDs())
		}
		if s.puts != 1 {
			t.Errorf("puts = %d, want 1", s.puts)
		}
	})

	t.Run("replace is exclusive over out-of-band tools", func(t *testing.T) {
		// Server has a,b plus x assigned outside Terraform; desired a,c.
		s := newToolServer("a", "b", "x")
		r := toolResource(t, s)
		m := toolModel(t, []string{"a", "c"})
		var diags diag.Diagnostics
		r.replace(context.Background(), &m, &diags)
		if diags.HasError() {
			t.Fatalf("diags: %v", diags)
		}
		if !equalStrs(modelToolIDs(t, m), []string{"a", "c"}) {
			t.Errorf("model tool_ids = %v, want [a c]", modelToolIDs(t, m))
		}
		if !equalStrs(s.currentIDs(), []string{"a", "c"}) {
			t.Errorf("server = %v, want [a c] (b and out-of-band x replaced away)", s.currentIDs())
		}
	})

	t.Run("replace to empty removes all", func(t *testing.T) {
		s := newToolServer("a", "b")
		r := toolResource(t, s)
		m := toolModel(t, []string{})
		var diags diag.Diagnostics
		r.replace(context.Background(), &m, &diags)
		if diags.HasError() {
			t.Fatalf("diags: %v", diags)
		}
		if len(modelToolIDs(t, m)) != 0 {
			t.Errorf("model tool_ids = %v, want empty", modelToolIDs(t, m))
		}
		if len(s.currentIDs()) != 0 {
			t.Errorf("server = %v, want empty", s.currentIDs())
		}
	})
}
