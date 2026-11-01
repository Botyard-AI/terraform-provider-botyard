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
	"unicode"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
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
	malformedOn                        string
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
			if api.malformedOn == "bot" {
				_, _ = w.Write([]byte(`{"created_at":"private-soul-marker"}`))
				return
			}
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
			// Mirror write_native_soul's native-only nonblank invariant.
			if strings.TrimFunc(body.Files[0].Content, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }) == "" {
				w.WriteHeader(422)
				_, _ = w.Write([]byte(`{"detail":"A native agent's soul cannot be empty."}`))
				return
			}
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
		if (api.malformedOn == "read" && req.Method == http.MethodGet) || (api.malformedOn == "write" && req.Method == http.MethodPut) {
			_, _ = w.Write([]byte(`{"files":[{"file_type":"soul","created_at":"private-soul-marker"}]}`))
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
		{"keep surrounding whitespace", " \n🛠️\t ", false, 1},
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
			if api.content != "updated" || api.writes != 3 {
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

func TestNativeSoulContentValidator(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   types.String
		invalid bool
	}{
		{"empty", types.StringValue(""), true},
		{"ascii whitespace", types.StringValue(" \t\n\r\v\f"), true},
		{"python separators", types.StringValue("\u001c\u001d\u001e\u001f"), true},
		{"unicode whitespace", types.StringValue("\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"), true},
		{"text preserved", types.StringValue(" \n# Soul 🛠️\t "), false},
		{"zero width not Python whitespace", types.StringValue("\u200b\ufeff"), false},
		{"unknown deferred", types.StringUnknown(), false},
		{"null handled by required schema", types.StringNull(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resp validator.StringResponse
			nativeSoulContentValidator{}.ValidateString(context.Background(), validator.StringRequest{Path: path.Root("content"), ConfigValue: tc.value}, &resp)
			if resp.Diagnostics.HasError() != tc.invalid {
				t.Fatalf("diagnostics: %v", resp.Diagnostics)
			}
		})
	}
}

func TestBotSoulRejectsBlankWithoutWriting(t *testing.T) {
	for _, content := range []string{"", " \t\n", "\u00a0\u001c"} {
		t.Run(fmt.Sprintf("%q", content), func(t *testing.T) {
			api, _, r := newSoulAPI(t)
			p := soulPlan(content)
			var d diag.Diagnostics
			r.apply(context.Background(), &p, &d)
			if !d.HasError() || api.writes != 0 || api.content != "platform default" {
				t.Fatalf("diags=%v writes=%d", d, api.writes)
			}
			// Bypass provider validation to prove the fake mirrors native API refusal.
			resp, err := r.data.client.UpdateBotFilesV1OrgsOrgIdBotsBotSlugFilesPutWithResponse(context.Background(), "org-1", "bot-1", client.BotFileUpdateRequest{Files: []client.BotFileEntry{{FileType: client.BotFileTypeSoul, Content: content}}})
			if err != nil || resp.StatusCode() != 422 || api.content != "platform default" || api.version != 1 {
				t.Fatalf("blank API write: resp=%v err=%v", resp, err)
			}
		})
	}
}

func TestBotSoulTerraformRejectsBlank(t *testing.T) {
	for _, content := range []string{"", " \t\n", "\u00a0\u001c"} {
		t.Run(fmt.Sprintf("%q", content), func(t *testing.T) {
			api, srv, _ := newSoulAPI(t)
			expr, err := json.Marshal(content)
			if err != nil {
				t.Fatal(err)
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps:                    []resource.TestStep{{Config: soulTestConfig(srv.URL, string(expr)), ExpectError: regexp.MustCompile("must contain non-whitespace text")}},
			})
			if api.writes != 0 {
				t.Fatal("blank plan reached the write API")
			}
		})
	}
}

func TestBotSoulDecoderErrorsDoNotLeak(t *testing.T) {
	for _, operation := range []string{"bot", "read", "write"} {
		t.Run(operation, func(t *testing.T) {
			api, _, r := newSoulAPI(t)
			api.malformedOn = operation
			ctx := context.Background()
			var err error
			// Positive control: the generated client's actual decoder error echoes
			// the untrusted timestamp. Do not accidentally test a harmless error.
			switch operation {
			case "bot":
				_, err = r.data.client.GetBotV1OrgsOrgIdBotsBotSlugGetWithResponse(ctx, "org-1", "bot-1")
			case "read":
				_, err = r.data.client.ListBotFilesV1OrgsOrgIdBotsBotSlugFilesGetWithResponse(ctx, "org-1", "bot-1")
			case "write":
				_, err = r.data.client.UpdateBotFilesV1OrgsOrgIdBotsBotSlugFilesPutWithResponse(ctx, "org-1", "bot-1", client.BotFileUpdateRequest{Files: []client.BotFileEntry{{FileType: client.BotFileTypeSoul, Content: "probe"}}})
			}
			if err == nil || !strings.Contains(err.Error(), "private-soul-marker") {
				t.Fatalf("positive control failed: %v", err)
			}
			p := soulPlan("desired")
			var d diag.Diagnostics
			r.apply(ctx, &p, &d)
			if !d.HasError() || strings.Contains(fmt.Sprint(d), "private-soul-marker") {
				t.Fatalf("unsafe diagnostics: %v", d)
			}
		})
	}
}
