package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// --- fixtures ---------------------------------------------------------------

const (
	pvCRM      = "11111111-1111-4111-8111-111111111111"
	pvCRMOld   = "11111111-1111-4111-8111-000000000001"
	pvSupport  = "22222222-2222-4222-8222-222222222222"
	pvArchived = "33333333-3333-4333-8333-333333333333"
	pvEmpty    = "44444444-4444-4444-8444-444444444444"
	pvBadTool  = "55555555-5555-4555-8555-555555555555"
	pvBadConn  = "66666666-6666-4666-8666-666666666666"
	pvNoRT     = "77777777-7777-4777-8777-777777777777"
)

func cataloguePackJSON(slug, packID, versionID string, versionNo int, publisher string) string {
	pub := "null"
	if publisher != "" {
		pub = fmt.Sprintf("%q", publisher)
	}
	return fmt.Sprintf(`{"pack_id":%q,"slug":%q,"visibility":"curated","publisher_org_id":%s,
"latest":{"pack_version_id":%q,"version_no":%d,"name":"%s v%d","summary":"About %s","avatar_sha256":"abc","content_hash":"h-%s","created_at":"2026-10-20T10:00:00Z"}}`,
		packID, slug, pub, versionID, versionNo, slug, versionNo, slug, versionID)
}

// The catalogue, in slug order as the API returns it.
var fakeCatalogue = []struct{ slug, json string }{
	{"crm", cataloguePackJSON("crm", "pk-crm", pvCRM, 3, "")},
	{"empty", cataloguePackJSON("empty", "pk-empty", pvEmpty, 1, "")},
	{"support", cataloguePackJSON("support", "pk-support", pvSupport, 1, "org-pub")},
}

func orgPackVersionJSON(id, slug string, versionNo int, archived, latest bool, skills, tools, connections, settings, model string) string {
	return fmt.Sprintf(`{"pack_id":"pk-%[2]s","pack_slug":%[2]q,"visibility":"curated","publisher_org_id":null,
"pack_archived":%[4]t,"is_latest":%[5]t,"pack_version_id":%[1]q,"version_no":%[3]d,"name":"%[2]s","summary":"s",
"settings":%[9]s,"recommended_model":%[10]s,"start_with_default_tools":false,"avatar_sha256":"av","avatar_url":"https://x/av",
"content_hash":"ch","changelog":null,"created_at":"2026-10-20T10:00:00Z","skills":[%[6]s],"tools":[%[7]s],"connections":[%[8]s],
"support":[],"description_md":"# %[2]s","soul":"You are %[2]s."}`,
		id, slug, versionNo, archived, latest, skills, tools, connections, settings, model)
}

const (
	fxSkill      = `{"required":true,"skill":{"skill_id":"sk-1","skill_slug":"crm-notes","skill_name":"CRM notes","skill_version_id":"skv-1","version_no":2,"version_name":"v2"}}`
	fxSkill2     = `{"required":false,"skill":{"skill_id":"sk-2","skill_slug":"tone","skill_name":"Tone","skill_version_id":"skv-2","version_no":1,"version_name":"v1"}}`
	fxToolMcp    = `{"required":true,"tool":{"runtime":"mcp","tool_id":"t-mcp","slug":"hubspot_search","name":"search","title":null,"mcp_server":"hubspot"}}`
	fxToolClaw   = `{"required":false,"tool":{"runtime":"openclaw","tool_id":"t-claw","slug":"web_fetch","name":"web_fetch","title":"Web fetch"}}`
	fxToolNative = `{"required":false,"tool":{"runtime":"native","tool_id":"t-nat","slug":"bash","name":"bash","title":null}}`
	fxConnMcp    = `{"kind":"mcp_catalog_entry","required":true,"purpose":"CRM access","mcp_catalog_entry":{"mcp_catalog_entry_id":"ce-1","slug":"hubspot","name":"HubSpot","logo_url":null,"logo_href":null},"tool_names":["search","get"]}`
	fxConnMcpAll = `{"kind":"mcp_catalog_entry","required":false,"purpose":null,"mcp_catalog_entry":{"mcp_catalog_entry_id":"ce-2","slug":"linear","name":"Linear","logo_url":null,"logo_href":null},"tool_names":null}`
	fxConnVault  = `{"kind":"vault_secret","required":true,"purpose":null,"secret_spec":{"key_path":"crm.api_key","display_name":"CRM key","description":null,"sensitivity":"secret","max_ttl_seconds":600}}`
	fxSettings   = `{"thinking_default":"high","reasoning_default":null,"tool_search":{"mode":"auto","auto_threshold_percent":20},"tool_images":null}`
	fxNoSettings = `{"thinking_default":null,"reasoning_default":null,"tool_search":null,"tool_images":null}`
)

