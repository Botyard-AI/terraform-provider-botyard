package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// This fake mirrors the scoped file API: PUT appends a revision, GET returns
// current revisions, and no DELETE exists. Every write must be soul-only.
type soulAPI struct {
	mu                                 sync.Mutex
	content                            string
	version                            int
	writes                             int
	harness                            string
	hosting                            string
	botStatus, readStatus, writeStatus int
	noSoul, duplicate, badWrite        bool
}

func newSoulAPI(t *testing.T) (*soulAPI, *httptest.Server, *BotSoulResource) {
	t.Helper()
	api := &soulAPI{content: "platform default", version: 1, harness: "botyard_native", hosting: "hosted", botStatus: 200, readStatus: 200, writeStatus: 201}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/v1/orgs/org-1/bots/bot-1" && req.Method == http.MethodGet {
			w.WriteHeader(api.botStatus)
			_, _ = fmt.Fprintf(w, `{"harness":%q,"hosting_type":%q}`, api.harness, api.hosting)
			return
		}
		if req.URL.Path != "/v1/orgs/org-1/bots/bot-1/files" {
			t.Errorf("unexpected path %s", req.URL.Path)
			w.WriteHeader(404)
			return
		}
		switch req.Method {
		case http.MethodGet:
			w.WriteHeader(api.readStatus)
		case http.MethodPut:
			var body client.BotFileUpdateRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body.Files) != 1 || body.Files[0].FileType != client.BotFileTypeSoul {
				t.Errorf("not a soul-only batch: %#v", body)
				w.WriteHeader(422)
				return
			}
			api.writes++
			w.WriteHeader(api.writeStatus)
			if api.writeStatus != 201 {
				_, _ = w.Write([]byte(`{"detail":"private soul content must not leak"}`))
				return
			}
			api.noSoul = false
			api.content = body.Files[0].Content
			api.version++
		default:
			t.Errorf("unexpected method %s", req.Method)
			w.WriteHeader(405)
			return
		}
		files := []client.BotFileResponse{{FileType: client.BotFileTypeSoul, Content: api.content, Version: api.version, DeployStatus: client.DeployStatusDeployed}}
		if api.noSoul || (api.badWrite && req.Method == http.MethodPut) {
			files = nil
		}
		if api.duplicate {
			files = append(files, files[0])
		}
		// Include another file to prove that read ignores it and PUT never sends it.
		files = append(files, client.BotFileResponse{FileType: client.BotFileTypeUser, Content: "user profile", Version: 1})
		_ = json.NewEncoder(w).Encode(client.BotFileListResponse{Files: files})
	}))
	t.Cleanup(srv.Close)
	c, err := client.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return api, srv, &BotSoulResource{data: &providerData{client: c, orgID: "org-1"}}
}

func soulPlan(content string) BotSoulResourceModel {
	return BotSoulResourceModel{BotSlug: types.StringValue("bot-1"), Content: types.StringValue(content)}
}

func TestBotSoulApply(t *testing.T) {
	for _, tc := range []struct {
		name       string
		content    string
		noSoul     bool
		wantWrites int
	}{
		{"adopt unchanged", "platform default", false, 0},
		{"replace", "# Soul\nBe useful. 🛠️\n", false, 1},
		{"clear", "", false, 1},
		{"missing soul", "new", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, _, r := newSoulAPI(t)
			api.noSoul = tc.noSoul
			p := soulPlan(tc.content)
			var d diag.Diagnostics
			r.apply(context.Background(), &p, &d)
			if d.HasError() {
				t.Fatal(d)
			}
			if api.writes != tc.wantWrites || p.ID.ValueString() != "bot-1" || api.content != tc.content {
				t.Fatalf("writes=%d plan=%#v content=%q", api.writes, p, api.content)
			}
			if p.Version.ValueInt64() != int64(api.version) {
				t.Fatal("wrong revision")
			}
			// A retry after an uncertain/committed write does not create another revision.
			api.noSoul = false
			r.apply(context.Background(), &p, &d)
			if d.HasError() || api.writes != tc.wantWrites {
				t.Fatalf("retry: %v, writes=%d", d, api.writes)
			}
		})
	}
}

func TestBotSoulFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*soulAPI)
		want   string
		writes int
	}{
		{"wrong harness", func(a *soulAPI) { a.harness = "openclaw" }, "Unsupported bot runtime", 0},
		{"wrong hosting", func(a *soulAPI) { a.hosting = "self_hosted" }, "Unsupported bot runtime", 0},
		{"bot missing", func(a *soulAPI) { a.botStatus = 404 }, "HTTP 404", 0},
		{"bot forbidden", func(a *soulAPI) { a.botStatus = 403 }, "HTTP 403", 0},
		{"read forbidden", func(a *soulAPI) { a.readStatus = 403 }, "HTTP 403", 0},
		{"files missing", func(a *soulAPI) { a.readStatus = 404 }, "HTTP 404", 0},
		{"read server failure", func(a *soulAPI) { a.readStatus = 500 }, "HTTP 500", 0},
		{"duplicate soul", func(a *soulAPI) { a.duplicate = true }, "multiple current", 0},
		{"write forbidden", func(a *soulAPI) { a.writeStatus = 403 }, "HTTP 403", 1},
		{"write validation failure", func(a *soulAPI) { a.writeStatus = 422 }, "HTTP 422", 1},
		{"write server failure", func(a *soulAPI) { a.writeStatus = 500 }, "HTTP 500", 1},
		{"incomplete write response", func(a *soulAPI) { a.badWrite = true }, "may have committed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, _, r := newSoulAPI(t)
			tc.setup(api)
			p := soulPlan("desired")
			var d diag.Diagnostics
			r.apply(context.Background(), &p, &d)
			got := fmt.Sprint(d)
			if !d.HasError() || !strings.Contains(got, tc.want) || strings.Contains(got, "private soul") || api.writes != tc.writes {
				t.Fatalf("diags=%s writes=%d", got, api.writes)
			}
		})
	}
}

