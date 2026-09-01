package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// cannedSkillJSON is a custom skill with two files in sort order.
const cannedSkillJSON = `{
  "id": "sk-1", "slug": "deploy-runbook", "name": "Deploy Runbook",
  "summary": "How to ship a release", "scope": "org", "provider": "custom",
  "created_by_user_id": null, "created_by_bot_id": null,
  "created_by_actor_type": null, "created_by_name": null, "created_by_avatar_url": null,
  "files": [
    {"id": "f-1", "filename": "SKILL.md", "content": "# Deploy\n", "content_hash": "h1", "sort_order": 0,
     "created_at": "2026-07-20T10:00:00Z", "updated_at": "2026-07-20T10:00:00Z"},
    {"id": "f-2", "filename": "references/rollback.md", "content": "roll back\n", "content_hash": "h2", "sort_order": 1,
     "created_at": "2026-07-20T10:00:00Z", "updated_at": "2026-07-20T10:00:00Z"}
  ],
  "created_at": "2026-07-20T10:00:00Z", "updated_at": "2026-07-20T11:30:00Z"
}`

// platformSkillJSON is a platform-provided skill: the API refuses to edit or
// delete it, so the resource must reject it on read/import.
const platformSkillJSON = `{
  "id": "sk-2", "slug": "botyard-github", "name": "Botyard GitHub",
  "summary": "platform skill", "scope": "platform", "provider": "botyard",
  "files": [{"id": "f-9", "filename": "SKILL.md", "content": "x", "content_hash": "h", "sort_order": 0,
     "created_at": "2026-07-20T10:00:00Z", "updated_at": "2026-07-20T10:00:00Z"}],
  "created_at": "2026-07-20T10:00:00Z", "updated_at": "2026-07-20T11:30:00Z"
}`

// skillResourceModel builds a minimal valid model: name/summary/default scope
// and a single SKILL.md file.
func skillResourceModel() SkillResourceModel {
	return SkillResourceModel{
		Name:    types.StringValue("Deploy Runbook"),
		Summary: types.StringValue("How to ship a release"),
		Scope:   types.StringValue("org"),
		Files: []skillFileModel{
			{Filename: types.StringValue("SKILL.md"), Content: types.StringValue("# Deploy\n")},
		},
	}
}

func newSkillClient(t *testing.T, url, apiKey string) *client.ClientWithResponses {
	t.Helper()
	c, err := client.NewClientWithResponses(url, client.WithRequestEditorFn(bearerAuth(apiKey)))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return c
}

func TestValidateSkillConfig(t *testing.T) {
	if d := validateSkillConfig(skillResourceModel()); d.HasError() {
		t.Errorf("valid config errored: %v", d)
	}

	// SKILL.md is mandatory.
	noEntry := skillResourceModel()
	noEntry.Files = []skillFileModel{
		{Filename: types.StringValue("notes.md"), Content: types.StringValue("hi")},
	}
	if !validateSkillConfig(noEntry).HasError() {
		t.Error("files without SKILL.md should error")
	}

	// An empty file list is rejected before the API sees it.
	empty := skillResourceModel()
	empty.Files = []skillFileModel{}
	if !validateSkillConfig(empty).HasError() {
		t.Error("empty files should error")
	}

	// Duplicate filenames.
	dup := skillResourceModel()
	dup.Files = append(dup.Files, skillFileModel{
		Filename: types.StringValue("SKILL.md"), Content: types.StringValue("again"),
	})
	if !validateSkillConfig(dup).HasError() {
		t.Error("duplicate filenames should error")
	}

	// Empty filename.
	blank := skillResourceModel()
	blank.Files = append(blank.Files, skillFileModel{
		Filename: types.StringValue(""), Content: types.StringValue("x"),
	})
	if !validateSkillConfig(blank).HasError() {
		t.Error("empty filename should error")
	}

	// Bad scope.
	scope := skillResourceModel()
	scope.Scope = types.StringValue("global")
	if !validateSkillConfig(scope).HasError() {
		t.Error("invalid scope should error")
	}
	scope.Scope = types.StringValue("member")
	if validateSkillConfig(scope).HasError() {
		t.Error("member scope should be valid")
	}

	// Size limits.
	big := skillResourceModel()
	big.Files[0].Content = types.StringValue(strings.Repeat("a", skillMaxFileBytes+1))
	if !validateSkillConfig(big).HasError() {
		t.Error("oversized file should error")
	}
	total := skillResourceModel()
	total.Files = nil
	for i := 0; i < 6; i++ {
		total.Files = append(total.Files, skillFileModel{
			Filename: types.StringValue([]string{"SKILL.md", "a", "b", "c", "d", "e"}[i]),
			Content:  types.StringValue(strings.Repeat("a", skillMaxFileBytes)),
		})
	}
	if !validateSkillConfig(total).HasError() {
		t.Error("oversized skill should error")
	}
}

