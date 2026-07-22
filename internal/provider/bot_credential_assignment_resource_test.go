package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// credServer is a stateful fake of the bot-credentials API. PUT performs a
// per-scope replace (wipes every scope named in `scopes`, then inserts the
// matching entries), GET returns all current links, and DELETE /{credential_id}
// removes every link for that credential and answers 204 No Content with a JSON
// content-type (the empty-204 shape the delete wrapper must tolerate). It counts
// writes so a converged apply can be asserted to issue exactly one PUT.
type credServer struct {
	links   []credentialEntry
	puts    int
	deletes int
}

func newCredServer(initial ...credentialEntry) *credServer {
	return &credServer{links: append([]credentialEntry(nil), initial...)}
}

func ent(id, scope string, ordinal int, defaultModel ...string) credentialEntry {
	e := credentialEntry{CredentialID: id, Scope: scope, Ordinal: ordinal}
	if len(defaultModel) > 0 {
		v := defaultModel[0]
		e.DefaultModel = &v
	}
	return e
}

func (s *credServer) list() []client.BotCredentialAssignmentResponse {
	out := make([]client.BotCredentialAssignmentResponse, 0, len(s.links))
	for _, l := range s.links {
		out = append(out, client.BotCredentialAssignmentResponse{
			CredentialId: l.CredentialID,
			Scope:        client.CredentialScope(l.Scope),
			Ordinal:      l.Ordinal,
			Label:        "label-" + l.CredentialID,
			Slug:         "slug-" + l.CredentialID,
			Provider:     client.CredentialProvider("anthropic"),
			Enabled:      true,
			DefaultModel: copyStrPtr(l.DefaultModel),
		})
	}
	return out
}

// current returns the stored links sorted by (scope, ordinal, credential_id).
func (s *credServer) current() []credentialEntry {
	out := append([]credentialEntry(nil), s.links...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		if out[i].Ordinal != out[j].Ordinal {
			return out[i].Ordinal < out[j].Ordinal
		}
		return out[i].CredentialID < out[j].CredentialID
	})
	return out
}

func (s *credServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	const base = "/v1/orgs/org-1/bots/bot-1/credentials"
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == base && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(s.list())
		case r.URL.Path == base && r.Method == http.MethodPut:
			s.puts++
			var body client.BotCredentialAssign
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode PUT body: %v", err)
			}
			if body.Scopes == nil {
				t.Errorf("PUT body missing scopes; resource must always send them")
			}
			s.applyPut(t, body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			// Mirror the real route: echo back the entries that were sent.
			_ = json.NewEncoder(w).Encode(assignEntriesToResponses(body.Credentials))
		case strings.HasPrefix(r.URL.Path, base+"/") && r.Method == http.MethodDelete:
			s.deletes++
			cid := strings.TrimPrefix(r.URL.Path, base+"/")
			kept := s.links[:0:0]
			for _, l := range s.links {
				if l.CredentialID != cid {
					kept = append(kept, l)
				}
			}
			s.links = kept
			// 204 No Content, but served with a JSON content-type + empty body.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// applyPut performs the per-scope replace: every scope in body.Scopes is wiped,
// then the matching entries are inserted. Entries whose scope is not in
// body.Scopes are rejected (mirrors the API's stray-scope 422 as a test error).
func (s *credServer) applyPut(t *testing.T, body client.BotCredentialAssign) {
	t.Helper()
	replace := map[string]struct{}{}
	for _, sc := range *body.Scopes {
		replace[string(sc)] = struct{}{}
	}
	kept := s.links[:0:0]
	for _, l := range s.links {
		if _, ok := replace[l.Scope]; !ok {
			kept = append(kept, l)
		}
	}
	s.links = kept
	seen := map[string]struct{}{}
	for _, e := range body.Credentials {
		if _, ok := replace[string(e.Scope)]; !ok {
			t.Errorf("PUT entry scope %q not listed in scopes", e.Scope)
		}
		key := fmt.Sprintf("%s|%d", e.Scope, e.Ordinal)
		if _, dup := seen[key]; dup {
			t.Errorf("PUT contains duplicate (scope, ordinal) %s", key)
		}
		seen[key] = struct{}{}
		s.links = append(s.links, credentialEntry{
			CredentialID: e.CredentialId,
			Scope:        string(e.Scope),
			Ordinal:      e.Ordinal,
			DefaultModel: copyStrPtr(e.DefaultModel),
		})
	}
}

func assignEntriesToResponses(entries []client.BotCredentialAssignEntry) []client.BotCredentialAssignmentResponse {
	out := make([]client.BotCredentialAssignmentResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, client.BotCredentialAssignmentResponse{
			CredentialId: e.CredentialId,
			Scope:        e.Scope,
			Ordinal:      e.Ordinal,
			Label:        "label-" + e.CredentialId,
			Slug:         "slug-" + e.CredentialId,
			Provider:     client.CredentialProvider("anthropic"),
			Enabled:      true,
			DefaultModel: copyStrPtr(e.DefaultModel),
		})
	}
	return out
}

