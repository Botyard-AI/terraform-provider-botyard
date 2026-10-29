package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	tfresource "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

const warnBotID = "11111111-1111-4111-8111-111111111111"

// packWarnAPI fakes the reads the strip warning makes (GET bot, GET installs)
// plus the skill and tool assignment endpoints for the end-to-end test.
type packWarnAPI struct {
	mu            sync.Mutex
	botStatus     int    // GET /bots/bot-1 status; 200 by default
	installsFail  bool   // GET /packs/installs returns 500
	items         string // JSON array of install items
	skills        map[string]bool
	tools         []string
	botReads      int
	installsReads int
	unexpected    []string // requests the fake does not serve; failed in t.Cleanup
}

// fakeAssignedSkill / fakeAssignedTool are the list rows the fake returns.
// They are encoded with encoding/json, never concatenated from request data.
type fakeAssignedSkill struct {
	AssignedAt   string `json:"assigned_at"`
	DeployStatus string `json:"deploy_status"`
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	SkillID      string `json:"skill_id"`
	Slug         string `json:"slug"`
	Summary      string `json:"summary"`
}

type fakeAssignedTool struct {
	AssignedAt      string `json:"assigned_at"`
	Domain          string `json:"domain"`
	Name            string `json:"name"`
	Runtime         string `json:"runtime"`
	RuntimeToolName string `json:"runtime_tool_name"`
	Slug            string `json:"slug"`
	SyncStatus      string `json:"sync_status"`
	ToolID          string `json:"tool_id"`
}

func fakeAssignedTools(ids []string) []fakeAssignedTool {
	out := make([]fakeAssignedTool, 0, len(ids))
	for _, id := range ids {
		out = append(out, fakeAssignedTool{AssignedAt: "2026-10-01T00:00:00Z", Domain: "d", Name: "n",
			Runtime: "native", RuntimeToolName: "n", Slug: "s", SyncStatus: "synced", ToolID: id})
	}
	return out
}

// checkUnexpected fails t if the fake saw a request it does not serve.
func (a *packWarnAPI) checkUnexpected(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range a.unexpected {
		t.Errorf("fake API: %s", u)
	}
}

func newPackWarnAPI(items ...string) *packWarnAPI {
	return &packWarnAPI{botStatus: 200, items: "[" + strings.Join(items, ",") + "]", skills: map[string]bool{}}
}

func skillItem(skillID string, live bool, origin string) string {
	bs := `null`
	if live {
		bs = `"bs-` + skillID + `"`
	}
	return fmt.Sprintf(`{"kind":"skill","bot_pack_item_id":"i-%s","origin":%q,"created_at":"2026-10-01T00:00:00Z",`+
		`"bot_skill_id":%s,"skill_id":%q}`, skillID, origin, bs, skillID)
}

func toolItem(toolID string, live bool) string {
	bt := `null`
	if live {
		bt = `"bt-` + toolID + `"`
	}
	return fmt.Sprintf(`{"kind":"tool","bot_pack_item_id":"i-%s","origin":"added","created_at":"2026-10-01T00:00:00Z",`+
		`"bot_tool_id":%s,"tool_catalog_id":%q}`, toolID, bt, toolID)
}