// TestValidateSkillConfig_Frontmatter is the perpetual-diff guard: the API
// strips leading YAML frontmatter from SKILL.md before storing it, so a config
// that declares it would never converge.
func TestValidateSkillConfig_Frontmatter(t *testing.T) {
	fm := skillResourceModel()
	fm.Files[0].Content = types.StringValue("---\nname: Deploy\n---\n\n# Deploy\n")
	if !validateSkillConfig(fm).HasError() {
		t.Error("SKILL.md with YAML frontmatter should error")
	}

	// Frontmatter in a non-entrypoint file is untouched by the API, so allowed.
	other := skillResourceModel()
	other.Files = append(other.Files, skillFileModel{
		Filename: types.StringValue("reference.md"),
		Content:  types.StringValue("---\ntitle: x\n---\nbody\n"),
	})
	if validateSkillConfig(other).HasError() {
		t.Error("frontmatter outside SKILL.md should be allowed")
	}

	// A horizontal rule mid-document is not frontmatter.
	hr := skillResourceModel()
	hr.Files[0].Content = types.StringValue("# Deploy\n\n---\n\nnotes\n")
	if validateSkillConfig(hr).HasError() {
		t.Error("mid-document --- should not be treated as frontmatter")
	}
}

// TestValidateSkillConfig_UnknownValues proves unknown (not-yet-computed)
// values are skipped rather than reported as violations at plan time.
func TestValidateSkillConfig_UnknownValues(t *testing.T) {
	unknownName := skillResourceModel()
	unknownName.Files = []skillFileModel{
		{Filename: types.StringUnknown(), Content: types.StringValue("x")},
	}
	if validateSkillConfig(unknownName).HasError() {
		t.Error("unknown filename should not trigger the missing-SKILL.md error")
	}

	unknownContent := skillResourceModel()
	unknownContent.Files[0].Content = types.StringUnknown()
	if validateSkillConfig(unknownContent).HasError() {
		t.Error("unknown content should be skipped")
	}
}

// TestBuildSkillUpdateBody_Sparse is the load-bearing test for the PATCH shape:
// the API gates `scope` on presence (only the creator may send it) and rewrites
// every file row when `files` is present, so unchanged fields must be omitted.
func TestBuildSkillUpdateBody_Sparse(t *testing.T) {
	state := skillResourceModel()

	// Nothing changed -> empty body.
	body, err := buildSkillUpdateBody(state, state)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if string(body) != "{}" {
		t.Errorf("no-op update body = %s, want {}", body)
	}

	// Summary only.
	plan := skillResourceModel()
	plan.Summary = types.StringValue("updated summary")
	body, err = buildSkillUpdateBody(plan, state)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sent := decodeObj(t, body)
	if jsonStr(t, sent["summary"]) != "updated summary" {
		t.Errorf("summary = %s", sent["summary"])
	}
	for _, k := range []string{"name", "scope", "files"} {
		if _, ok := sent[k]; ok {
			t.Errorf("unchanged field %q must be omitted from the PATCH body", k)
		}
	}

	// Rename sends name but still no scope (the slug stays put server-side).
	plan = skillResourceModel()
	plan.Name = types.StringValue("Release Runbook")
	sent = decodeObj(t, mustBuildSkillUpdate(t, plan, state))
	if jsonStr(t, sent["name"]) != "Release Runbook" {
		t.Errorf("name = %s", sent["name"])
	}
	if _, ok := sent["scope"]; ok {
		t.Error("rename must not send scope")
	}

	// Scope change is the only case that sends scope.
	plan = skillResourceModel()
	plan.Scope = types.StringValue("member")
	sent = decodeObj(t, mustBuildSkillUpdate(t, plan, state))
	if jsonStr(t, sent["scope"]) != "member" {
		t.Errorf("scope = %s", sent["scope"])
	}
}

