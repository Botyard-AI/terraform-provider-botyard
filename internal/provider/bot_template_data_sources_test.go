package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// cannedBotTemplatesJSON has a visible template whose config patch contains
// both an explicit null and an unmodeled future key, plus a second template
// with null config. This exercises lossless config_json preservation and both
// null/non-null branches.
const cannedBotTemplatesJSON = `[
  {
    "id": "tpl-1",
    "slug": "coding-agent",
    "name": "Coding Agent",
    "description": "A visible coding bundle",
    "icon": "code",
    "files": {"soul": "You are helpful.", "heartbeat": "tick"},
    "skill_ids": ["sk-a", "sk-b"],
    "tool_ids": ["tool-1", "tool-2", "tool-3"],
    "supports_guided_setup": true,
    "config": {"reasoning_default": "off", "tool_search": null, "future_unmodeled": {"enabled": true}}
  },
  {
    "id": "tpl-2",
    "slug": "blank",
    "name": "Blank",
    "description": "No defaults",
    "icon": "user",
    "files": {},
    "skill_ids": [],
    "tool_ids": [],
    "supports_guided_setup": false,
    "config": null
  }
]`

// TestFindBotTemplateBySlug covers the singular botyard_bot_template lookup: a
// slug hit returns the matching template, and a miss (including an empty list)
// returns ok=false so Read emits a "not found" diagnostic.
func TestFindBotTemplateBySlug(t *testing.T) {
	templates := []client.BotTemplateWithRawConfig{
		{Template: client.BotTemplateResponse{Id: "tpl-1", Slug: "coding-agent"}},
		{Template: client.BotTemplateResponse{Id: "tpl-2", Slug: "blank"}},
	}
	got, found := findBotTemplateBySlug(templates, "coding-agent")
	if !found {
		t.Fatal("findBotTemplateBySlug did not find an existing slug")
	}
	if got.Template.Id != "tpl-1" {
		t.Errorf("findBotTemplateBySlug id = %q, want tpl-1", got.Template.Id)
	}
	if _, found := findBotTemplateBySlug(templates, "nope"); found {
		t.Error("findBotTemplateBySlug found a non-existent slug")
	}
	if _, found := findBotTemplateBySlug(nil, "coding-agent"); found {
		t.Error("findBotTemplateBySlug found a slug in an empty list")
	}
}

func TestListBotTemplates_RoundTripAndMapping(t *testing.T) {
	const (
		apiKey = "byk_test_secret"
		orgID  = "org-1"
	)
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedBotTemplatesJSON))
	}))
	defer srv.Close()

	c, err := client.NewClientWithResponses(srv.URL, client.WithRequestEditorFn(bearerAuth(apiKey)))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	data := &providerData{client: c, orgID: orgID}

	var diags diag.Diagnostics
	templates, ok := listBotTemplates(context.Background(), data, &diags)
	if !ok || diags.HasError() {
		t.Fatalf("listBotTemplates failed: ok=%v diags=%v", ok, diags)
	}
	if want := "Bearer " + apiKey; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
	if want := "/v1/orgs/" + orgID + "/bot-templates"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if len(templates) != 2 {
		t.Fatalf("got %d templates, want 2", len(templates))
	}

	// Visible template: bundle + files + lossless config_json present.
	m0 := botTemplateToModel(templates[0])
	if m0.Slug.ValueString() != "coding-agent" || !m0.SupportsGuidedSetup.ValueBool() {
		t.Errorf("m0 slug/guided = %q/%v", m0.Slug.ValueString(), m0.SupportsGuidedSetup.ValueBool())
	}
	if len(m0.ToolIDs) != 3 || m0.ToolIDs[0] != "tool-1" {
		t.Errorf("m0 tool_ids = %v", m0.ToolIDs)
	}
	if len(m0.SkillIDs) != 2 || m0.SkillIDs[1] != "sk-b" {
		t.Errorf("m0 skill_ids = %v", m0.SkillIDs)
	}
	if m0.Files["soul"] != "You are helpful." {
		t.Errorf("m0 files[soul] = %q", m0.Files["soul"])
	}
	if m0.ConfigJSON.IsNull() {
		t.Fatal("m0 config_json is null, want a JSON object")
	}
	var gotConfig map[string]any
	if err := json.Unmarshal([]byte(m0.ConfigJSON.ValueString()), &gotConfig); err != nil {
		t.Fatalf("config_json not valid JSON: %v", err)
	}
	wantConfig := map[string]any{
		"reasoning_default": "off",
		"tool_search":       nil,
		"future_unmodeled":  map[string]any{"enabled": true},
	}
	if !reflect.DeepEqual(gotConfig, wantConfig) {
		t.Errorf("config_json = %#v, want semantic equality with %#v", gotConfig, wantConfig)
	}

	// Blank template: null config -> null config_json; empty bundles.
	m1 := botTemplateToModel(templates[1])
	if !m1.ConfigJSON.IsNull() {
		t.Errorf("m1 config_json = %q, want null", m1.ConfigJSON.ValueString())
	}
	if len(m1.ToolIDs) != 0 || len(m1.SkillIDs) != 0 {
		t.Errorf("m1 bundles not empty: tools=%v skills=%v", m1.ToolIDs, m1.SkillIDs)
	}
}