func (a *packWarnAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	jsonOK := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
	switch p := r.URL.Path; {
	case p == "/v1/orgs/org-1/bots/bot-1" && r.Method == http.MethodGet:
		a.botReads++
		if a.botStatus != 200 {
			jsonOK(a.botStatus, `{"title":"nope","status":`+fmt.Sprint(a.botStatus)+`}`)
			return
		}
		jsonOK(200, `{"id":"`+warnBotID+`","slug":"bot-1","name":"Bot"}`)
	case p == "/v1/orgs/org-1/packs/installs" && r.Method == http.MethodGet:
		a.installsReads++
		if a.installsFail {
			jsonOK(500, `{"title":"boom","status":500}`)
			return
		}
		if r.URL.Query().Get("bot_ids") != warnBotID {
			a.unexpected = append(a.unexpected, fmt.Sprintf("installs read with bot_ids %q, want %q",
				r.URL.Query().Get("bot_ids"), warnBotID))
			jsonOK(400, `{"title":"wrong bot_ids","status":400}`)
			return
		}
		jsonOK(200, `{"bots":[{"bot_id":"`+warnBotID+`","installs":[{"bot_pack_install_id":"in-1",`+
			`"connections":[],"installed_at":"2026-10-01T00:00:00Z","items":`+a.items+`,"name":"Sales",`+
			`"pack_id":"p-1","pack_slug":"sales","pack_version_id":"pv-1","pending_connections":[],`+
			`"updated_at":"2026-10-01T00:00:00Z","version_no":1}]}],"not_found":[]}`)
	case p == "/v1/orgs/org-1/bots/bot-1/skills":
		a.serveSkills(w, r, jsonOK)
	case p == "/v1/orgs/org-1/bots/bot-1/tools":
		if r.Method == http.MethodPut {
			var body client.BotToolAssignRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			a.tools = body.ToolIds
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fakeAssignedTools(a.tools))
	default:
		a.unexpected = append(a.unexpected, fmt.Sprintf("unexpected request %s %s", r.Method, p))
		jsonOK(404, `{"title":"unexpected request","status":404}`)
	}
}