func credResource(t *testing.T, s *credServer) *BotCredentialAssignmentResource {
	t.Helper()
	srv := httptest.NewServer(s.handler(t))
	t.Cleanup(srv.Close)
	c, err := client.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return &BotCredentialAssignmentResource{data: &providerData{client: c, orgID: "org-1"}}
}

// entryKeys renders entries as canonical, comparable keys (including
// default_model) so two entry slices can be compared without cross-indexing.
func entryKeys(entries []credentialEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		dm := "<nil>"
		if e.DefaultModel != nil {
			dm = *e.DefaultModel
		}
		out = append(out, fmt.Sprintf("%s|%s|%d|%s", e.Scope, e.CredentialID, e.Ordinal, dm))
	}
	return out
}

// equalEntries compares two entry slices as sets (order-independent), including
// default_model.
func equalEntries(a, b []credentialEntry) bool {
	ka, kb := entryKeys(a), entryKeys(b)
	sort.Strings(ka)
	sort.Strings(kb)
	return equalStrs(ka, kb)
}

func TestApplyCredentialAssignment(t *testing.T) {
	ctx := context.Background()

	t.Run("create from empty across scopes", func(t *testing.T) {
		s := newCredServer()
		r := credResource(t, s)
		desired := []credentialEntry{
			ent("a", "llm", 0, "claude-opus-4-8"),
			ent("b", "llm", 1),
			ent("c", "web_search", 0),
		}
		var diags diag.Diagnostics
		final := applyCredentialAssignment(ctx, r.data.client, "org-1", "bot-1", desired, distinctScopes(desired), &diags)
		if diags.HasError() {
			t.Fatalf("diags: %v", diags)
		}
		if !equalEntries(final, desired) {
			t.Errorf("final = %+v, want %+v", final, desired)
		}
		if !equalEntries(s.current(), desired) {
			t.Errorf("server = %+v, want %+v", s.current(), desired)
		}
		if s.puts != 1 {
			t.Errorf("puts = %d, want 1", s.puts)
		}
	})

	t.Run("replace is exclusive within a managed scope only", func(t *testing.T) {
		// llm has a@0 plus out-of-band x@1; integration has z@0 (unmanaged).
		s := newCredServer(ent("a", "llm", 0), ent("x", "llm", 1), ent("z", "integration", 0))
		r := credResource(t, s)
		desired := []credentialEntry{ent("a", "llm", 0), ent("b", "llm", 1)}
		var diags diag.Diagnostics
		final := applyCredentialAssignment(ctx, r.data.client, "org-1", "bot-1", desired, distinctScopes(desired), &diags)
		if diags.HasError() {
			t.Fatalf("diags: %v", diags)
		}
		// Returned managed set = llm only, x replaced away.
		if !equalEntries(final, desired) {
			t.Errorf("final = %+v, want %+v", final, desired)
		}
		// Server: llm converged; integration z@0 left untouched.
		// current() sorts by (scope, ordinal, id): "integration" < "llm".
		want := []credentialEntry{ent("z", "integration", 0), ent("a", "llm", 0), ent("b", "llm", 1)}
		if !equalEntries(s.current(), want) {
			t.Errorf("server = %+v, want %+v (integration untouched)", s.current(), want)
		}
	})

	t.Run("update reorder + swap + default_model change within a scope", func(t *testing.T) {
		s := newCredServer(ent("a", "llm", 0), ent("b", "llm", 1))
		r := credResource(t, s)
		// b becomes primary with a model preference; a demoted.
		desired := []credentialEntry{ent("b", "llm", 0, "claude-opus-4-8"), ent("a", "llm", 1)}
		var diags diag.Diagnostics
		final := applyCredentialAssignment(ctx, r.data.client, "org-1", "bot-1", desired, distinctScopes(desired), &diags)
		if diags.HasError() {
			t.Fatalf("diags: %v", diags)
		}
		want := []credentialEntry{ent("b", "llm", 0, "claude-opus-4-8"), ent("a", "llm", 1)}
		if !equalEntries(final, want) {
			t.Errorf("final = %+v, want %+v", final, want)
		}
	})

	t.Run("update dropping a scope clears it via the union", func(t *testing.T) {
		s := newCredServer(ent("a", "llm", 0), ent("c", "web_search", 0))
		r := credResource(t, s)
		desired := []credentialEntry{ent("a", "llm", 0)} // web_search dropped
		prior := []credentialEntry{ent("a", "llm", 0), ent("c", "web_search", 0)}
		scopes := unionScopes(distinctScopes(desired), distinctScopes(prior))
		var diags diag.Diagnostics
		final := applyCredentialAssignment(ctx, r.data.client, "org-1", "bot-1", desired, scopes, &diags)
		if diags.HasError() {
			t.Fatalf("diags: %v", diags)
		}
		if !equalEntries(final, desired) {
			t.Errorf("final = %+v, want %+v", final, desired)
		}
		if !equalEntries(s.current(), desired) {
			t.Errorf("server = %+v, want just llm (web_search cleared)", s.current())
		}
	})

	t.Run("empty desired with no scopes is a no-op", func(t *testing.T) {
		s := newCredServer(ent("z", "integration", 0)) // out-of-band, unmanaged
		r := credResource(t, s)
		var diags diag.Diagnostics
		final := applyCredentialAssignment(ctx, r.data.client, "org-1", "bot-1", nil, nil, &diags)
		if diags.HasError() {
			t.Fatalf("diags: %v", diags)
		}
		if len(final) != 0 {
			t.Errorf("final = %+v, want empty", final)
		}
		if s.puts != 0 {
			t.Errorf("puts = %d, want 0 (nothing to replace)", s.puts)
		}
		if len(s.current()) != 1 {
			t.Errorf("server unmanaged link should survive, got %+v", s.current())
		}
	})
}

func TestBotCredentialCreateReadDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("Read refreshes only managed scopes", func(t *testing.T) {
		// Server has a managed llm scope plus an unmanaged integration cred.
		s := newCredServer(ent("a", "llm", 0), ent("z", "integration", 0))
		r := credResource(t, s)
		prior := []credentialEntry{ent("a", "llm", 0)}
		all, status, _, err := listBotCredentials(ctx, r.data.client, "org-1", "bot-1")
		if err != nil || status != 200 {
			t.Fatalf("list: status=%d err=%v", status, err)
		}
		got := filterByScopes(all, distinctScopes(prior))
		if !equalEntries(got, prior) {
			t.Errorf("read managed = %+v, want %+v (integration ignored)", got, prior)
		}
	})

	t.Run("Delete unassigns each distinct credential", func(t *testing.T) {
		s := newCredServer(ent("a", "llm", 0), ent("b", "llm", 1), ent("c", "web_search", 0))
		r := credResource(t, s)
		state := []credentialEntry{ent("a", "llm", 0), ent("b", "llm", 1), ent("c", "web_search", 0)}
		for _, cid := range distinctCredentialIDs(state) {
			status, _, err := r.data.client.UnassignBotCredential(ctx, "org-1", "bot-1", cid)
			if err != nil {
				t.Fatalf("unassign %s: %v", cid, err)
			}
			switch status {
			case 200, 202, 204, 404:
			default:
				t.Errorf("unassign %s returned %d", cid, status)
			}
		}
		if len(s.current()) != 0 {
			t.Errorf("server = %+v, want empty after delete", s.current())
		}
		if s.deletes != 3 {
			t.Errorf("deletes = %d, want 3", s.deletes)
		}
	})
}