var fakeVersions = map[string]string{
	pvCRM: orgPackVersionJSON(pvCRM, "crm", 3, false, true,
		fxSkill+","+fxSkill2, fxToolMcp+","+fxToolClaw+","+fxToolNative, fxConnMcp+","+fxConnMcpAll+","+fxConnVault,
		fxSettings, `{"provider":"botyard","model":"claude-sonnet-5-5"}`),
	pvCRMOld:   orgPackVersionJSON(pvCRMOld, "crm", 1, false, false, fxSkill, fxToolMcp, "", fxNoSettings, "null"),
	pvArchived: orgPackVersionJSON(pvArchived, "retired", 4, true, true, fxSkill, "", "", fxNoSettings, "null"),
	pvEmpty:    orgPackVersionJSON(pvEmpty, "empty", 1, false, true, "", "", "", fxNoSettings, "null"),
	pvBadTool: orgPackVersionJSON(pvBadTool, "future", 1, false, true, "",
		fxToolMcp+`,{"required":true,"tool":{"runtime":"wasm","tool_id":"t-w","slug":"w","name":"w","title":null}}`, "", fxNoSettings, "null"),
	pvBadConn: orgPackVersionJSON(pvBadConn, "future", 1, false, true, "", "",
		`{"kind":"oauth_app","required":true,"purpose":null}`, fxNoSettings, "null"),
	pvNoRT: orgPackVersionJSON(pvNoRT, "future", 1, false, true, "",
		`{"required":true,"tool":{"tool_id":"t-x","slug":"x","name":"x","title":null}}`, "", fxNoSettings, "null"),
}

// --- fake API ---------------------------------------------------------------

type packsAPI struct {
	mu        sync.Mutex
	pageSize  int // packs per page; 0 means all on one page
	failWith  int // when non-zero every request returns this status
	listCalls []string
}

func newPacksAPI(t *testing.T) (*packsAPI, *httptest.Server) {
	t.Helper()
	api := &packsAPI{}
	srv := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(srv.Close)
	return api, srv
}