func TestBotSoulRead(t *testing.T) {
	for _, tc := range []struct {
		name             string
		setup            func(*soulAPI)
		removed, failure bool
	}{
		{"drift", func(a *soulAPI) { a.content = "outside edit"; a.version = 4 }, false, false},
		{"missing bot", func(a *soulAPI) { a.botStatus = 404 }, true, false},
		{"missing files", func(a *soulAPI) { a.readStatus = 404 }, true, false},
		{"missing soul", func(a *soulAPI) { a.noSoul = true }, true, false},
		{"forbidden", func(a *soulAPI) { a.readStatus = 403 }, false, true},
		{"failed", func(a *soulAPI) { a.readStatus = 500 }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, _, r := newSoulAPI(t)
			tc.setup(api)
			ctx := context.Background()
			var sch fwresource.SchemaResponse
			r.Schema(ctx, fwresource.SchemaRequest{}, &sch)
			state := tfsdk.State{Schema: sch.Schema}
			p := soulPlan("prior")
			p.ID = p.BotSlug
			p.Version = types.Int64Value(1)
			if d := state.Set(ctx, &p); d.HasError() {
				t.Fatal(d)
			}
			resp := fwresource.ReadResponse{State: state}
			r.Read(ctx, fwresource.ReadRequest{State: state}, &resp)
			if resp.Diagnostics.HasError() != tc.failure {
				t.Fatal(resp.Diagnostics)
			}
			if tc.failure {
				return
			}
			if resp.State.Raw.IsNull() != tc.removed {
				t.Fatalf("removed=%v", resp.State.Raw.IsNull())
			}
			if !tc.removed {
				var got BotSoulResourceModel
				if d := resp.State.Get(ctx, &got); d.HasError() {
					t.Fatal(d)
				}
				if got.Content.ValueString() != api.content || got.Version.ValueInt64() != int64(api.version) {
					t.Fatalf("wrong read %#v", got)
				}
			}
			if api.writes != 0 {
				t.Fatal("refresh wrote files")
			}
		})
	}
}

func TestBotSoulTransportFailure(t *testing.T) {
	_, srv, r := newSoulAPI(t)
	srv.Close()
	p := soulPlan("desired")
	var d diag.Diagnostics
	r.apply(context.Background(), &p, &d)
	if !d.HasError() {
		t.Fatal("missing transport error")
	}
}

func soulTestConfig(endpoint, contentExpr string) string {
	return fmt.Sprintf(`provider "botyard" {
 endpoint = %q
 api_key = "test-key"
 org_id = "org-1"
}
resource "botyard_bot_soul" "test" {
 bot_slug = "bot-1"
 content = %s
}`, endpoint, contentExpr)
}

func TestBotSoulTerraformLifecycle(t *testing.T) {
	api, srv, _ := newSoulAPI(t)
	f := filepath.Join(t.TempDir(), "SOUL.md")
	soul := "# Soul\nBe useful. 🛠️\n"
	if err := os.WriteFile(f, []byte(soul), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := soulTestConfig(srv.URL, fmt.Sprintf("file(%q)", f))
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(_ *terraform.State) error {
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.content != "" || api.writes != 4 {
				return fmt.Errorf("destroy changed soul or unexpected writes: content=%q writes=%d", api.content, api.writes)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{Config: cfg, Check: resource.ComposeTestCheckFunc(resource.TestCheckResourceAttr("botyard_bot_soul.test", "content", soul), resource.TestCheckResourceAttr("botyard_bot_soul.test", "version", "2"))},
			{Config: cfg, PlanOnly: true},
			{ResourceName: "botyard_bot_soul.test", ImportState: true, ImportStateId: "bot-1", ImportStateVerify: true},
			{PreConfig: func() { api.mu.Lock(); defer api.mu.Unlock(); api.content = "outside edit"; api.version++ }, Config: cfg, Check: resource.TestCheckResourceAttr("botyard_bot_soul.test", "content", soul)},
			{Config: soulTestConfig(srv.URL, `"updated"`)},
			{Config: soulTestConfig(srv.URL, `""`), Check: resource.TestCheckResourceAttr("botyard_bot_soul.test", "content", "")},
		},
	})
}

func TestBotSoulTerraformRejectsOversize(t *testing.T) {
	_, srv, _ := newSoulAPI(t)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps:                    []resource.TestStep{{Config: soulTestConfig(srv.URL, fmt.Sprintf("%q", strings.Repeat("x", 102401))), ExpectError: regexp.MustCompile("102400")}},
	})
}

func TestBotSoulDeletePreservesInstructions(t *testing.T) {
	api, _, r := newSoulAPI(t)
	r.Delete(context.Background(), fwresource.DeleteRequest{}, &fwresource.DeleteResponse{})
	if api.content != "platform default" || api.writes != 0 {
		t.Fatal("destroy modified the live soul")
	}
}