func TestCredentialScopeHelpers(t *testing.T) {
	entries := []credentialEntry{
		ent("a", "llm", 0), ent("b", "llm", 1), ent("c", "web_search", 0), ent("a", "llm", 0),
	}
	if got, want := distinctScopes(entries), []string{"llm", "web_search"}; !equalStrs(got, want) {
		t.Errorf("distinctScopes = %v, want %v", got, want)
	}
	if got, want := distinctCredentialIDs(entries), []string{"a", "b", "c"}; !equalStrs(got, want) {
		t.Errorf("distinctCredentialIDs = %v, want %v", got, want)
	}
	if got, want := unionScopes([]string{"llm"}, []string{"web_search", "llm"}), []string{"llm", "web_search"}; !equalStrs(got, want) {
		t.Errorf("unionScopes = %v, want %v", got, want)
	}
	filtered := filterByScopes(entries, []string{"web_search"})
	if len(filtered) != 1 || filtered[0].CredentialID != "c" {
		t.Errorf("filterByScopes = %+v, want [c@web_search]", filtered)
	}
}

func TestParseCredentialImportID(t *testing.T) {
	tests := []struct {
		id      string
		slug    string
		scopes  []string
		wantErr bool
	}{
		{"my-bot", "my-bot", nil, false},
		{"my-bot:llm", "my-bot", []string{"llm"}, false},
		{"my-bot:llm,web_search", "my-bot", []string{"llm", "web_search"}, false},
		{" my-bot : llm , integration ", "my-bot", []string{"llm", "integration"}, false},
		{"my-bot:bogus", "", nil, true},
		{"my-bot:llm,bogus", "", nil, true},
		{"", "", nil, true},
		{":llm", "", nil, true},
		{"my-bot:", "", nil, true},
		{"my-bot:,", "", nil, true},
	}
	for _, tc := range tests {
		slug, scopes, err := parseCredentialImportID(tc.id)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseCredentialImportID(%q) = (%q,%v,nil), want error", tc.id, slug, scopes)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCredentialImportID(%q) error: %v", tc.id, err)
			continue
		}
		if slug != tc.slug || !equalStrs(scopes, tc.scopes) {
			t.Errorf("parseCredentialImportID(%q) = (%q,%v), want (%q,%v)", tc.id, slug, scopes, tc.slug, tc.scopes)
		}
	}
}

// importedCredentials imports with the given ID against s and returns the
// managed credentials placed into state (or the import diagnostics).
func importedCredentials(t *testing.T, s *credServer, id string) ([]credentialEntry, diag.Diagnostics) {
	t.Helper()
	ctx := context.Background()
	r := credResource(t, s)
	var sresp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sresp)
	importResp := resource.ImportStateResponse{State: tfsdk.State{Schema: sresp.Schema}}
	r.ImportState(ctx, resource.ImportStateRequest{ID: id}, &importResp)
	if importResp.Diagnostics.HasError() {
		return nil, importResp.Diagnostics
	}
	var got BotCredentialAssignmentResourceModel
	importResp.Diagnostics.Append(importResp.State.Get(ctx, &got)...)
	if importResp.Diagnostics.HasError() {
		t.Fatalf("import state.Get: %v", importResp.Diagnostics)
	}
	if got.BotSlug.ValueString() != "bot-1" || got.ID.ValueString() != "bot-1" {
		t.Errorf("import slug/id = %q/%q, want bot-1", got.BotSlug.ValueString(), got.ID.ValueString())
	}
	var d diag.Diagnostics
	return entriesFromSet(ctx, got.Credentials, &d), d
}