func (a *packsAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if a.failWith != 0 {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(a.failWith)
		_, _ = fmt.Fprintf(w, `{"title":"Not Found","detail":"organization org-1 not found","status":%d}`, a.failWith)
		return
	}
	switch r.URL.Path {
	case "/v1/orgs/org-1/packs":
		a.listCalls = append(a.listCalls, r.URL.RawQuery)
		a.servePacks(w, r)
	case "/v1/orgs/org-1/packs/versions":
		a.serveVersions(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"no route"}`))
	}
}

func (a *packsAPI) servePacks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	requested, filtered := q["slugs"]
	var matched []string
	var notFound []string
	if filtered {
		have := map[string]bool{}
		for _, p := range fakeCatalogue {
			have[p.slug] = true
		}
		for _, s := range requested {
			if !have[s] {
				notFound = append(notFound, s)
			}
		}
	}
	want := map[string]bool{}
	for _, s := range requested {
		want[s] = true
	}
	for _, p := range fakeCatalogue {
		if (!filtered || want[p.slug]) && (q.Get("cursor") == "" || p.slug > q.Get("cursor")) {
			matched = append(matched, p.json)
		}
	}
	next := "null"
	if a.pageSize > 0 && len(matched) > a.pageSize {
		matched = matched[:a.pageSize]
		var last struct{ Slug string }
		_ = json.Unmarshal([]byte(matched[len(matched)-1]), &last)
		next = fmt.Sprintf("%q", last.Slug)
	}
	nf, _ := json.Marshal(append([]string{}, notFound...))
	_, _ = fmt.Fprintf(w, `{"packs":[%s],"next_cursor":%s,"not_found":%s}`, strings.Join(matched, ","), next, nf)
}

func (a *packsAPI) serveVersions(w http.ResponseWriter, r *http.Request) {
	var found []string
	notFound := []string{}
	for _, id := range r.URL.Query()["pack_version_ids"] {
		if v, ok := fakeVersions[id]; ok {
			found = append(found, v)
		} else {
			notFound = append(notFound, id)
		}
	}
	nf, _ := json.Marshal(notFound)
	_, _ = fmt.Fprintf(w, `{"versions":[%s],"not_found":%s}`, strings.Join(found, ","), nf)
}

func (a *packsAPI) set(f func(a *packsAPI)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f(a)
}

func (a *packsAPI) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.listCalls...)
}

func packsConfig(endpoint, body string) string {
	return fmt.Sprintf(`
provider "botyard" {
  endpoint = %q
  api_key  = "byk_test"
  org_id   = "org-1"
}
%s
`, endpoint, body)
}

func packsUnitTest(t *testing.T, steps ...resource.TestStep) {
	t.Helper()
	resource.UnitTest(t, resource.TestCase{ProtoV6ProviderFactories: testAccProtoV6ProviderFactories, Steps: steps})
}

// --- botyard_packs ----------------------------------------------------------

func TestPacksDataSource_WholeCatalogueAcrossPages(t *testing.T) {
	api, srv := newPacksAPI(t)
	api.set(func(a *packsAPI) { a.pageSize = 2 })
	packsUnitTest(t, resource.TestStep{
		Config: packsConfig(srv.URL, `data "botyard_packs" "all" {}`),
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("data.botyard_packs.all", "packs.#", "3"),
			resource.TestCheckResourceAttr("data.botyard_packs.all", "packs.0.slug", "crm"),
			resource.TestCheckResourceAttr("data.botyard_packs.all", "packs.0.pack_id", "pk-crm"),
			resource.TestCheckResourceAttr("data.botyard_packs.all", "packs.0.visibility", "curated"),
			resource.TestCheckNoResourceAttr("data.botyard_packs.all", "packs.0.publisher_org_id"),
			resource.TestCheckResourceAttr("data.botyard_packs.all", "packs.0.latest.pack_version_id", pvCRM),
			resource.TestCheckResourceAttr("data.botyard_packs.all", "packs.0.latest.version_no", "3"),
			resource.TestCheckResourceAttr("data.botyard_packs.all", "packs.0.latest.created_at", "2026-10-20T10:00:00Z"),
			resource.TestCheckResourceAttr("data.botyard_packs.all", "packs.2.slug", "support"),
			resource.TestCheckResourceAttr("data.botyard_packs.all", "packs.2.publisher_org_id", "org-pub"),
			resource.TestCheckResourceAttr("data.botyard_packs.all", "not_found.#", "0"),
			func(*terraform.State) error {
				// Two pages per read; the second carries the first's cursor.
				calls := api.calls()
				if len(calls) < 2 || !strings.Contains(calls[1], "cursor=empty") {
					return fmt.Errorf("list calls = %v, want a second page with cursor=empty", calls)
				}
				if strings.Contains(calls[0], "slugs=") {
					return fmt.Errorf("unfiltered read sent slugs: %q", calls[0])
				}
				return nil
			},
		),
	})
}

func TestPacksDataSource_SlugFilterAndNotFound(t *testing.T) {
	api, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config: packsConfig(srv.URL, `data "botyard_packs" "some" { slugs = ["support", "nope", "crm"] }`),
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("data.botyard_packs.some", "packs.#", "2"),
			resource.TestCheckResourceAttr("data.botyard_packs.some", "packs.0.slug", "crm"),
			resource.TestCheckResourceAttr("data.botyard_packs.some", "packs.1.slug", "support"),
			resource.TestCheckResourceAttr("data.botyard_packs.some", "not_found.#", "1"),
			resource.TestCheckResourceAttr("data.botyard_packs.some", "not_found.0", "nope"),
			func(*terraform.State) error {
				q := api.calls()[0]
				for _, s := range []string{"slugs=crm", "slugs=nope", "slugs=support"} {
					if !strings.Contains(q, s) {
						return fmt.Errorf("query %q missing %s", q, s)
					}
				}
				return nil
			},
		),
	})
}

func TestPacksDataSource_EmptyCatalogue(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config: packsConfig(srv.URL, `data "botyard_packs" "none" { slugs = ["nope"] }`),
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("data.botyard_packs.none", "packs.#", "0"),
			resource.TestCheckResourceAttr("data.botyard_packs.none", "not_found.#", "1"),
		),
	})
}

func TestPacksDataSource_EmptySlugsRejected(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, `data "botyard_packs" "x" { slugs = [] }`),
		ExpectError: regexp.MustCompile(`set must contain at least 1 elements`),
	})
}

func TestPacksDataSource_HTTPError(t *testing.T) {
	api, srv := newPacksAPI(t)
	api.set(func(a *packsAPI) { a.failWith = http.StatusNotFound })
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, `data "botyard_packs" "all" {}`),
		ExpectError: regexp.MustCompile(`HTTP 404: organization org-1 not found`),
	})
}

// --- botyard_pack -----------------------------------------------------------

func TestPackDataSource_BySlug(t *testing.T) {
	api, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config: packsConfig(srv.URL, `data "botyard_pack" "crm" { slug = "crm" }`),
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("data.botyard_pack.crm", "pack_id", "pk-crm"),
			resource.TestCheckResourceAttr("data.botyard_pack.crm", "latest.pack_version_id", pvCRM),
			resource.TestCheckResourceAttr("data.botyard_pack.crm", "latest.name", "crm v3"),
			resource.TestCheckResourceAttr("data.botyard_pack.crm", "latest.content_hash", "h-"+pvCRM),
			func(*terraform.State) error {
				if q := api.calls()[0]; !strings.Contains(q, "slugs=crm") {
					return fmt.Errorf("lookup did not filter by slug: %q", q)
				}
				return nil
			},
		),
	})
}

func TestPackDataSource_NotFound(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, `data "botyard_pack" "x" { slug = "retired" }`),
		ExpectError: regexp.MustCompile(`No pack with slug "retired"`),
	})
}

func TestPackDataSource_HTTPError(t *testing.T) {
	api, srv := newPacksAPI(t)
	api.set(func(a *packsAPI) { a.failWith = http.StatusNotFound })
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, `data "botyard_pack" "x" { slug = "crm" }`),
		ExpectError: regexp.MustCompile(`HTTP 404`),
	})
}

// --- botyard_pack_version ---------------------------------------------------

func TestPackVersionDataSource_ByID(t *testing.T) {
	_, srv := newPacksAPI(t)
	const n = "data.botyard_pack_version.v"
	packsUnitTest(t, resource.TestStep{
		Config: packsConfig(srv.URL, fmt.Sprintf(`data "botyard_pack_version" "v" { pack_version_id = %q }`, pvCRM)),
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr(n, "slug", "crm"),
			resource.TestCheckResourceAttr(n, "version_no", "3"),
			resource.TestCheckResourceAttr(n, "is_latest", "true"),
			resource.TestCheckResourceAttr(n, "pack_archived", "false"),
			resource.TestCheckResourceAttr(n, "start_with_default_tools", "false"),
			resource.TestCheckResourceAttr(n, "soul", "You are crm."),
			resource.TestCheckNoResourceAttr(n, "changelog"),
			resource.TestCheckResourceAttr(n, "recommended_model.provider", "botyard"),
			resource.TestCheckResourceAttr(n, "recommended_model.model", "claude-sonnet-5-5"),
			// settings: set fields carried, unset fields null.
			resource.TestCheckResourceAttr(n, "settings.thinking_default", "high"),
			resource.TestCheckNoResourceAttr(n, "settings.reasoning_default"),
			resource.TestCheckResourceAttr(n, "settings.tool_search.mode", "auto"),
			resource.TestCheckResourceAttr(n, "settings.tool_search.auto_threshold_percent", "20"),
			resource.TestCheckNoResourceAttr(n, "settings.tool_images.enabled"),
			// skills
			resource.TestCheckResourceAttr(n, "skills.#", "2"),
			resource.TestCheckResourceAttr(n, "skills.0.skill_id", "sk-1"),
			resource.TestCheckResourceAttr(n, "skills.0.skill_version_id", "skv-1"),
			resource.TestCheckResourceAttr(n, "skills.0.version_no", "2"),
			resource.TestCheckResourceAttr(n, "skills.0.required", "true"),
			resource.TestCheckResourceAttr(n, "skills.1.required", "false"),
			resource.TestCheckTypeSetElemAttr(n, "skill_ids.*", "sk-1"),
			resource.TestCheckTypeSetElemAttr(n, "skill_ids.*", "sk-2"),
			// tools: every runtime variant dispatched.
			resource.TestCheckResourceAttr(n, "tools.#", "3"),
			resource.TestCheckResourceAttr(n, "tools.0.runtime", "mcp"),
			resource.TestCheckResourceAttr(n, "tools.0.mcp_server", "hubspot"),
			resource.TestCheckNoResourceAttr(n, "tools.0.title"),
			resource.TestCheckResourceAttr(n, "tools.1.runtime", "openclaw"),
			resource.TestCheckResourceAttr(n, "tools.1.title", "Web fetch"),
			resource.TestCheckNoResourceAttr(n, "tools.1.mcp_server"),
			resource.TestCheckResourceAttr(n, "tools.2.runtime", "native"),
			resource.TestCheckResourceAttr(n, "tools.2.tool_id", "t-nat"),
			resource.TestCheckResourceAttr(n, "tool_ids.#", "3"),
			resource.TestCheckTypeSetElemAttr(n, "tool_ids.*", "t-mcp"),
			resource.TestCheckTypeSetElemAttr(n, "tool_ids.*", "t-claw"),
			resource.TestCheckTypeSetElemAttr(n, "tool_ids.*", "t-nat"),
			// connections: exactly one variant block per kind.
			resource.TestCheckResourceAttr(n, "connections.#", "3"),
			resource.TestCheckResourceAttr(n, "connections.0.kind", "mcp_catalog_entry"),
			resource.TestCheckResourceAttr(n, "connections.0.purpose", "CRM access"),
			resource.TestCheckResourceAttr(n, "connections.0.mcp_catalog_entry.slug", "hubspot"),
			resource.TestCheckResourceAttr(n, "connections.0.mcp_catalog_entry.tool_names.#", "2"),
			resource.TestCheckNoResourceAttr(n, "connections.0.vault_secret"),
			resource.TestCheckNoResourceAttr(n, "connections.1.mcp_catalog_entry.tool_names"),
			resource.TestCheckNoResourceAttr(n, "connections.1.purpose"),
			resource.TestCheckResourceAttr(n, "connections.2.kind", "vault_secret"),
			resource.TestCheckResourceAttr(n, "connections.2.vault_secret.key_path", "crm.api_key"),
			resource.TestCheckResourceAttr(n, "connections.2.vault_secret.sensitivity", "secret"),
			resource.TestCheckResourceAttr(n, "connections.2.vault_secret.max_ttl_seconds", "600"),
			resource.TestCheckNoResourceAttr(n, "connections.2.mcp_catalog_entry"),
		),
	})
}

func TestPackVersionDataSource_BySlugIsLatest(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config: packsConfig(srv.URL, `data "botyard_pack_version" "v" { slug = "crm" }`),
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("data.botyard_pack_version.v", "pack_version_id", pvCRM),
			resource.TestCheckResourceAttr("data.botyard_pack_version.v", "version_no", "3"),
			resource.TestCheckResourceAttr("data.botyard_pack_version.v", "is_latest", "true"),
		),
	})
}

func TestPackVersionDataSource_OlderVersionByID(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config: packsConfig(srv.URL, fmt.Sprintf(`data "botyard_pack_version" "v" { pack_version_id = %q }`, pvCRMOld)),
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("data.botyard_pack_version.v", "version_no", "1"),
			resource.TestCheckResourceAttr("data.botyard_pack_version.v", "is_latest", "false"),
			resource.TestCheckResourceAttr("data.botyard_pack_version.v", "slug", "crm"),
		),
	})
}

func TestPackVersionDataSource_ArchivedPack(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config: packsConfig(srv.URL, fmt.Sprintf(`data "botyard_pack_version" "v" { pack_version_id = %q }`, pvArchived)),
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("data.botyard_pack_version.v", "pack_archived", "true"),
			resource.TestCheckResourceAttr("data.botyard_pack_version.v", "slug", "retired"),
			resource.TestCheckResourceAttr("data.botyard_pack_version.v", "skill_ids.#", "1"),
		),
	})
}

func TestPackVersionDataSource_EmptyLists(t *testing.T) {
	_, srv := newPacksAPI(t)
	const n = "data.botyard_pack_version.v"
	packsUnitTest(t, resource.TestStep{
		Config: packsConfig(srv.URL, fmt.Sprintf(`data "botyard_pack_version" "v" { pack_version_id = %q }`, pvEmpty)),
		Check: resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr(n, "skills.#", "0"),
			resource.TestCheckResourceAttr(n, "tools.#", "0"),
			resource.TestCheckResourceAttr(n, "connections.#", "0"),
			// Empty, not null: setunion() over them must work.
			resource.TestCheckResourceAttr(n, "skill_ids.#", "0"),
			resource.TestCheckResourceAttr(n, "tool_ids.#", "0"),
			resource.TestCheckNoResourceAttr(n, "recommended_model"),
			resource.TestCheckNoResourceAttr(n, "settings.tool_search"),
		),
	})
}

func TestPackVersionDataSource_NotFound(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, `data "botyard_pack_version" "v" { pack_version_id = "99999999-9999-4999-8999-999999999999" }`),
		ExpectError: regexp.MustCompile(`Pack version not found`),
	})
}

func TestPackVersionDataSource_SlugNotFound(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, `data "botyard_pack_version" "v" { slug = "retired" }`),
		ExpectError: regexp.MustCompile(`No pack with slug "retired"`),
	})
}

func TestPackVersionDataSource_HTTPError(t *testing.T) {
	api, srv := newPacksAPI(t)
	api.set(func(a *packsAPI) { a.failWith = http.StatusNotFound })
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, fmt.Sprintf(`data "botyard_pack_version" "v" { pack_version_id = %q }`, pvCRM)),
		ExpectError: regexp.MustCompile(`HTTP 404`),
	})
}

func TestPackVersionDataSource_MalformedID(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, `data "botyard_pack_version" "v" { pack_version_id = "not-a-uuid" }`),
		ExpectError: regexp.MustCompile(`"not-a-uuid" is not a UUID`),
	})
}

func TestPackVersionDataSource_ExactlyOneAddress(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, `data "botyard_pack_version" "v" {}`),
		ExpectError: regexp.MustCompile(`(?s)Exactly one of these attributes must be configured`),
	}, resource.TestStep{
		Config: packsConfig(srv.URL, fmt.Sprintf(`data "botyard_pack_version" "v" {
  slug            = "crm"
  pack_version_id = %q
}`, pvCRM)),
		ExpectError: regexp.MustCompile(`(?s)Exactly one of these attributes must be configured`),
	})
}

// Unknown discriminators must fail loudly, never panic and never drop the item.
func TestPackVersionDataSource_UnknownToolRuntime(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, fmt.Sprintf(`data "botyard_pack_version" "v" { pack_version_id = %q }`, pvBadTool)),
		ExpectError: regexp.MustCompile(`(?s)Unsupported pack tool.*tools\[1\].*unknown runtime "wasm".*Upgrade the Botyard provider`),
	})
}

func TestPackVersionDataSource_MissingToolRuntime(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, fmt.Sprintf(`data "botyard_pack_version" "v" { pack_version_id = %q }`, pvNoRT)),
		ExpectError: regexp.MustCompile(`(?s)Unsupported pack tool.*has no "runtime" discriminator`),
	})
}

func TestPackVersionDataSource_UnknownConnectionKind(t *testing.T) {
	_, srv := newPacksAPI(t)
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, fmt.Sprintf(`data "botyard_pack_version" "v" { pack_version_id = %q }`, pvBadConn)),
		ExpectError: regexp.MustCompile(`(?s)Unsupported pack connection.*unknown kind "oauth_app"`),
	})
}

// The fixtures must stay in the API's slug order for the pagination fake.
func TestPackFixturesSorted(t *testing.T) {
	if !sort.SliceIsSorted(fakeCatalogue, func(i, j int) bool { return fakeCatalogue[i].slug < fakeCatalogue[j].slug }) {
		t.Fatal("fakeCatalogue must be in slug order")
	}
}

// A server whose cursor never advances must fail the read, not loop forever.
func TestListPacks_StuckCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"packs":[%s],"next_cursor":"same","not_found":[]}`, fakeCatalogue[0].json)
	}))
	defer srv.Close()
	packsUnitTest(t, resource.TestStep{
		Config:      packsConfig(srv.URL, `data "botyard_packs" "all" {}`),
		ExpectError: regexp.MustCompile(`cursor that does not advance`),
	})
}
