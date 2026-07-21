package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

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

// newBotClient builds a ClientWithResponses pointed at a mock server URL.
func newBotClient(t *testing.T, url, apiKey string) *client.ClientWithResponses {
	t.Helper()
	c, err := client.NewClientWithResponses(url, client.WithRequestEditorFn(bearerAuth(apiKey)))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return c
}

// tombstoneBotJSON is a bot whose desired_state is "deleted" (soft-delete
// tombstone still readable before the reconciler purges it).
const tombstoneBotJSON = `{
  "id": "b-123", "slug": "my-bot", "org_id": "org-1", "name": "My Bot",
  "namespace": "bot-my-bot", "runtime_class": "kata_qemu",
  "storage_class": "cluster_default", "runtime_privilege_mode": "privileged",
  "onboarding_state": "none", "health_status": "healthy",
  "desired_state": "deleted", "config_generation": 7,
  "created_at": "2026-07-20T10:00:00Z", "updated_at": "2026-07-20T11:30:00Z"
}`

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
	model.AvatarURL = types.StringValue("https://ex/a.png")

	body, err := buildBotUpdateBody(model)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m := decodeObj(t, body)

	if jsonStr(t, m["description"]) != "new desc" {
		t.Errorf("description = %s", m["description"])
	}
	if jsonStr(t, m["avatar_url"]) != "https://ex/a.png" {
		t.Errorf("avatar_url = %s", m["avatar_url"])
	}
	// name is immutable (RequiresReplace) and rejected by BotUpdate; config,
	// tier, and runtime fields are out of Phase A scope for the metadata PATCH.
	for _, k := range []string{"name", "config", "tier", "cluster_id", "runtime_class", "runtime_privilege_mode", "slug"} {
		if _, ok := m[k]; ok {
			t.Errorf("update body must not carry %q", k)
		}
	}
}

// TestBuildBotUpdateBody_ClearsWithNull is the regression for the avatar_url /
// description clear semantics: removing a value from config (types.StringNull)
// must send an explicit JSON null so the API clears the stored value, rather
// than a stale prior value. (avatar_url is plain Optional — no Computed /
// UseStateForUnknown that would restore the old value.)
func TestBuildBotUpdateBody_ClearsWithNull(t *testing.T) {
	model := botResourceModel() // description + avatar_url both null
	body, err := buildBotUpdateBody(model)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	m := decodeObj(t, body)
	if string(m["avatar_url"]) != "null" {
		t.Errorf("cleared avatar_url must serialize as null, got %s", m["avatar_url"])
	}
	if string(m["description"]) != "null" {
		t.Errorf("cleared description must serialize as null, got %s", m["description"])
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

// TestMapBotResource_NullOptionals proves omitted description/avatar_url map to
// null (not empty string), keeping state consistent with an omitted config.
func TestMapBotResource_NullOptionals(t *testing.T) {
	ts := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	b := &client.BotResponse{
		Id: "b-1", Slug: "s", OrgId: "o", Name: "n", Namespace: "ns",
		Description: nil, AvatarUrl: nil,
		DesiredState: client.DesiredStateRunning, CreatedAt: ts, UpdatedAt: ts,
	}
	var m BotResourceModel
	mapBotResource(b, &m)
	if !m.Description.IsNull() {
		t.Errorf("description should be null, got %q", m.Description.ValueString())
	}
	if !m.AvatarURL.IsNull() {
		t.Errorf("avatar_url should be null, got %q", m.AvatarURL.ValueString())
	}
}

func TestBotReadDisposition(t *testing.T) {
	live := &client.BotResponse{DesiredState: client.DesiredStateRunning}
	tombstone := &client.BotResponse{DesiredState: client.DesiredStateDeleted}
	cases := []struct {
		name   string
		status int
		body   *client.BotResponse
		want   botReadResult
	}{
		{"live 200", 200, live, botReadOK},
		{"404 gone", 404, nil, botReadGone},
		{"tombstone 200 deleted", 200, tombstone, botReadGone},
		{"non-404 nil body", 500, nil, botReadUnexpected},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := botReadDisposition(c.status, c.body); got != c.want {
				t.Errorf("botReadDisposition(%d) = %d, want %d", c.status, got, c.want)
			}
		})
	}
}