func TestImportStateScopeAware(t *testing.T) {
	// Bot has assignments across three scopes.
	seed := func() *credServer {
		return newCredServer(
			ent("a", "llm", 0, "claude-opus-4-8"), ent("b", "llm", 1),
			ent("c", "web_search", 0), ent("z", "integration", 0),
		)
	}

	t.Run("bare slug imports all scopes", func(t *testing.T) {
		got, d := importedCredentials(t, seed(), "bot-1")
		if d.HasError() {
			t.Fatalf("import diags: %v", d)
		}
		want := []credentialEntry{
			ent("a", "llm", 0, "claude-opus-4-8"), ent("b", "llm", 1),
			ent("c", "web_search", 0), ent("z", "integration", 0),
		}
		if !equalEntries(got, want) {
			t.Errorf("import(all) = %+v, want %+v", got, want)
		}
	})

	t.Run("explicit scopes import only those (fixes the drift-away bug)", func(t *testing.T) {
		got, d := importedCredentials(t, seed(), "bot-1:llm")
		if d.HasError() {
			t.Fatalf("import diags: %v", d)
		}
		want := []credentialEntry{ent("a", "llm", 0, "claude-opus-4-8"), ent("b", "llm", 1)}
		if !equalEntries(got, want) {
			t.Errorf("import(llm) = %+v, want %+v (only llm, not filtered away)", got, want)
		}
		// Post-import Read must be a no-op: Read derives managed scopes from the
		// imported state, so it reconstructs exactly the same set.
		all, status, _, err := listBotCredentials(context.Background(), credResource(t, seed()).data.client, "org-1", "bot-1")
		if err != nil || status != 200 {
			t.Fatalf("readback list: status=%d err=%v", status, err)
		}
		if reread := filterByScopes(all, distinctScopes(got)); !equalEntries(reread, want) {
			t.Errorf("post-import Read = %+v, want %+v (no drift)", reread, want)
		}
	})

	t.Run("multiple explicit scopes", func(t *testing.T) {
		got, d := importedCredentials(t, seed(), "bot-1:llm,web_search")
		if d.HasError() {
			t.Fatalf("import diags: %v", d)
		}
		want := []credentialEntry{ent("a", "llm", 0, "claude-opus-4-8"), ent("b", "llm", 1), ent("c", "web_search", 0)}
		if !equalEntries(got, want) {
			t.Errorf("import(llm,web_search) = %+v, want %+v", got, want)
		}
	})

	t.Run("invalid import ID errors", func(t *testing.T) {
		if _, d := importedCredentials(t, seed(), "bot-1:bogus"); !d.HasError() {
			t.Error("import with bogus scope should error")
		}
	})

	t.Run("explicit empty scope is rejected, then out-of-band assignment stays unowned", func(t *testing.T) {
		// Bot has llm assignments but web_search is currently empty.
		s := newCredServer(ent("a", "llm", 0), ent("b", "llm", 1))
		// Importing the empty web_search scope is rejected: an empty scope cannot
		// be represented in state, so we refuse rather than silently drop it.
		if _, d := importedCredentials(t, s, "bot-1:llm,web_search"); !d.HasError() {
			t.Fatal("importing an empty explicit scope should error (unrepresentable)")
		}
		// The user imports only the populated scope instead.
		got, d := importedCredentials(t, s, "bot-1:llm")
		if d.HasError() {
			t.Fatalf("import of populated scope errored: %v", d)
		}
		want := []credentialEntry{ent("a", "llm", 0), ent("b", "llm", 1)}
		if !equalEntries(got, want) {
			t.Errorf("import(llm) = %+v, want %+v", got, want)
		}
		// An out-of-band web_search assignment now appears. Because web_search was
		// never (and could not be) imported, a Read over the managed scopes must
		// ignore it — no phantom ownership. This is the exact edge the R2 review
		// flagged, now consistent with the per-scope model.
		s.links = append(s.links, ent("c", "web_search", 0))
		all, status, _, err := listBotCredentials(context.Background(), credResource(t, s).data.client, "org-1", "bot-1")
		if err != nil || status != 200 {
			t.Fatalf("readback: status=%d err=%v", status, err)
		}
		if reread := filterByScopes(all, distinctScopes(got)); !equalEntries(reread, want) {
			t.Errorf("Read over managed scopes leaked out-of-band web_search: %+v", reread)
		}
	})
}

