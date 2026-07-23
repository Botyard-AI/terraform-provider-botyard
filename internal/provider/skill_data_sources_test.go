package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

func skillPageJSON(items string, hasMore bool) string {
	return fmt.Sprintf(`{"items":[%s],"total":3,"limit":100,"offset":0,"has_more":%t}`, items, hasMore)
}

const skillA = `{"id":"sk-a","slug":"alpha","name":"Alpha","summary":"first","scope":"org","provider":"custom","file_count":2,"created_at":"2026-07-20T10:00:00Z","updated_at":"2026-07-20T11:00:00Z"}`
const skillB = `{"id":"sk-b","slug":"beta","name":"Beta","summary":"second","scope":"platform","provider":"anthropic","file_count":0,"created_at":"2026-07-20T10:00:00Z","updated_at":"2026-07-20T11:00:00Z"}`
const skillC = `{"id":"sk-c","slug":"gamma","name":"Gamma","summary":"third","scope":"org","provider":"custom","file_count":5,"created_at":"2026-07-20T10:00:00Z","updated_at":"2026-07-20T11:00:00Z"}`

// TestListSkills_Pagination proves listSkills walks every page: page 1 reports
// has_more=true, page 2 (offset>0) closes it out.
func TestListSkills_Pagination(t *testing.T) {
	var offsets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		off := r.URL.Query().Get("offset")
		offsets = append(offsets, off)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if off == "0" {
			_, _ = w.Write([]byte(skillPageJSON(skillA+","+skillB, true)))
			return
		}
		_, _ = w.Write([]byte(skillPageJSON(skillC, false)))
	}))
	defer srv.Close()

	c, err := client.NewClientWithResponses(srv.URL, client.WithRequestEditorFn(bearerAuth("k")))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	data := &providerData{client: c, orgID: "org-1"}

	var diags diag.Diagnostics
	skills, ok := listSkills(context.Background(), data, &diags)
	if !ok || diags.HasError() {
		t.Fatalf("listSkills failed: ok=%v diags=%v", ok, diags)
	}
	if len(skills) != 3 {
		t.Fatalf("got %d skills across pages, want 3", len(skills))
	}
	if len(offsets) != 2 || offsets[0] != "0" || offsets[1] != "2" {
		t.Fatalf("offsets requested = %v, want [0 2]", offsets)
	}
	// Mapping spot-check on the plural element model.
	m := skillToModel(skills[0])
	if m.ID.ValueString() != "sk-a" || m.Slug.ValueString() != "alpha" || m.Provider.ValueString() != "custom" {
		t.Errorf("m id/slug/provider = %q/%q/%q", m.ID.ValueString(), m.Slug.ValueString(), m.Provider.ValueString())
	}
	if m.Scope.ValueString() != "org" || m.FileCount.ValueInt64() != 2 {
		t.Errorf("m scope/file_count = %q/%d", m.Scope.ValueString(), m.FileCount.ValueInt64())
	}
}

// TestListSkills_EmptyFirstPageStops guards the defensive break: has_more=true
// but an empty items page must not loop forever.
func TestListSkills_EmptyFirstPageStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(skillPageJSON("", true)))
	}))
	defer srv.Close()

	c, err := client.NewClientWithResponses(srv.URL, client.WithRequestEditorFn(bearerAuth("k")))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	data := &providerData{client: c, orgID: "org-1"}

	var diags diag.Diagnostics
	skills, ok := listSkills(context.Background(), data, &diags)
	if !ok || diags.HasError() {
		t.Fatalf("listSkills failed: ok=%v diags=%v", ok, diags)
	}
	if len(skills) != 0 {
		t.Fatalf("got %d skills, want 0", len(skills))
	}
}