func mustBuildSkillUpdate(t *testing.T, plan, state SkillResourceModel) []byte {
	t.Helper()
	body, err := buildSkillUpdateBody(plan, state)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return body
}

// TestBuildSkillUpdateBody_FileReplacement proves any file edit sends the whole
// ordered list (the API replaces the file set wholesale) and that reordering
// alone counts as a change, since order is persisted as sort_order.
func TestBuildSkillUpdateBody_FileReplacement(t *testing.T) {
	state := skillResourceModel()
	state.Files = append(state.Files, skillFileModel{
		Filename: types.StringValue("a.md"), Content: types.StringValue("A"),
	})

	// Content edit.
	plan := skillResourceModel()
	plan.Files = []skillFileModel{
		{Filename: types.StringValue("SKILL.md"), Content: types.StringValue("# Deploy v2\n")},
		{Filename: types.StringValue("a.md"), Content: types.StringValue("A")},
	}
	sent := decodeObj(t, mustBuildSkillUpdate(t, plan, state))
	files, ok := sent["files"]
	if !ok {
		t.Fatal("files must be sent when a file changes")
	}
	if !strings.Contains(string(files), "# Deploy v2") || !strings.Contains(string(files), "a.md") {
		t.Errorf("files payload should carry the full replacement set, got %s", files)
	}

	// Dropping a file sends the shorter list (that is how deletion happens).
	plan = skillResourceModel()
	sent = decodeObj(t, mustBuildSkillUpdate(t, plan, state))
	if strings.Contains(string(sent["files"]), "a.md") {
		t.Errorf("dropped file should be absent from the replacement set, got %s", sent["files"])
	}

	// Reorder only.
	plan = skillResourceModel()
	plan.Files = []skillFileModel{
		{Filename: types.StringValue("a.md"), Content: types.StringValue("A")},
		{Filename: types.StringValue("SKILL.md"), Content: types.StringValue("# Deploy\n")},
	}
	if _, ok := decodeObj(t, mustBuildSkillUpdate(t, plan, state))["files"]; !ok {
		t.Error("reordering files must send the files list (order is sort_order)")
	}
}

func TestSkillFilesEqual(t *testing.T) {
	a := []skillFileModel{
		{Filename: types.StringValue("SKILL.md"), Content: types.StringValue("x")},
	}
	if !skillFilesEqual(a, a) {
		t.Error("identical lists should compare equal")
	}
	if skillFilesEqual(a, nil) {
		t.Error("different lengths should compare unequal")
	}
	b := []skillFileModel{
		{Filename: types.StringValue("SKILL.md"), Content: types.StringValue("y")},
	}
	if skillFilesEqual(a, b) {
		t.Error("differing content should compare unequal")
	}
}

func TestMapSkillResource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedSkillJSON))
	}))
	defer srv.Close()

	c := newSkillClient(t, srv.URL, "byk_test")
	resp, err := c.GetSkillV1OrgsOrgIdSkillsSkillSlugGetWithResponse(context.Background(), "org-1", "deploy-runbook")
	if err != nil {
		t.Fatalf("GetSkill...: %v", err)
	}
	if resp.JSON200 == nil {
		t.Fatalf("JSON200 nil (status %d, body %q)", resp.StatusCode(), string(resp.Body))
	}

	var m SkillResourceModel
	mapSkillResource(resp.JSON200, &m)
	if m.ID.ValueString() != "sk-1" || m.Slug.ValueString() != "deploy-runbook" {
		t.Errorf("id/slug = %q/%q", m.ID.ValueString(), m.Slug.ValueString())
	}
	if m.Name.ValueString() != "Deploy Runbook" || m.Scope.ValueString() != "org" {
		t.Errorf("name/scope = %q/%q", m.Name.ValueString(), m.Scope.ValueString())
	}
	if m.CreatedAt.ValueString() != "2026-07-20T10:00:00Z" || m.UpdatedAt.ValueString() != "2026-07-20T11:30:00Z" {
		t.Errorf("timestamps = %q/%q", m.CreatedAt.ValueString(), m.UpdatedAt.ValueString())
	}
	if len(m.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(m.Files))
	}
	// Files must keep the server's sort order.
	if m.Files[0].Filename.ValueString() != "SKILL.md" || m.Files[1].Filename.ValueString() != "references/rollback.md" {
		t.Errorf("file order = %q, %q", m.Files[0].Filename.ValueString(), m.Files[1].Filename.ValueString())
	}
	if m.Files[0].Content.ValueString() != "# Deploy\n" {
		t.Errorf("content = %q", m.Files[0].Content.ValueString())
	}
}