// credentialSchemaNestedAttrs pulls the scope and ordinal attributes out of the
// resource schema so their configured validators can be exercised directly.
func credentialSchemaNestedAttrs(t *testing.T) (schema.StringAttribute, schema.Int64Attribute) {
	t.Helper()
	var sresp resource.SchemaResponse
	(&BotCredentialAssignmentResource{}).Schema(context.Background(), resource.SchemaRequest{}, &sresp)
	creds, ok := sresp.Schema.Attributes["credentials"].(schema.SetNestedAttribute)
	if !ok {
		t.Fatalf("credentials attribute is not a SetNestedAttribute")
	}
	scopeAttr, ok := creds.NestedObject.Attributes["scope"].(schema.StringAttribute)
	if !ok {
		t.Fatalf("scope attribute is not a StringAttribute")
	}
	ordAttr, ok := creds.NestedObject.Attributes["ordinal"].(schema.Int64Attribute)
	if !ok {
		t.Fatalf("ordinal attribute is not an Int64Attribute")
	}
	return scopeAttr, ordAttr
}

func scopeHasError(t *testing.T, scopeAttr schema.StringAttribute, value string) bool {
	t.Helper()
	resp := &validator.StringResponse{}
	req := validator.StringRequest{Path: path.Root("scope"), ConfigValue: types.StringValue(value)}
	for _, v := range scopeAttr.Validators {
		v.ValidateString(context.Background(), req, resp)
	}
	return resp.Diagnostics.HasError()
}

func ordinalHasError(t *testing.T, ordAttr schema.Int64Attribute, value int64) bool {
	t.Helper()
	resp := &validator.Int64Response{}
	req := validator.Int64Request{Path: path.Root("ordinal"), ConfigValue: types.Int64Value(value)}
	for _, v := range ordAttr.Validators {
		v.ValidateInt64(context.Background(), req, resp)
	}
	return resp.Diagnostics.HasError()
}

// TestCredentialSchemaValidators proves invalid scope/ordinal input is rejected
// at the schema-validation phase — which the framework runs before Create — so
// an invalid config never reaches the client and issues no API request.
func TestCredentialSchemaValidators(t *testing.T) {
	scopeAttr, ordAttr := credentialSchemaNestedAttrs(t)

	for _, good := range credentialScopeValues {
		if scopeHasError(t, scopeAttr, good) {
			t.Errorf("valid scope %q was rejected", good)
		}
	}
	for _, bad := range []string{"bogus", "LLM", "", "web-search"} {
		if !scopeHasError(t, scopeAttr, bad) {
			t.Errorf("invalid scope %q was accepted", bad)
		}
	}

	for _, good := range []int64{0, 1, 5, 100} {
		if ordinalHasError(t, ordAttr, good) {
			t.Errorf("valid ordinal %d was rejected", good)
		}
	}
	for _, bad := range []int64{-1, -10} {
		if !ordinalHasError(t, ordAttr, bad) {
			t.Errorf("invalid ordinal %d was accepted", bad)
		}
	}
}
