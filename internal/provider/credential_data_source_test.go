package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/Botyard-AI/terraform-provider-botyard/internal/client"
)

// cannedUserClaudeListJSON is an org's credential list in which the
// system-created User Claude subscription row is listed after a BYOK key, as
// it is for orgs that had credentials before the row was backfilled.
const cannedUserClaudeListJSON = `[
  {
    "credential_id": "cred-openrouter",
    "org_id": "org-1",
    "bot_id": null,
    "label": "OpenRouter",
    "slug": "openrouter",
    "api_protocol": "openai_chat",
    "provider": "openrouter",
    "scope": "llm",
    "base_url": null,
    "auth_method": "api_key",
    "key_prefix": "sk-or-...",
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
    "credential_id": "cred-user-claude",
    "org_id": "org-1",
    "bot_id": null,
    "label": "User Claude subscription",
    "slug": "user-claude-subscription",
    "api_protocol": "claude_code_cli",
    "provider": "user_claude_subscription",
    "scope": "llm",
    "base_url": null,
    "auth_method": "none",
    "key_prefix": null,
    "oauth_config": null,
    "is_default": false,
    "enabled": true,
    "available_to_all_bots": true,
    "managed": true,
    "last_tested_at": null,
    "last_test_status": null,
    "last_test_error": null,
    "created_at": "2026-11-06T10:00:00Z",
    "updated_at": "2026-11-06T10:00:00Z"
  }
]`

func decodeCannedCredentials(t *testing.T) []client.CredentialResponse {
	t.Helper()
	var creds []client.CredentialResponse
	if err := json.Unmarshal([]byte(cannedUserClaudeListJSON), &creds); err != nil {
		t.Fatalf("decode canned credentials: %v", err)
	}
	return creds
}

func TestFindCredentialBySlug(t *testing.T) {
	creds := decodeCannedCredentials(t)

	c, ok := findCredentialBySlug(creds, "user-claude-subscription")
	if !ok {
		t.Fatal("user-claude-subscription not found")
	}
	if c.CredentialId != "cred-user-claude" {
		t.Errorf("credential_id = %q, want cred-user-claude", c.CredentialId)
	}
	// The generated client must carry the provider value through unchanged.
	if c.Provider != client.CredentialProviderUserClaudeSubscription {
		t.Errorf("provider = %q, want user_claude_subscription", c.Provider)
	}

	if _, ok := findCredentialBySlug(creds, "missing"); ok {
		t.Error("found a credential for an unknown slug")
	}
	if _, ok := findCredentialBySlug(nil, "openrouter"); ok {
		t.Error("found a credential in an empty list")
	}
}

func TestCredentialToSingularModel(t *testing.T) {
	c, _ := findCredentialBySlug(decodeCannedCredentials(t), "user-claude-subscription")
	m := credentialToSingularModel(c)

	if m.CredentialID.ValueString() != "cred-user-claude" || m.Slug.ValueString() != "user-claude-subscription" {
		t.Errorf("credential_id/slug = %q/%q", m.CredentialID.ValueString(), m.Slug.ValueString())
	}
	if m.CredentialProvider.ValueString() != "user_claude_subscription" || m.Scope.ValueString() != "llm" {
		t.Errorf("credential_provider/scope = %q/%q", m.CredentialProvider.ValueString(), m.Scope.ValueString())
	}
	if m.AuthMethod.ValueString() != "none" || !m.KeyPrefix.IsNull() || !m.Managed.ValueBool() {
		t.Errorf("auth_method/key_prefix/managed = %q/%v/%v", m.AuthMethod.ValueString(), m.KeyPrefix, m.Managed)
	}
	if m.CreatedAt.ValueString() != "2026-11-06T10:00:00Z" {
		t.Errorf("created_at = %q", m.CreatedAt.ValueString())
	}
}

func newFakeCredentialListAPI(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/orgs/org-1/credentials" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func credentialDataSourceConfig(endpoint, slug string) string {
	return fmt.Sprintf(`
provider "botyard" {
  endpoint = %q
  api_key  = "byk_test"
  org_id   = "org-1"
}

data "botyard_credential" "x" {
  slug = %q
}
`, endpoint, slug)
}

// TestCredentialDataSource runs the data source through Terraform: it resolves
// the User Claude subscription by slug, and fails clearly for an unknown slug
// and for an API error.
func TestCredentialDataSource(t *testing.T) {
	srv := newFakeCredentialListAPI(t, http.StatusOK, cannedUserClaudeListJSON)
	const addr = "data.botyard_credential.x"

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: credentialDataSourceConfig(srv.URL, "user-claude-subscription"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "credential_id", "cred-user-claude"),
					resource.TestCheckResourceAttr(addr, "slug", "user-claude-subscription"),
					resource.TestCheckResourceAttr(addr, "credential_provider", "user_claude_subscription"),
					resource.TestCheckResourceAttr(addr, "scope", "llm"),
					resource.TestCheckResourceAttr(addr, "auth_method", "none"),
					resource.TestCheckResourceAttr(addr, "managed", "true"),
					resource.TestCheckResourceAttr(addr, "enabled", "true"),
					resource.TestCheckNoResourceAttr(addr, "key_prefix"),
					resource.TestCheckNoResourceAttr(addr, "bot_id"),
				),
			},
			{
				Config:      credentialDataSourceConfig(srv.URL, "no-such-credential"),
				ExpectError: regexp.MustCompile(`No organization credential with slug "no-such-credential"`),
			},
		},
	})
}

func TestCredentialDataSource_APIError(t *testing.T) {
	srv := newFakeCredentialListAPI(t, http.StatusForbidden, `{"detail":"forbidden"}`)

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config:      credentialDataSourceConfig(srv.URL, "user-claude-subscription"),
			ExpectError: regexp.MustCompile(`Listing credentials returned HTTP 403`),
		}},
	})
}