// TestSkillResource_CreateRoundTripWithAuth proves the create POST carries the
// bearer token, targets the org-scoped path, and sends exactly the four
// SkillCreate fields with files in declaration order.
func TestSkillResource_CreateRoundTripWithAuth(t *testing.T) {
	const apiKey, orgID = "byk_test_secret", "org-1"

	var gotAuth, gotPath, gotMethod string
	var gotBodyRaw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotBodyRaw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(cannedSkillJSON))
	}))
	defer srv.Close()

	c := newSkillClient(t, srv.URL, apiKey)
	plan := skillResourceModel()
	plan.Files = append(plan.Files, skillFileModel{
		Filename: types.StringValue("references/rollback.md"),
		Content:  types.StringValue("roll back\n"),
	})
	resp, err := c.CreateSkillV1OrgsOrgIdSkillsPostWithResponse(context.Background(), orgID, client.SkillCreate{
		Name:    plan.Name.ValueString(),
		Summary: plan.Summary.ValueString(),
		Scope:   client.SkillScope(plan.Scope.ValueString()),
		Files:   skillFileInputs(plan.Files),
	})
	if err != nil {
		t.Fatalf("CreateSkill...: %v", err)
	}

	if want := "Bearer " + apiKey; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if want := "/v1/orgs/" + orgID + "/skills"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	sent := decodeObj(t, gotBodyRaw)
	if jsonStr(t, sent["name"]) != "Deploy Runbook" || jsonStr(t, sent["scope"]) != "org" {
		t.Errorf("name/scope = %s/%s", sent["name"], sent["scope"])
	}
	// Server-owned identity must never be sent on create.
	for _, k := range []string{"id", "slug", "provider", "created_at", "updated_at"} {
		if _, ok := sent[k]; ok {
			t.Errorf("create body carries server-owned key %q", k)
		}
	}
	idx := strings.Index(string(sent["files"]), "SKILL.md")
	if idx < 0 || idx > strings.Index(string(sent["files"]), "rollback.md") {
		t.Errorf("files must be sent in declaration order, got %s", sent["files"])
	}
	if resp.JSON201 == nil {
		t.Fatalf("JSON201 nil (status %d, body %q)", resp.StatusCode(), string(resp.Body))
	}
}

// TestSkillResource_UpdateRoundTrip proves the PATCH is slug-addressed.
func TestSkillResource_UpdateRoundTrip(t *testing.T) {
	const orgID, slug = "org-1", "deploy-runbook"
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedSkillJSON))
	}))
	defer srv.Close()

	c := newSkillClient(t, srv.URL, "byk_test")
	plan := skillResourceModel()
	plan.Summary = types.StringValue("updated")
	body := mustBuildSkillUpdate(t, plan, skillResourceModel())
	resp, err := c.UpdateSkillV1OrgsOrgIdSkillsSkillSlugPatchWithBodyWithResponse(
		context.Background(), orgID, slug, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("UpdateSkill...: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if want := "/v1/orgs/" + orgID + "/skills/" + slug; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if resp.JSON200 == nil {
		t.Fatalf("JSON200 nil (status %d)", resp.StatusCode())
	}
}

