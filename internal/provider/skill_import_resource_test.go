package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func strPtr(s string) *string { return &s }

func TestParseSkillImportSource(t *testing.T) {
	for _, tc := range []struct {
		in, head, skill string
		ref             *string
	}{
		{"acme/skills", "acme/skills", "", nil},
		{"acme/skills/deploy#v1", "acme/skills/deploy", "", strPtr("v1")},
		{"  acme/skills#v1  ", "acme/skills", "", strPtr("v1")},
		{"acme/skills#", "acme/skills", "", nil},
		{"acme/skills#main@deploy", "acme/skills", "deploy", strPtr("main")},
		{"acme/skills#@deploy", "acme/skills", "deploy", nil},
		{"acme/skills#release%2Fv2", "acme/skills", "", strPtr("release/v2")},
		{"acme/skills#release/v2", "acme/skills", "", strPtr("release/v2")},
		{"github:acme/skills#v1", "acme/skills", "", strPtr("v1")},
		{"acme/skills@deploy#v3", "acme/skills@deploy", "", strPtr("v3")},
		{"https://github.com/acme/skills/tree/main/deploy", "https://github.com/acme/skills/tree/main/deploy", "", nil},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got := parseSkillImportSource(tc.in)
			if got.head != tc.head || got.skill != tc.skill {
				t.Errorf("head/skill = %q/%q, want %q/%q", got.head, got.skill, tc.head, tc.skill)
			}
			switch {
			case tc.ref == nil && got.ref != nil:
				t.Errorf("ref = %q, want nil", *got.ref)
			case tc.ref != nil && (got.ref == nil || *got.ref != *tc.ref):
				t.Errorf("ref = %v, want %q", got.ref, *tc.ref)
			}
		})
	}
}

func TestSkillImportSourceFromProvenance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src      client.SkillSourceResponse
		want     string
		wantHead string // what the server's grammar sees before '#'
	}{
		{"root default branch", client.SkillSourceResponse{Url: "https://github.com/acme/skills"}, "acme/skills", "acme/skills"},
		{"path and ref", client.SkillSourceResponse{Url: "https://github.com/acme/skills", Path: strPtr("deploy"), Ref: strPtr("v1")}, "acme/skills/deploy#v1", "acme/skills/deploy"},
		{"nested path, slash ref", client.SkillSourceResponse{Url: "https://github.com/acme/skills/", Path: strPtr("/a/b/"), Ref: strPtr("release/v2")}, "acme/skills/a/b#release/v2", "acme/skills/a/b"},
		{"delimiters in ref escaped", client.SkillSourceResponse{Url: "https://github.com/acme/skills", Ref: strPtr("we@ird#ref")}, "acme/skills#we%40ird%23ref", "acme/skills"},
		// The server never unquotes the part before '#', so '@' and '%' in a
		// path are literal and are written as they are.
		{"at sign in path", client.SkillSourceResponse{Url: "https://github.com/acme/skills", Path: strPtr("team@x/deploy"), Ref: strPtr("v1")}, "acme/skills/team@x/deploy#v1", "acme/skills/team@x/deploy"},
		{"percent in path", client.SkillSourceResponse{Url: "https://github.com/acme/skills", Path: strPtr("100%25/deploy")}, "acme/skills/100%25/deploy", "acme/skills/100%25/deploy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := skillImportSourceFromProvenance(&tc.src)
			if !ok || got != tc.want {
				t.Fatalf("got %q (ok=%v), want %q", got, ok, tc.want)
			}
			p := parseSkillImportSource(got)
			if p.head != tc.wantHead || p.skill != "" {
				t.Errorf("round trip head/skill = %q/%q, want %q with no skill", p.head, p.skill, tc.wantHead)
			}
			switch {
			case tc.src.Ref == nil && p.ref != nil:
				t.Errorf("round trip ref = %q, want none", *p.ref)
			case tc.src.Ref != nil && (p.ref == nil || *p.ref != *tc.src.Ref):
				t.Errorf("round trip ref = %v, want %q", p.ref, *tc.src.Ref)
			}
		})
	}

	// A path the grammar cannot express must not be rebuilt: "dir#notes"
	// would otherwise become path "dir" at ref "notes".
	for _, path := range []string{"dir#notes", "a/b#c", "has space", "tab\there"} {
		t.Run("unrepresentable "+path, func(t *testing.T) {
			src := client.SkillSourceResponse{Url: "https://github.com/acme/skills", Path: strPtr(path), Ref: strPtr("v1")}
			if got, ok := skillImportSourceFromProvenance(&src); ok {
				t.Fatalf("rebuilt %q for unrepresentable path %q", got, path)
			}
		})
	}
}

