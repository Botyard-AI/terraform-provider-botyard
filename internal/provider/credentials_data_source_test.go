package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// cannedOrgCatalogListJSON exercises credential_id mapping, a null key_prefix, an
// omitted `managed` field, and a bot-private credential.
const cannedOrgCatalogListJSON = `[
  {
    "credential_id": "cred-1",
    "org_id": "org-1",
    "bot_id": null,
    "label": "OpenAI prod",
    "slug": "openai-prod",
    "api_protocol": null,
    "provider": "openai",
    "scope": "llm",
    "base_url": null,
    "auth_method": "api_key",
    "key_prefix": "sk-abcd...",
    "oauth_config": null,
    "is_default": true,
    "enabled": true,
    "available_to_all_bots": true,
    "managed": false,
    "last_tested_at": null,
    "last_test_status": null,
    "last_test_error": null,
    "created_at": "2026-07-20T10:00:00Z",
    "updated_at": "2026-07-20T11:00:00Z"
  },
  {
    "credential_id": "cred-2",
    "org_id": "org-1",
    "bot_id": "bot-9",
    "label": "Brave search",
    "slug": "brave",
    "api_protocol": null,
    "provider": "brave",
    "scope": "web_search",
    "base_url": null,
    "auth_method": "api_key",
    "key_prefix": null,
    "oauth_config": null,
    "is_default": false,
    "enabled": true,
    "available_to_all_bots": false,
    "last_tested_at": null,
    "last_test_status": null,
    "last_test_error": null,
    "created_at": "2026-07-20T10:00:00Z",
    "updated_at": "2026-07-20T11:00:00Z"
  }
]`

func TestListCredentials_RoundTripAndMapping(t *testing.T) {
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
		_, _ = w.Write([]byte(cannedOrgCatalogListJSON))
	}))
	defer srv.Close()

	c, err := client.NewClientWithResponses(srv.URL, client.WithRequestEditorFn(bearerAuth(apiKey)))
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}

	resp, err := c.ListCredentialsV1OrgsOrgIdCredentialsGetWithResponse(context.Background(), orgID, nil)
	if err != nil {
		t.Fatalf("ListCredentials...: %v", err)
	}
	if want := "Bearer " + apiKey; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
	if want := "/v1/orgs/" + orgID + "/credentials"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if resp.JSON200 == nil {
		t.Fatalf("JSON200 nil (status %d body %q)", resp.StatusCode(), string(resp.Body))
	}
	creds := *resp.JSON200
	if len(creds) != 2 {
		t.Fatalf("got %d credentials, want 2", len(creds))
	}

	// credential_id maps to the model's identifier; key_prefix present.
	m0 := credentialToModel(creds[0])
	if m0.CredentialID.ValueString() != "cred-1" {
		t.Errorf("m0 credential_id = %q, want cred-1", m0.CredentialID.ValueString())
	}
	if m0.Provider.ValueString() != "openai" || m0.Scope.ValueString() != "llm" {
		t.Errorf("m0 provider/scope = %q/%q", m0.Provider.ValueString(), m0.Scope.ValueString())
	}
	if m0.KeyPrefix.ValueString() != "sk-abcd..." || m0.AuthMethod.ValueString() != "api_key" {
		t.Errorf("m0 key_prefix/auth_method = %q/%q", m0.KeyPrefix.ValueString(), m0.AuthMethod.ValueString())
	}
	if !m0.AvailableToAllBots.ValueBool() || m0.Managed.ValueBool() || !m0.BotID.IsNull() {
		t.Errorf("m0 available/managed/bot_id = %v/%v/%v", m0.AvailableToAllBots.ValueBool(), m0.Managed.ValueBool(), m0.BotID)
	}

	// Second entry: null key_prefix, omitted managed -> null bool, private bot_id.
	m1 := credentialToModel(creds[1])
	if !m1.KeyPrefix.IsNull() {
		t.Errorf("m1 key_prefix = %q, want null", m1.KeyPrefix.ValueString())
	}
	if !m1.Managed.IsNull() {
		t.Errorf("m1 managed = %v, want null (field omitted)", m1.Managed)
	}
	if m1.BotID.ValueString() != "bot-9" {
		t.Errorf("m1 bot_id = %q, want bot-9", m1.BotID.ValueString())
	}
}