func (a *packWarnAPI) serveSkills(w http.ResponseWriter, r *http.Request, jsonOK func(int, string)) {
	switch r.Method {
	case http.MethodPut:
		var body client.BotSkillAssign
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, id := range body.SkillIds {
			a.skills[id] = true
		}
		jsonOK(201, "[]")
	case http.MethodDelete:
		var body client.BotSkillIds
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, id := range body.SkillIds {
			delete(a.skills, id)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		out := make([]fakeAssignedSkill, 0, len(a.skills))
		for id := range a.skills {
			out = append(out, fakeAssignedSkill{AssignedAt: "2026-10-01T00:00:00Z", DeployStatus: "deployed",
				Name: "n", Provider: "botyard", SkillID: id, Slug: "s"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func (a *packWarnAPI) reads() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.botReads, a.installsReads
}

func warnProviderData(t *testing.T, api *packWarnAPI) *providerData {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { api.checkUnexpected(t) })
	c, err := client.NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return &providerData{client: c, orgID: "org-1"}
}

// assignmentSet describes one side of a plan: absent (nil), or a bot slug and
// an id set. unknownSlug / unknownIDs / unknownElem model values only known
// at apply.
type assignmentSet struct {
	ids         []string
	unknownSlug bool
	unknownIDs  bool
	unknownElem bool
}

func assignmentRaw(t *testing.T, sch resource.SchemaResponse, attrName string, s *assignmentSet) tftypes.Value {
	t.Helper()
	ctx := context.Background()
	typ := sch.Schema.Type().TerraformType(ctx)
	if s == nil {
		return tftypes.NewValue(typ, nil)
	}
	st := tfsdk.State{Schema: sch.Schema, Raw: tftypes.NewValue(typ, nil)}
	var diags diag.Diagnostics
	slug := types.StringValue("bot-1")
	if s.unknownSlug {
		slug = types.StringUnknown()
	}
	elems := make([]attr.Value, 0, len(s.ids)+1)
	for _, id := range s.ids {
		elems = append(elems, types.StringValue(id))
	}
	if s.unknownElem {
		elems = append(elems, types.StringUnknown())
	}
	set := types.SetValueMust(types.StringType, elems)
	if s.unknownIDs {
		set = types.SetUnknown(types.StringType)
	}
	diags.Append(st.SetAttribute(ctx, path.Root("id"), types.StringValue("bot-1"))...)
	diags.Append(st.SetAttribute(ctx, path.Root("bot_slug"), slug)...)
	diags.Append(st.SetAttribute(ctx, path.Root(attrName), set)...)
	if diags.HasError() {
		t.Fatalf("building %s value: %v", attrName, diags)
	}
	return st.Raw
}

type planModifyingResource interface {
	resource.ResourceWithModifyPlan
}

func modifyPlanDiags(t *testing.T, r planModifyingResource, attrName string, state, plan *assignmentSet) diag.Diagnostics {
	t.Helper()
	ctx := context.Background()
	var sch resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sch)
	req := resource.ModifyPlanRequest{
		State: tfsdk.State{Schema: sch.Schema, Raw: assignmentRaw(t, sch, attrName, state)},
		Plan:  tfsdk.Plan{Schema: sch.Schema, Raw: assignmentRaw(t, sch, attrName, plan)},
	}
	resp := resource.ModifyPlanResponse{Plan: req.Plan}
	r.ModifyPlan(ctx, req, &resp)
	return resp.Diagnostics
}

func warningText(d diag.Diagnostics) string {
	var b strings.Builder
	for _, w := range d.Warnings() {
		b.WriteString(w.Summary() + "\n" + w.Detail() + "\n")
	}
	return b.String()
}

func TestPackStripWarning_Skills(t *testing.T) {
	items := []string{
		skillItem("sk-pack-a", true, "added"),
		skillItem("sk-pack-b", true, "adopted"),
		skillItem("sk-pack-gone", false, "added"), // already unassigned: not live
		toolItem("tl-pack", true),                 // a tool: not this resource's concern
		`{"kind":"setting","bot_pack_item_id":"i-s","origin":"added","created_at":"2026-10-01T00:00:00Z","path":"p"}`,
		`{"kind":"hologram","bot_pack_item_id":"i-h"}`, // unknown kind from a newer API: skipped
	}
	cases := []struct {
		name         string
		state, plan  *assignmentSet
		botStatus    int
		installsFail bool
		wantWarn     []string // substrings; nil means no warning
		wantNoWarn   []string
		wantReads    bool
	}{
		{
			name:       "create without the pack's skills warns",
			plan:       &assignmentSet{ids: []string{"sk-mine"}},
			wantWarn:   []string{"Plan removes pack-installed skills", "sk-pack-a", "sk-pack-b", `pack "sales"`, "setunion(data.botyard_pack_version.<name>.skill_ids, [...])"},
			wantNoWarn: []string{"sk-pack-gone", "tl-pack", "sk-mine"},
			wantReads:  true,
		},
		{
			name:       "update dropping one pack skill names only that one",
			state:      &assignmentSet{ids: []string{"sk-mine", "sk-pack-a", "sk-pack-b"}},
			plan:       &assignmentSet{ids: []string{"sk-mine", "sk-pack-b"}},
			wantWarn:   []string{"sk-pack-a"},
			wantNoWarn: []string{"sk-pack-b"},
			wantReads:  true,
		},
		{
			name:      "plan keeping every pack skill is quiet",
			plan:      &assignmentSet{ids: []string{"sk-mine", "sk-pack-a", "sk-pack-b"}},
			wantReads: true,
		},
		{
			name:  "no change makes no calls",
			state: &assignmentSet{ids: []string{"sk-mine"}},
			plan:  &assignmentSet{ids: []string{"sk-mine"}},
		},
		{
			name: "unknown set makes no calls",
			plan: &assignmentSet{unknownIDs: true},
		},
		{
			name: "unknown element makes no calls",
			plan: &assignmentSet{ids: []string{"sk-mine"}, unknownElem: true},
		},
		{
			name: "unknown bot slug makes no calls",
			plan: &assignmentSet{ids: []string{"sk-mine"}, unknownSlug: true},
		},
		{
			name:       "bot not created yet is quiet",
			plan:       &assignmentSet{ids: []string{"sk-mine"}},
			botStatus:  404,
			wantNoWarn: []string{"pack"},
		},
		{
			name:         "failed installs read warns that the check was skipped",
			plan:         &assignmentSet{ids: []string{"sk-mine"}},
			installsFail: true,
			wantWarn:     []string{"Could not check for pack-installed skills", "HTTP 500"},
			wantReads:    true,
		},
		{
			name:       "destroy unassigning pack skills warns",
			state:      &assignmentSet{ids: []string{"sk-mine", "sk-pack-a"}},
			wantWarn:   []string{"Destroying this assignment removes pack-installed skills", "sk-pack-a"},
			wantNoWarn: []string{"sk-pack-b", "sk-mine"},
			wantReads:  true,
		},
		{
			name:      "destroy without pack skills is quiet",
			state:     &assignmentSet{ids: []string{"sk-mine"}},
			wantReads: true,
		},
		{
			name:  "destroy of an empty set makes no calls",
			state: &assignmentSet{ids: []string{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newPackWarnAPI(items...)
			if tc.botStatus != 0 {
				api.botStatus = tc.botStatus
			}
			api.installsFail = tc.installsFail
			r := &BotSkillAssignmentResource{data: warnProviderData(t, api)}
			d := modifyPlanDiags(t, r, "skill_ids", tc.state, tc.plan)
			if d.HasError() {
				t.Fatalf("ModifyPlan must never fail a plan, got: %v", d)
			}
			got := warningText(d)
			if tc.wantWarn == nil && got != "" && len(tc.wantNoWarn) == 0 {
				t.Fatalf("expected no warning, got:\n%s", got)
			}
			for _, s := range tc.wantWarn {
				if !strings.Contains(got, s) {
					t.Errorf("warning missing %q:\n%s", s, got)
				}
			}
			for _, s := range tc.wantNoWarn {
				if strings.Contains(got, s) {
					t.Errorf("warning must not mention %q:\n%s", s, got)
				}
			}
			bots, installs := api.reads()
			if tc.wantReads && (bots != 1 || installs != 1) {
				t.Errorf("want 1 bot read and 1 installs read, got %d and %d", bots, installs)
			}
			if !tc.wantReads && installs != 0 {
				t.Errorf("want no installs read, got %d", installs)
			}
			if !tc.wantReads && tc.botStatus == 0 && bots != 0 {
				t.Errorf("want no bot read, got %d", bots)
			}
		})
	}
}

func TestPackStripWarning_Tools(t *testing.T) {
	api := newPackWarnAPI(toolItem("tl-pack", true), toolItem("tl-gone", false), skillItem("sk-pack", true, "added"))
	r := &BotToolAssignmentResource{data: warnProviderData(t, api)}

	d := modifyPlanDiags(t, r, "tool_ids", nil, &assignmentSet{ids: []string{"tl-mine"}})
	got := warningText(d)
	for _, s := range []string{"Plan removes pack-installed tools", "tl-pack", "setunion(data.botyard_pack_version.<name>.tool_ids, [...])"} {
		if !strings.Contains(got, s) {
			t.Errorf("warning missing %q:\n%s", s, got)
		}
	}
	for _, s := range []string{"tl-gone", "sk-pack"} {
		if strings.Contains(got, s) {
			t.Errorf("warning must not mention %q:\n%s", s, got)
		}
	}

	d = modifyPlanDiags(t, r, "tool_ids", nil, &assignmentSet{ids: []string{"tl-mine", "tl-pack"}})
	if w := warningText(d); w != "" {
		t.Errorf("keeping the pack tool must be quiet, got:\n%s", w)
	}

	d = modifyPlanDiags(t, r, "tool_ids", &assignmentSet{ids: []string{"tl-pack"}}, nil)
	if w := warningText(d); !strings.Contains(w, "Destroying this assignment removes pack-installed tools") {
		t.Errorf("destroy must warn, got:\n%s", w)
	}
}

// TestPackStripWarning_ApplyStillConverges proves the warning is advisory: a
// config that drops a pack skill plans and applies, and the skill is gone.
func TestPackStripWarning_ApplyStillConverges(t *testing.T) {
	api := newPackWarnAPI(skillItem("sk-pack", true, "added"))
	api.skills["sk-pack"] = true
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { api.checkUnexpected(t) })
	tfresource.UnitTest(t, tfresource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []tfresource.TestStep{{
			Config: packsConfig(srv.URL, `
resource "botyard_bot_skill_assignment" "s" {
  bot_slug  = "bot-1"
  skill_ids = ["sk-mine"]
}
`),
			Check: tfresource.ComposeAggregateTestCheckFunc(
				tfresource.TestCheckResourceAttr("botyard_bot_skill_assignment.s", "skill_ids.#", "1"),
				tfresource.TestCheckTypeSetElemAttr("botyard_bot_skill_assignment.s", "skill_ids.*", "sk-mine"),
				func(_ *terraform.State) error {
					api.mu.Lock()
					defer api.mu.Unlock()
					if api.skills["sk-pack"] {
						return fmt.Errorf("pack skill still assigned: %v", api.skills)
					}
					return nil
				},
			),
		}},
	})
}
