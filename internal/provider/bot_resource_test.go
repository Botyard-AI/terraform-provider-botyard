package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// botResourceModel builds a minimal valid create-plan model: only the required
// name plus null optionals (description/avatar_url unset).
func botResourceModel() BotResourceModel {
	return BotResourceModel{
		Name:        types.StringValue("My Bot"),
		Description: types.StringNull(),
		AvatarURL:   types.StringNull(),
	}
}

// forbiddenCreateKeys are attributes that must never appear in the create wire
// body: tier/cluster_id are deliberately dropped (task #825); the runtime
// placement + identity fields are server-computed and rejected by the strict
// BotCreate model; config sub-fields are a later phase.
var forbiddenCreateKeys = []string{
	"tier", "cluster_id", "runtime_class", "storage_class",
	"runtime_privilege_mode", "durable_root_owns_home", "slug", "id",
}

func TestBuildBotCreateBody_MinimalSendsEmptyConfig(t *testing.T) {
	body, diags := buildBotCreateBody(botResourceModel())
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	m := decodeObj(t, body)

	if jsonStr(t, m["name"]) != "My Bot" {
		t.Errorf("name = %s", m["name"])
	}
	// config is required by the API but must be sent as an empty object so it
	// does not null out OpenClaw defaults.
	if string(m["config"]) != "{}" {
		t.Errorf("config = %s, want {}", m["config"])
	}
	// unset optionals serialize as explicit null (BotCreate accepts null).
	if string(m["description"]) != "null" || string(m["avatar_url"]) != "null" {
		t.Errorf("unset optionals should be null: description=%s avatar_url=%s", m["description"], m["avatar_url"])
	}
	for _, k := range forbiddenCreateKeys {
		if _, ok := m[k]; ok {
			t.Errorf("create body must not carry %q", k)
		}
	}
}

func TestBuildBotCreateBody_WithMetadata(t *testing.T) {
	model := botResourceModel()
	model.Description = types.StringValue("Platform developer")
	model.AvatarURL = types.StringValue("https://ex/a.png")

	body, diags := buildBotCreateBody(model)
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	m := decodeObj(t, body)
	if jsonStr(t, m["description"]) != "Platform developer" {
		t.Errorf("description = %s", m["description"])
	}
	if jsonStr(t, m["avatar_url"]) != "https://ex/a.png" {
		t.Errorf("avatar_url = %s", m["avatar_url"])
	}
}

func TestBuildBotUpdateBody_OnlyMetadataFields(t *testing.T) {
	model := botResourceModel()
	model.Description = types.StringValue("new desc")

	body, err := buildBotUpdateBody(model)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m := decodeObj(t, body)

	if jsonStr(t, m["description"]) != "new desc" {
		t.Errorf("description = %s", m["description"])
	}
	if _, ok := m["avatar_url"]; !ok {
		t.Error("update body should carry avatar_url")
	}
	// name is immutable (RequiresReplace) and rejected by BotUpdate; config,
	// tier, and runtime fields are out of Phase A scope for the metadata PATCH.
	for _, k := range []string{"name", "config", "tier", "cluster_id", "runtime_class", "runtime_privilege_mode", "slug"} {
		if _, ok := m[k]; ok {
			t.Errorf("update body must not carry %q", k)
		}
	}
}