func TestBotDeleteStatusAccepted(t *testing.T) {
	for _, code := range []int{200, 202, 204, 404} {
		if !botDeleteStatusAccepted(code) {
			t.Errorf("status %d should be accepted", code)
		}
	}
	for _, code := range []int{400, 403, 409, 500, 502} {
		if botDeleteStatusAccepted(code) {
			t.Errorf("status %d should NOT be accepted", code)
		}
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
	var gotBodyRaw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotBodyRaw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(cannedBotJSON))
	}))
	defer srv.Close()

	c := newBotClient(t, srv.URL, apiKey)

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
	// The server received exactly the Phase A create shape (decoded without `any`).
	sent := decodeObj(t, gotBodyRaw)
	if string(sent["config"]) != "{}" {
		t.Errorf("config should be an empty object, got %s", sent["config"])
	}
	for _, k := range forbiddenCreateKeys {
		if _, ok := sent[k]; ok {
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

// TestBotResource_UpdateRoundTrip proves the metadata PATCH targets the
// slug-addressed path with the correct method and sparse body, and maps the
// 200 response.
func TestBotResource_UpdateRoundTrip(t *testing.T) {
	const orgID, slug = "org-1", "my-bot"
	var gotPath, gotMethod string
	var gotBodyRaw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotBodyRaw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedBotJSON))
	}))
	defer srv.Close()

	c := newBotClient(t, srv.URL, "byk_test")
	model := botResourceModel()
	model.Description = types.StringValue("updated")
	body, err := buildBotUpdateBody(model)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	resp, err := c.UpdateBotV1OrgsOrgIdBotsBotSlugPatchWithBodyWithResponse(
		context.Background(), orgID, slug, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("UpdateBot...: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if want := "/v1/orgs/" + orgID + "/bots/" + slug; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	sent := decodeObj(t, gotBodyRaw)
	if jsonStr(t, sent["description"]) != "updated" {
		t.Errorf("description = %s", sent["description"])
	}
	if _, ok := sent["name"]; ok {
		t.Error("update must not carry name")
	}
	if resp.JSON200 == nil {
		t.Fatalf("JSON200 nil (status %d)", resp.StatusCode())
	}
}

// TestBotResource_ReadRoundTrip exercises the real GET response handling through
// the client for the live, tombstone, and 404 cases and asserts the resource's
// disposition decision for each.
func TestBotResource_ReadRoundTrip(t *testing.T) {
	const orgID, slug = "org-1", "my-bot"
	cases := []struct {
		name   string
		status int
		body   string
		want   botReadResult
	}{
		{"live", http.StatusOK, cannedBotJSON, botReadOK},
		{"tombstone", http.StatusOK, tombstoneBotJSON, botReadGone},
		{"not found", http.StatusNotFound, `{"detail":"Bot not found"}`, botReadGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newBotClient(t, srv.URL, "byk_test")
			resp, err := c.GetBotV1OrgsOrgIdBotsBotSlugGetWithResponse(context.Background(), orgID, slug)
			if err != nil {
				t.Fatalf("GetBot...: %v", err)
			}
			if got := botReadDisposition(resp.StatusCode(), resp.JSON200); got != tc.want {
				t.Errorf("disposition = %d, want %d (status %d)", got, tc.want, resp.StatusCode())
			}
		})
	}
}

// TestBotResource_DeleteRoundTrip proves a soft-delete 200 is accepted and an
// unexpected status surfaces as not-accepted (an error in Delete).
func TestBotResource_DeleteRoundTrip(t *testing.T) {
	const orgID, slug = "org-1", "my-bot"
	for _, tc := range []struct {
		name     string
		status   int
		accepted bool
	}{
		{"soft delete ok", http.StatusOK, true},
		{"server error", http.StatusInternalServerError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(cannedBotJSON))
			}))
			defer srv.Close()

			c := newBotClient(t, srv.URL, "byk_test")
			resp, err := c.DeleteBotV1OrgsOrgIdBotsBotSlugDeleteWithResponse(context.Background(), orgID, slug)
			if err != nil {
				t.Fatalf("DeleteBot...: %v", err)
			}
			if got := botDeleteStatusAccepted(resp.StatusCode()); got != tc.accepted {
				t.Errorf("accepted = %v, want %v (status %d)", got, tc.accepted, resp.StatusCode())
			}
		})
	}
}

// TestBotResource_ImportStateSetsSlug proves import passes the ID through to the
// slug attribute (bots are slug-addressed) and does not populate id.
func TestBotResource_ImportStateSetsSlug(t *testing.T) {
	ctx := context.Background()
	r := &BotResource{}
	var sr resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sr)

	objType := sr.Schema.Type().TerraformType(ctx).(tftypes.Object)
	vals := make(map[string]tftypes.Value, len(objType.AttributeTypes))
	for name, typ := range objType.AttributeTypes {
		vals[name] = tftypes.NewValue(typ, nil)
	}
	resp := resource.ImportStateResponse{
		State: tfsdk.State{Schema: sr.Schema, Raw: tftypes.NewValue(objType, vals)},
	}
	r.ImportState(ctx, resource.ImportStateRequest{ID: "my-bot"}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("import diags: %v", resp.Diagnostics)
	}

	var slug types.String
	resp.Diagnostics.Append(resp.State.GetAttribute(ctx, path.Root("slug"), &slug)...)
	if slug.ValueString() != "my-bot" {
		t.Errorf("imported slug = %q, want my-bot", slug.ValueString())
	}
	var id types.String
	resp.State.GetAttribute(ctx, path.Root("id"), &id)
	if !id.IsNull() {
		t.Errorf("id should be null after slug-only import, got %q", id.ValueString())
	}
}