// TestSkillResource_DeleteRoundTrip covers the empty-204-with-JSON-content-type
// case that the generated parser chokes on, which is why DeleteSkill exists.
func TestSkillResource_DeleteRoundTrip(t *testing.T) {
	const orgID, slug = "org-1", "deploy-runbook"
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		accepted bool
	}{
		{"no content", http.StatusNoContent, "", true},
		{"already gone", http.StatusNotFound, `{"detail":"Skill not found"}`, true},
		{"non-custom skill", http.StatusBadRequest, `{"detail":"Cannot delete non-custom skills."}`, false},
		{"server error", http.StatusInternalServerError, `{"detail":"boom"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				// The API serves even an empty 204 with a JSON content-type.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newSkillClient(t, srv.URL, "byk_test")
			status, _, err := c.DeleteSkill(context.Background(), orgID, slug)
			if err != nil {
				t.Fatalf("DeleteSkill: %v", err)
			}
			if gotMethod != http.MethodDelete {
				t.Errorf("method = %q, want DELETE", gotMethod)
			}
			if want := "/v1/orgs/" + orgID + "/skills/" + slug; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}
			if got := skillDeleteStatusAccepted(status); got != tc.accepted {
				t.Errorf("accepted = %v, want %v (status %d)", got, tc.accepted, status)
			}
		})
	}
}

// TestCheckSkillManageable proves a platform-provided skill is rejected rather
// than adopted into state — the API would refuse every subsequent write.
func TestCheckSkillManageable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantError bool
	}{
		{"custom", cannedSkillJSON, false},
		{"platform", platformSkillJSON, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newSkillClient(t, srv.URL, "byk_test")
			resp, err := c.GetSkillV1OrgsOrgIdSkillsSkillSlugGetWithResponse(context.Background(), "org-1", "s")
			if err != nil {
				t.Fatalf("GetSkill...: %v", err)
			}
			if resp.JSON200 == nil {
				t.Fatalf("JSON200 nil (status %d, body %q)", resp.StatusCode(), string(resp.Body))
			}
			if got := checkSkillManageable(resp.JSON200).HasError(); got != tc.wantError {
				t.Errorf("HasError = %v, want %v", got, tc.wantError)
			}
		})
	}
}

// TestSkillResource_ImportStateSetsSlug proves import passes the ID through to
// the slug attribute (skills are slug-addressed).
func TestSkillResource_ImportStateSetsSlug(t *testing.T) {
	ctx := context.Background()
	r := &SkillResource{}
	var sr fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &sr)
	if sr.Diagnostics.HasError() {
		t.Fatalf("schema diags: %v", sr.Diagnostics)
	}

	objType := sr.Schema.Type().TerraformType(ctx).(tftypes.Object)
	vals := make(map[string]tftypes.Value, len(objType.AttributeTypes))
	for name, typ := range objType.AttributeTypes {
		vals[name] = tftypes.NewValue(typ, nil)
	}
	resp := fwresource.ImportStateResponse{
		State: tfsdk.State{Schema: sr.Schema, Raw: tftypes.NewValue(objType, vals)},
	}
	r.ImportState(ctx, fwresource.ImportStateRequest{ID: "deploy-runbook"}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("import diags: %v", resp.Diagnostics)
	}

	var slug types.String
	resp.Diagnostics.Append(resp.State.GetAttribute(ctx, path.Root("slug"), &slug)...)
	if slug.ValueString() != "deploy-runbook" {
		t.Errorf("slug = %q, want deploy-runbook", slug.ValueString())
	}
}

// TestSkillResource_SchemaHasNoReservedAttributes guards the reason `provider`
// is not surfaced: Terraform reserves it as a root attribute name in resource
// schemas (see the SkillResourceModel doc comment).
func TestSkillResource_SchemaHasNoReservedAttributes(t *testing.T) {
	ctx := context.Background()
	var sr fwresource.SchemaResponse
	(&SkillResource{}).Schema(ctx, fwresource.SchemaRequest{}, &sr)
	for _, reserved := range []string{"provider", "count", "for_each", "depends_on", "lifecycle"} {
		if _, ok := sr.Schema.Attributes[reserved]; ok {
			t.Errorf("schema declares reserved root attribute %q", reserved)
		}
	}
}