func TestMapBotResource(t *testing.T) {
	ts := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	b := &client.BotResponse{
		Id:                   "b-123",
		Slug:                 "my-bot",
		OrgId:                "org-1",
		Name:                 "My Bot",
		Description:          strp("Platform developer"),
		AvatarUrl:            strp("https://ex/a.png"),
		Namespace:            "bot-my-bot",
		ClusterId:            "hel1-0", // present on the API type; must NOT surface in the model
		Tier:                 client.BotTier("starter"),
		RuntimeClass:         client.BotRuntimeClass("kata_qemu"),
		StorageClass:         client.BotStorageClass("cluster_default"),
		RuntimePrivilegeMode: client.BotRuntimePrivilegeMode("privileged"),
		DurableRootOwnsHome:  true,
		Access:               client.BotAccess("open"),
		OnboardingState:      client.BotOnboardingState("none"),
		HealthStatus:         client.BotHealthStatus("healthy"),
		DesiredState:         client.DesiredStateRunning,
		ConfigGeneration:     7,
		CreatedAt:            ts,
		UpdatedAt:            ts,
	}

	var m BotResourceModel
	mapBotResource(b, &m)

	cases := map[string]struct{ got, want string }{
		"id":                     {m.ID.ValueString(), "b-123"},
		"slug":                   {m.Slug.ValueString(), "my-bot"},
		"org_id":                 {m.OrgID.ValueString(), "org-1"},
		"name":                   {m.Name.ValueString(), "My Bot"},
		"description":            {m.Description.ValueString(), "Platform developer"},
		"avatar_url":             {m.AvatarURL.ValueString(), "https://ex/a.png"},
		"namespace":              {m.Namespace.ValueString(), "bot-my-bot"},
		"runtime_class":          {m.RuntimeClass.ValueString(), "kata_qemu"},
		"storage_class":          {m.StorageClass.ValueString(), "cluster_default"},
		"runtime_privilege_mode": {m.RuntimePrivilegeMode.ValueString(), "privileged"},
		"access":                 {m.Access.ValueString(), "open"},
		"onboarding_state":       {m.OnboardingState.ValueString(), "none"},
		"health_status":          {m.HealthStatus.ValueString(), "healthy"},
		"desired_state":          {m.DesiredState.ValueString(), "running"},
	}
	for field, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", field, c.got, c.want)
		}
	}
	if !m.DurableRootOwnsHome.ValueBool() {
		t.Error("durable_root_owns_home should be true")
	}
	if m.ConfigGeneration.ValueInt64() != 7 {
		t.Errorf("config_generation = %d, want 7", m.ConfigGeneration.ValueInt64())
	}
}

// TestBotResource_CreateRoundTripWithAuth proves the generated client, Bearer
// auth, the org-scoped POST path, and the hand-built create body all wire
// together against a mock API, and that the 201 response maps into the model.
func TestBotResource_CreateRoundTripWithAuth(t *testing.T) {
	const (
		apiKey = "byk_test_secret"
		orgID  = "org-1"
	)

	var gotAuth, gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(cannedBotJSON))
	}))
	defer srv.Close()

	c, err := client.NewClientWithResponses(srv.URL, client.WithRequestEditorFn(bearerAuth(apiKey)))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}

	body, diags := buildBotCreateBody(botResourceModel())
	if diags.HasError() {
		t.Fatalf("diags: %v", diags)
	}
	resp, err := c.CreateBotV1OrgsOrgIdBotsPostWithBodyWithResponse(
		context.Background(), orgID, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("CreateBot...: %v", err)
	}

	if want := "Bearer " + apiKey; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if want := "/v1/orgs/" + orgID + "/bots"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	// The server received exactly the Phase A create shape.
	if gotBody["config"] == nil {
		t.Error("server did not receive a config object")
	}
	if cfg, ok := gotBody["config"].(map[string]any); !ok || len(cfg) != 0 {
		t.Errorf("config should be an empty object, got %v", gotBody["config"])
	}
	for _, k := range forbiddenCreateKeys {
		if _, ok := gotBody[k]; ok {
			t.Errorf("server received forbidden key %q", k)
		}
	}
	if resp.JSON201 == nil {
		t.Fatalf("JSON201 is nil (status %d, body %q)", resp.StatusCode(), string(resp.Body))
	}

	var m BotResourceModel
	mapBotResource(resp.JSON201, &m)
	if m.Slug.ValueString() != "my-bot" || m.ID.ValueString() != "b-123" {
		t.Errorf("mapped slug/id = %q/%q", m.Slug.ValueString(), m.ID.ValueString())
	}
}
