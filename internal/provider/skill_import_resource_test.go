package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
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
		name string
		src  client.SkillSourceResponse
		want string
	}{
		{"root default branch", client.SkillSourceResponse{Url: "https://github.com/acme/skills"}, "acme/skills"},
		{"path and ref", client.SkillSourceResponse{Url: "https://github.com/acme/skills", Path: strPtr("deploy"), Ref: strPtr("v1")}, "acme/skills/deploy#v1"},
		{"nested path, slash ref", client.SkillSourceResponse{Url: "https://github.com/acme/skills/", Path: strPtr("/a/b/"), Ref: strPtr("release/v2")}, "acme/skills/a/b#release/v2"},
		{"delimiters escaped", client.SkillSourceResponse{Url: "https://github.com/acme/skills", Ref: strPtr("we@ird#ref")}, "acme/skills#we%40ird%23ref"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := skillImportSourceFromProvenance(&tc.src)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			// The rebuilt reference must parse back to the recorded ref.
			if tc.src.Ref != nil {
				if p := parseSkillImportSource(got); p.ref == nil || *p.ref != *tc.src.Ref {
					t.Errorf("round trip ref = %v, want %q", p.ref, *tc.src.Ref)
				}
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
	attached := func(ref *string) *client.SkillResponse {
		return &client.SkillResponse{Source: &client.SkillSourceResponse{
			Url: "https://github.com/acme/s", CommitSha: "abc", Ref: ref,
		}}
	}
	detached := &client.SkillResponse{}
	sha := strPtr("abc")

	for _, tc := range []struct {
		name        string
		plan, state SkillImportResourceModel
		current     *client.SkillResponse
		force       bool
		want        skillRefreshKind
		wantRef     string
	}{
		{"ref bump sends only ref", importModel("acme/s#v2", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v1")), false, skillRefreshRef, "v2"},
		{"ref bump ignores force", importModel("acme/s#v2", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v1")), true, skillRefreshRef, "v2"},
		{"repoint on new repo", importModel("acme/other#v1", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v1")), false, skillRefreshRepoint, ""},
		{"repoint on skill selector", importModel("acme/s#v1@a", nil, nil), importModel("acme/s#v1@b", sha, nil),
			attached(strPtr("v1")), false, skillRefreshRepoint, ""},
		{"repoint to default branch", importModel("acme/s", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v1")), false, skillRefreshRepoint, ""},
		{"detached without force is blocked", importModel("acme/s#v1", nil, nil), importModel("acme/s#v1", nil, nil),
			detached, false, skillRefreshBlocked, ""},
		{"detached with force re-attaches", importModel("acme/s#v1", nil, nil), importModel("acme/s#v1", nil, nil),
			detached, true, skillRefreshRepoint, ""},
		{"already matches (e.g. force toggled)", importModel("acme/s#v1", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v1")), false, skillRefreshNone, ""},
		{"out-of-band ref drift restored", importModel("acme/s#v1", nil, nil), importModel("acme/s#v1", sha, nil),
			attached(strPtr("v3")), false, skillRefreshRef, "v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decideSkillImportRefresh(tc.plan, tc.state, tc.current, tc.force)
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

// ---------------------------------------------------------------------------
// Hermetic lifecycle: real terraform plan/apply against a stateful fake API.
// ---------------------------------------------------------------------------

type fakeSkill struct {
	ID, Slug, Name, Summary string
	Content                 string
	// provenance; URL == "" means detached / never imported.
	URL, Path, Ref, Commit string
}

type fakeCall struct {
	Method, Path string
	Body         map[string]any
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

func (f *fakeSkillAPI) json(s *fakeSkill) map[string]any {
	out := map[string]any{
		"id": s.ID, "slug": s.Slug, "name": s.Name, "summary": s.Summary,
		"scope": "org", "provider": "custom", "source": nil,
		"created_at": "2026-09-23T10:00:00Z", "updated_at": "2026-09-23T10:00:00Z",
		"files": []map[string]any{{
			"id": "f-1", "filename": "SKILL.md", "content": s.Content, "content_hash": "h",
			"sort_order": 0, "created_at": "2026-09-23T10:00:00Z", "updated_at": "2026-09-23T10:00:00Z",
		}},
	}
	if s.URL != "" {
		src := map[string]any{
			"kind": "github", "url": s.URL, "commit_sha": s.Commit,
			"imported_at": "2026-09-23T10:00:00Z", "ref": nil, "path": nil,
		}
		if s.Ref != "" {
			src["ref"] = s.Ref
		}
		if s.Path != "" {
			src["path"] = s.Path
		}
		out["source"] = src
	}
	return out
}

func (f *fakeSkillAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.calls = append(f.calls, fakeCall{Method: r.Method, Path: r.URL.Path, Body: body})

	write := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	const prefix = "/v1/orgs/org-1/skills"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	switch {
	case r.Method == http.MethodPost && rest == "/import":
		f.nextID++
		s := &fakeSkill{ID: fmt.Sprintf("sk-%d", f.nextID), Slug: "deploy", Name: "Deploy"}
		if n, ok := body["name"].(string); ok {
			s.Name, s.Slug = n, strings.ToLower(n)
		}
		f.attach(s, body["source"].(string))
		f.skills[s.Slug] = s
		write(http.StatusCreated, f.json(s))

	case r.Method == http.MethodPost && strings.HasSuffix(rest, "/refresh"):
		s, ok := f.skills[strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/refresh")]
		if !ok {
			write(http.StatusNotFound, map[string]any{"detail": "Skill not found"})
			return
		}
		force, _ := body["force"].(bool)
		source, hasSource := body["source"].(string)
		attached := s.URL != ""
		switch {
		case hasSource && !force:
			write(http.StatusConflict, map[string]any{"detail": "pass force", "code": "skill_not_imported"})
		case hasSource:
			f.attach(s, source)
			write(http.StatusOK, f.json(s))
		case !attached && force:
			write(http.StatusUnprocessableEntity, map[string]any{"detail": "force needs source"})
		case !attached:
			write(http.StatusConflict, map[string]any{"detail": "detached", "code": "skill_not_imported"})
		default:
			ref := s.Ref
			if rv, ok := body["ref"].(string); ok {
				ref = rv
			}
			if commit := fakeCommit(s.URL, s.Path, ref); commit != s.Commit || force {
				s.Ref, s.Commit = ref, commit
				s.Content = "# content at " + commit[:8] + "\n"
				s.Summary = "summary at " + ref
			}
			write(http.StatusOK, f.json(s))
		}

	case strings.HasPrefix(rest, "/") && !strings.Contains(rest[1:], "/"):
		s, ok := f.skills[rest[1:]]
		if !ok {
			write(http.StatusNotFound, map[string]any{"detail": "Skill not found"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			write(http.StatusOK, f.json(s))
		case http.MethodDelete:
			delete(f.skills, s.Slug)
			write(http.StatusNoContent, nil)
		default:
			write(http.StatusMethodNotAllowed, nil)
		}
	default:
		write(http.StatusNotFound, map[string]any{"detail": "no route " + r.URL.Path})
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
func (f *fakeSkillAPI) takeRefreshBodies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/refresh") {
			out = append(out, c.Body)
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
	expectRefresh := func(want ...map[string]any) resource.TestCheckFunc {
		return func(*terraform.State) error {
			got := fake.takeRefreshBodies()
			if len(got) != len(want) {
				return fmt.Errorf("refresh calls = %v, want %v", got, want)
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
					expectRefresh(map[string]any{"ref": "v2", "source": nil}),
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
					expectRefresh(map[string]any{"force": true, "ref": nil, "source": v2}),
				),
			},
			{
				// 7. Re-point at a different repository, still in place.
				Config: skillImportConfig(srv.URL, "acme/other/deploy#v1", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					sameID,
					resource.TestCheckResourceAttr(skillImportAddr, "source_url", "https://github.com/acme/other"),
					resource.TestCheckResourceAttr(skillImportAddr, "force", "false"),
					expectRefresh(map[string]any{"force": true, "ref": nil, "source": "acme/other/deploy#v1"}),
				),
			},
			{
				// 8. terraform import by slug fills every computed provenance
				// attribute and rebuilds `source`.
				ResourceName:                         skillImportAddr,
				Config:                               skillImportConfig(srv.URL, "acme/other/deploy#v1", ""),
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