func TestValidateSkillImportSource(t *testing.T) {
	for _, tc := range []struct {
		in      string
		wantErr bool
	}{
		{"acme/skills#v1", false},
		{"", true},
		{"   ", true},
		{"acme/skills #v1", true},
		{strings.Repeat("a", 501), true},
	} {
		if got := validateSkillImportSource(tc.in).HasError(); got != tc.wantErr {
			t.Errorf("validate(%q) error = %v, want %v", tc.in, got, tc.wantErr)
		}
	}
}

func importModel(source string, commit, ref *string) SkillImportResourceModel {
	m := SkillImportResourceModel{
		Source:    types.StringValue(source),
		CommitSHA: types.StringPointerValue(commit),
		Ref:       types.StringPointerValue(ref),
	}
	return m
}

func TestPlannedSkillImportRefresh(t *testing.T) {
	sha := strPtr("abc")
	for _, tc := range []struct {
		name        string
		plan, state SkillImportResourceModel
		want        bool
	}{
		{"unchanged", importModel("acme/s#v1", nil, nil), importModel("acme/s#v1", sha, strPtr("v1")), false},
		{"ref bump", importModel("acme/s#v2", nil, nil), importModel("acme/s#v1", sha, strPtr("v1")), true},
		{"detached", importModel("acme/s#v1", nil, nil), importModel("acme/s#v1", nil, nil), true},
		{"out-of-band ref drift", importModel("acme/s#v1", nil, nil), importModel("acme/s#v1", sha, strPtr("v3")), true},
		{"out-of-band pin on default branch", importModel("acme/s", nil, nil), importModel("acme/s", sha, strPtr("v3")), true},
		{"url form not judged", importModel("https://github.com/acme/s/tree/v1", nil, nil),
			importModel("https://github.com/acme/s/tree/v1", sha, strPtr("v1")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := plannedSkillImportRefresh(tc.plan, tc.state); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecideSkillImportRefresh(t *testing.T) {
	attached := func(ref *string) skillImportRecord { return skillImportRecord{attached: true, ref: ref} }
	detached := skillImportRecord{}
	sha := strPtr("abc")
	adopted := importModel("", sha, nil)
	adopted.Source = types.StringNull()

	for _, tc := range []struct {
		name        string
		plan, state SkillImportResourceModel
		recorded    skillImportRecord
		force       bool
		want        skillRefreshKind
		wantRef     string
	}{
		{"ref bump sends only ref", importModel("acme/s#v2", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v1")), false, skillRefreshRef, "v2"},
		{"ref bump ignores force", importModel("acme/s#v2", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v1")), true, skillRefreshRef, "v2"},
		{"new repo without force is blocked", importModel("acme/other#v1", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v1")), false, skillRefreshBlockedRepoint, ""},
		{"new repo with force re-points", importModel("acme/other#v1", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v1")), true, skillRefreshRepoint, ""},
		{"new path without force is blocked", importModel("acme/s/b#v1", nil, nil), importModel("acme/s/a#v1", sha, nil),
			attached(strPtr("v1")), false, skillRefreshBlockedRepoint, ""},
		{"skill selector without force is blocked", importModel("acme/s#v1@a", nil, nil), importModel("acme/s#v1@b", sha, nil),
			attached(strPtr("v1")), false, skillRefreshBlockedRepoint, ""},
		{"skill selector with force re-points", importModel("acme/s#v1@a", nil, nil), importModel("acme/s#v1@b", sha, nil),
			attached(strPtr("v1")), true, skillRefreshRepoint, ""},
		{"dropping the ref without force is blocked", importModel("acme/s", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v1")), false, skillRefreshBlockedRepoint, ""},
		{"dropping the ref with force re-points", importModel("acme/s", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v1")), true, skillRefreshRepoint, ""},
		{"out-of-band pin, config on default branch, no force", importModel("acme/s", nil, nil), importModel("acme/s", sha, nil),
			attached(strPtr("v3")), false, skillRefreshBlockedRepoint, ""},
		{"adopted with unrebuildable source, no force", importModel("acme/s#v1", nil, nil), adopted,
			attached(strPtr("v1")), false, skillRefreshBlockedRepoint, ""},
		{"detached without force is blocked", importModel("acme/s#v1", nil, nil), importModel("acme/s#v1", nil, nil),
			detached, false, skillRefreshBlockedDetached, ""},
		{"detached with force re-attaches", importModel("acme/s#v1", nil, nil), importModel("acme/s#v1", nil, nil),
			detached, true, skillRefreshRepoint, ""},
		{"already matches (e.g. force toggled)", importModel("acme/s#v1", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v1")), false, skillRefreshNone, ""},
		{"out-of-band ref drift restored", importModel("acme/s#v1", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v3")), false, skillRefreshRef, "v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decideSkillImportRefresh(tc.plan, tc.state, tc.recorded, tc.force)
			if got.kind != tc.want {
				t.Fatalf("kind = %d, want %d", got.kind, tc.want)
			}
			switch got.kind {
			case skillRefreshRef:
				if got.body.Source != nil || got.body.Force != nil {
					t.Errorf("a ref bump must not send source/force: %+v", got.body)
				}
				if got.body.Ref == nil || *got.body.Ref != tc.wantRef {
					t.Errorf("ref = %v, want %q", got.body.Ref, tc.wantRef)
				}
			case skillRefreshRepoint:
				if !tc.force {
					t.Errorf("re-pointed without the user's force")
				}
				if got.body.Source == nil || *got.body.Source != tc.plan.Source.ValueString() {
					t.Errorf("source = %v, want %q", got.body.Source, tc.plan.Source.ValueString())
				}
				if got.body.Force == nil || !*got.body.Force {
					t.Errorf("re-point must send force=true: %+v", got.body)
				}
			}
		})
	}
}

func TestSkillImportRecordFromState(t *testing.T) {
	if r := skillImportRecordFromState(importModel("acme/s#v1", nil, strPtr("v1"))); r.attached {
		t.Errorf("null commit_sha must read as detached: %+v", r)
	}
	r := skillImportRecordFromState(importModel("acme/s#v1", strPtr("abc"), strPtr("v1")))
	if !r.attached || r.ref == nil || *r.ref != "v1" {
		t.Errorf("attached record = %+v", r)
	}
}

// ---------------------------------------------------------------------------
// Hermetic lifecycle: real terraform plan/apply against a stateful fake API.
// ---------------------------------------------------------------------------

type fakeSkill struct {
	ID, Slug, Name, Summary string
	Content                 string
	// provenance; URL == "" means detached / never imported.
	URL, Path, Ref, Commit string
}

// fakeCall records one request. Refresh is the only body the tests inspect,
// so it is decoded into the generated request type.
type fakeCall struct {
	Method, Path string
	Refresh      client.SkillRefreshRequest
}

// fakeAPIError is the API's error envelope, reduced to what the provider reads.
type fakeAPIError struct {
	Detail string `json:"detail"`
	Code   string `json:"code,omitempty"`
}

// fakeSkillAPI implements the slice of the skills API the resource uses, with
// the refresh coordinate rules of skill_import/service.py::_refresh_coordinate.
type fakeSkillAPI struct {
	mu     sync.Mutex
	skills map[string]*fakeSkill
	calls  []fakeCall
	nextID int
}

func newFakeSkillAPI() *fakeSkillAPI { return &fakeSkillAPI{skills: map[string]*fakeSkill{}} }

func fakeCommit(url, path, ref string) string {
	h := sha256.Sum256([]byte(url + "|" + path + "|" + ref))
	return hex.EncodeToString(h[:20])
}

// resolve parses `owner/repo[/path][#ref]` (enough grammar for these tests).
func fakeResolve(source string) (url, path, ref string) {
	p := parseSkillImportSource(source)
	parts := strings.SplitN(p.head, "/", 3)
	url = "https://github.com/" + parts[0] + "/" + parts[1]
	if len(parts) == 3 {
		path = parts[2]
	}
	if p.ref != nil {
		ref = *p.ref
	}
	return url, path, ref
}

func (f *fakeSkillAPI) attach(s *fakeSkill, source string) {
	s.URL, s.Path, s.Ref = fakeResolve(source)
	s.Commit = fakeCommit(s.URL, s.Path, s.Ref)
	s.Content = "# content at " + s.Commit[:8] + "\n"
	s.Summary = "summary at " + s.Ref
}

var fakeTime = time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)

func (f *fakeSkillAPI) response(s *fakeSkill) client.SkillResponse {
	out := client.SkillResponse{
		Id: s.ID, Slug: s.Slug, Name: s.Name, Summary: s.Summary,
		Scope: client.SkillScopeOrg, Provider: client.SkillProviderCustom,
		CreatedAt: fakeTime, UpdatedAt: fakeTime,
		Files: []client.SkillFileResponse{{
			Id: "f-1", Filename: "SKILL.md", Content: s.Content, ContentHash: "h",
			CreatedAt: fakeTime, UpdatedAt: fakeTime,
		}},
	}
	if s.URL != "" {
		src := client.SkillSourceResponse{
			Kind: client.SkillSourceKindGithub, Url: s.URL, CommitSha: s.Commit, ImportedAt: fakeTime,
		}
		if s.Ref != "" {
			src.Ref = strPtr(s.Ref)
		}
		if s.Path != "" {
			src.Path = strPtr(s.Path)
		}
		out.Source = &src
	}
	return out
}

func writeFakeJSON(w http.ResponseWriter, code int, payload []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(payload)
}

func writeFakeSkill(w http.ResponseWriter, code int, s client.SkillResponse) {
	b, err := json.Marshal(s)
	if err != nil {
		writeFakeError(w, http.StatusInternalServerError, fakeAPIError{Detail: err.Error()})
		return
	}
	writeFakeJSON(w, code, b)
}

func writeFakeError(w http.ResponseWriter, code int, e fakeAPIError) {
	b, _ := json.Marshal(e)
	writeFakeJSON(w, code, b)
}

func (f *fakeSkillAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	raw, _ := io.ReadAll(r.Body)
	call := fakeCall{Method: r.Method, Path: r.URL.Path}

	const prefix = "/v1/orgs/org-1/skills"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	switch {
	case r.Method == http.MethodPost && rest == "/import":
		f.calls = append(f.calls, call)
		var body client.SkillImportRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			writeFakeError(w, http.StatusUnprocessableEntity, fakeAPIError{Detail: err.Error()})
			return
		}
		f.nextID++
		s := &fakeSkill{ID: fmt.Sprintf("sk-%d", f.nextID), Slug: "deploy", Name: "Deploy"}
		if body.Name != nil {
			s.Name, s.Slug = *body.Name, strings.ToLower(*body.Name)
		}
		f.attach(s, body.Source)
		f.skills[s.Slug] = s
		writeFakeSkill(w, http.StatusCreated, f.response(s))

	case r.Method == http.MethodPost && strings.HasSuffix(rest, "/refresh"):
		var body client.SkillRefreshRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			writeFakeError(w, http.StatusUnprocessableEntity, fakeAPIError{Detail: err.Error()})
			return
		}
		call.Refresh = body
		f.calls = append(f.calls, call)
		s, ok := f.skills[strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/refresh")]
		if !ok {
			writeFakeError(w, http.StatusNotFound, fakeAPIError{Detail: "Skill not found"})
			return
		}
		force := body.Force != nil && *body.Force
		attached := s.URL != ""
		switch {
		case body.Source != nil && !force:
			writeFakeError(w, http.StatusConflict, fakeAPIError{Detail: "pass force", Code: "skill_not_imported"})
		case body.Source != nil:
			f.attach(s, *body.Source)
			writeFakeSkill(w, http.StatusOK, f.response(s))
		case !attached && force:
			writeFakeError(w, http.StatusUnprocessableEntity, fakeAPIError{Detail: "force needs source"})
		case !attached:
			writeFakeError(w, http.StatusConflict, fakeAPIError{Detail: "detached", Code: "skill_not_imported"})
		default:
			ref := s.Ref
			if body.Ref != nil {
				ref = *body.Ref
			}
			if commit := fakeCommit(s.URL, s.Path, ref); commit != s.Commit || force {
				s.Ref, s.Commit = ref, commit
				s.Content = "# content at " + commit[:8] + "\n"
				s.Summary = "summary at " + ref
			}
			writeFakeSkill(w, http.StatusOK, f.response(s))
		}

	case strings.HasPrefix(rest, "/") && !strings.Contains(rest[1:], "/"):
		f.calls = append(f.calls, call)
		s, ok := f.skills[rest[1:]]
		if !ok {
			writeFakeError(w, http.StatusNotFound, fakeAPIError{Detail: "Skill not found"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeFakeSkill(w, http.StatusOK, f.response(s))
		case http.MethodDelete:
			delete(f.skills, s.Slug)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	default:
		writeFakeError(w, http.StatusNotFound, fakeAPIError{Detail: "no route " + r.URL.Path})
	}
}

// detach simulates a local edit in the Botyard UI.
func (f *fakeSkillAPI) detach(slug string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.skills[slug]
	s.URL, s.Path, s.Ref, s.Commit = "", "", "", ""
	s.Content = "# edited by a human\n"
}

// refreshCalls returns the bodies of every /refresh call since the last reset.
func (f *fakeSkillAPI) takeRefreshBodies() []client.SkillRefreshRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []client.SkillRefreshRequest
	for _, c := range f.calls {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/refresh") {
			out = append(out, c.Refresh)
		}
	}
	f.calls = nil
	return out
}

func skillImportConfig(endpoint, source, extra string) string {
	return fmt.Sprintf(`
provider "botyard" {
  endpoint = %[1]q
  api_key  = "byk_test"
  org_id   = "org-1"
}

resource "botyard_skill_import" "deploy" {
  source = %[2]q
  %[3]s
}
`, endpoint, source, extra)
}

const skillImportAddr = "botyard_skill_import.deploy"

// TestSkillImportResource_Lifecycle drives real terraform plan/apply cycles
// against the fake API. It is the evidence for the in-place-refresh design:
// the id captured at create must survive a ref bump, a forced re-attach after
// a local edit, and a re-point to a different repository.
func TestSkillImportResource_Lifecycle(t *testing.T) {
	fake := newFakeSkillAPI()
	srv := httptest.NewServer(fake)
	defer srv.Close()

	var createdID string
	captureID := func(s *terraform.State) error {
		createdID = s.RootModule().Resources[skillImportAddr].Primary.ID
		if createdID == "" {
			return fmt.Errorf("no id after create")
		}
		return nil
	}
	sameID := func(s *terraform.State) error {
		if got := s.RootModule().Resources[skillImportAddr].Primary.ID; got != createdID {
			return fmt.Errorf("id changed from %q to %q: the skill was replaced, breaking assignments", createdID, got)
		}
		return nil
	}
	expectRefresh := func(want ...client.SkillRefreshRequest) resource.TestCheckFunc {
		return func(*terraform.State) error {
			got := fake.takeRefreshBodies()
			if len(got) != len(want) {
				return fmt.Errorf("refresh calls = %+v, want %+v", got, want)
			}
			for i := range want {
				g, _ := json.Marshal(got[i])
				w, _ := json.Marshal(want[i])
				if string(g) != string(w) {
					return fmt.Errorf("refresh body %d = %s, want %s", i, g, w)
				}
			}
			return nil
		}
	}

	v1, v2 := "acme/skills/deploy#v1", "acme/skills/deploy#v2"
	other := "acme/other/deploy#v1"
	forced := func(source string) client.SkillRefreshRequest {
		force := true
		return client.SkillRefreshRequest{Force: &force, Source: strPtr(source)}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(*terraform.State) error {
			if n := len(fake.skills); n != 0 {
				return fmt.Errorf("%d skill(s) left after destroy", n)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				// 1. Create via /import. The framework also asserts that a
				// follow-up plan is empty.
				PreConfig: func() { fake.takeRefreshBodies() },
				Config:    skillImportConfig(srv.URL, v1, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					captureID,
					resource.TestCheckResourceAttr(skillImportAddr, "slug", "deploy"),
					resource.TestCheckResourceAttr(skillImportAddr, "source_kind", "github"),
					resource.TestCheckResourceAttr(skillImportAddr, "source_url", "https://github.com/acme/skills"),
					resource.TestCheckResourceAttr(skillImportAddr, "path", "deploy"),
					resource.TestCheckResourceAttr(skillImportAddr, "ref", "v1"),
					resource.TestCheckResourceAttr(skillImportAddr, "commit_sha",
						fakeCommit("https://github.com/acme/skills", "deploy", "v1")),
					resource.TestCheckResourceAttr(skillImportAddr, "files.#", "1"),
					resource.TestCheckResourceAttr(skillImportAddr, "force", "false"),
					expectRefresh(),
				),
			},
			{
				// 2. Unchanged config: a clean no-op plan, and no API writes.
				Config: skillImportConfig(srv.URL, v1, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()},
				},
				Check: expectRefresh(),
			},
			{
				// 3. v1 -> v2 is an in-place update that sends only {ref}.
				Config: skillImportConfig(srv.URL, v2, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(skillImportAddr, plancheck.ResourceActionUpdate),
						plancheck.ExpectUnknownValue(skillImportAddr, tfjsonpath.New("commit_sha")),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					sameID,
					resource.TestCheckResourceAttr(skillImportAddr, "ref", "v2"),
					resource.TestCheckResourceAttr(skillImportAddr, "commit_sha",
						fakeCommit("https://github.com/acme/skills", "deploy", "v2")),
					expectRefresh(client.SkillRefreshRequest{Ref: strPtr("v2")}),
				),
			},
			{
				// 4. A human edits the skill in Botyard, detaching it. The
				// default is to refuse, with an actionable message, and not
				// to call /refresh at all.
				PreConfig:   func() { fake.detach("deploy") },
				Config:      skillImportConfig(srv.URL, v2, ""),
				ExpectError: regexp.MustCompile(`(?s)Skill has local edits.*force = true.*terraform state rm`),
			},
			{
				// 5. The edit survived the refused apply.
				PreConfig: func() {
					if got := fake.skills["deploy"].Content; got != "# edited by a human\n" {
						t.Errorf("refused apply overwrote the edit: content = %q", got)
					}
					fake.takeRefreshBodies()
				},
				// 6. force = true re-attaches in place.
				Config: skillImportConfig(srv.URL, v2, "force = true"),
				Check: resource.ComposeAggregateTestCheckFunc(
					sameID,
					resource.TestCheckResourceAttr(skillImportAddr, "ref", "v2"),
					resource.TestCheckResourceAttr(skillImportAddr, "commit_sha",
						fakeCommit("https://github.com/acme/skills", "deploy", "v2")),
					expectRefresh(forced(v2)),
				),
			},
			{
				// 7. Force off again: only the attribute changes, which is an
				// in-place update that makes no API write.
				Config: skillImportConfig(srv.URL, v2, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(skillImportAddr, plancheck.ResourceActionUpdate),
						plancheck.ExpectKnownValue(skillImportAddr, tfjsonpath.New("commit_sha"),
							knownvalue.StringExact(fakeCommit("https://github.com/acme/skills", "deploy", "v2"))),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					sameID,
					resource.TestCheckResourceAttr(skillImportAddr, "force", "false"),
					expectRefresh(),
				),
			},
			{
				// 8. Re-pointing at a different repository without force is
				// refused before any write: the API needs force for it, and
				// force could overwrite a concurrent edit.
				Config:      skillImportConfig(srv.URL, other, ""),
				ExpectError: regexp.MustCompile(`(?s)Changing the skill's source needs force.*force = true`),
			},
			{
				// 9. Nothing was sent; with force it re-points in place.
				PreConfig: func() {
					if got := fake.takeRefreshBodies(); len(got) != 0 {
						t.Errorf("refused re-point still called /refresh: %+v", got)
					}
					if got := fake.skills["deploy"].URL; got != "https://github.com/acme/skills" {
						t.Errorf("refused re-point changed the source: %q", got)
					}
				},
				Config: skillImportConfig(srv.URL, other, "force = true"),
				Check: resource.ComposeAggregateTestCheckFunc(
					sameID,
					resource.TestCheckResourceAttr(skillImportAddr, "source_url", "https://github.com/acme/other"),
					resource.TestCheckResourceAttr(skillImportAddr, "force", "true"),
					expectRefresh(forced(other)),
				),
			},
			{
				Config: skillImportConfig(srv.URL, other, ""),
				Check:  expectRefresh(),
			},
			{
				// 10. terraform import by slug fills every computed provenance
				// attribute and rebuilds `source`.
				ResourceName:                         skillImportAddr,
				Config:                               skillImportConfig(srv.URL, other, ""),
				ImportState:                          true,
				ImportStateId:                        "deploy",
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "slug",
			},
		},
	})
}

// TestSkillImportResource_AdoptNeverImported covers `terraform import` of a
// skill authored in Botyard: it has no provenance, so the first apply must be
// refused by default and attach it only with force.
func TestSkillImportResource_AdoptNeverImported(t *testing.T) {
	fake := newFakeSkillAPI()
	fake.skills["deploy"] = &fakeSkill{ID: "sk-authored", Slug: "deploy", Name: "Deploy", Summary: "s", Content: "# hand written\n"}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	src := "acme/skills/deploy#v1"
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				ResourceName:       skillImportAddr,
				Config:             skillImportConfig(srv.URL, src, ""),
				ImportState:        true,
				ImportStateId:      "deploy",
				ImportStatePersist: true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					a := states[0].Attributes
					if a["id"] != "sk-authored" || a["commit_sha"] != "" || a["source"] != "" {
						return fmt.Errorf("unexpected imported attributes: %v", a)
					}
					return nil
				},
			},
			{
				Config:      skillImportConfig(srv.URL, src, ""),
				ExpectError: regexp.MustCompile(`Skill has local edits`),
			},
			{
				Config: skillImportConfig(srv.URL, src, "force = true"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(skillImportAddr, "id", "sk-authored"),
					resource.TestCheckResourceAttr(skillImportAddr, "ref", "v1"),
				),
			},
		},
	})
}

// TestSkillImportResource_SchemaHasNoReservedAttributes mirrors the
// botyard_skill guard.
func TestSkillImportResource_SchemaHasNoReservedAttributes(t *testing.T) {
	var sr fwresource.SchemaResponse
	(&SkillImportResource{}).Schema(context.Background(), fwresource.SchemaRequest{}, &sr)
	if sr.Diagnostics.HasError() {
		t.Fatalf("schema diags: %v", sr.Diagnostics)
	}
	for _, reserved := range []string{"provider", "count", "for_each", "depends_on", "lifecycle"} {
		if _, ok := sr.Schema.Attributes[reserved]; ok {
			t.Errorf("schema declares reserved root attribute %q", reserved)
		}
	}
	src := sr.Schema.Attributes["source"]
	if !src.IsRequired() {
		t.Error("source must be Required")
	}
}
