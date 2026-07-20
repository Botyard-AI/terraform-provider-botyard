package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

const cannedBotJSON = `{
  "id": "b-123",
  "slug": "my-bot",
  "org_id": "org-1",
  "cluster_id": "hel1-0",
  "name": "My Bot",
  "namespace": "bot-my-bot",
  "tier": "starter",
  "runtime_class": "kata_qemu",
  "storage_class": "cluster_default",
  "runtime_privilege_mode": "privileged",
  "onboarding_state": "complete",
  "health_status": "healthy",
  "desired_state": "running",
  "config_generation": 7,
  "created_at": "2026-07-20T10:00:00Z",
  "updated_at": "2026-07-20T11:30:00Z"
}`

// TestBotDataSource_ClientRoundTripWithAuth proves the generated client, the
// Bearer auth editor, and the org-scoped path all wire together against a mock
// API, and that the response maps into the data-source model.
func TestBotDataSource_ClientRoundTripWithAuth(t *testing.T) {
	const (
		apiKey  = "byk_test_secret"
		orgID   = "org-1"
		botSlug = "my-bot"
	)

	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(cannedBotJSON))
	}))
	defer srv.Close()

	c, err := client.NewClientWithResponses(srv.URL, client.WithRequestEditorFn(bearerAuth(apiKey)))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}

	resp, err := c.GetBotV1OrgsOrgIdBotsBotSlugGetWithResponse(context.Background(), orgID, botSlug)
	if err != nil {
		t.Fatalf("GetBot...: %v", err)
	}

	if want := "Bearer " + apiKey; gotAuth != want {
		t.Errorf("Authorization header = %q, want %q", gotAuth, want)
	}
	if want := "/v1/orgs/" + orgID + "/bots/" + botSlug; gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}
	if resp.JSON200 == nil {
		t.Fatalf("JSON200 is nil (status %d, body %q)", resp.StatusCode(), string(resp.Body))
	}

	model := botToModel(resp.JSON200)
	cases := map[string]struct{ got, want string }{
		"slug":             {model.Slug.ValueString(), "my-bot"},
		"id":               {model.ID.ValueString(), "b-123"},
		"org_id":           {model.OrgID.ValueString(), "org-1"},
		"cluster_id":       {model.ClusterID.ValueString(), "hel1-0"},
		"name":             {model.Name.ValueString(), "My Bot"},
		"tier":             {model.Tier.ValueString(), "starter"},
		"health_status":    {model.HealthStatus.ValueString(), "healthy"},
		"desired_state":    {model.DesiredState.ValueString(), "running"},
		"created_at":       {model.CreatedAt.ValueString(), "2026-07-20T10:00:00Z"},
		"updated_at":       {model.UpdatedAt.ValueString(), "2026-07-20T11:30:00Z"},
		"privilege_mode":   {model.RuntimePrivilegeMode.ValueString(), "privileged"},
		"onboarding_state": {model.OnboardingState.ValueString(), "complete"},
	}
	for field, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", field, c.got, c.want)
		}
	}
	if model.ConfigGeneration.ValueInt64() != 7 {
		t.Errorf("config_generation = %d, want 7", model.ConfigGeneration.ValueInt64())
	}
}
