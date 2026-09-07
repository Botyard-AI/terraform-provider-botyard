package provider

import (
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// testAccPreCheck skips unless the acceptance suite has a real Botyard org to
// talk to. Run with:
//
//	BOTYARD_ENDPOINT=... BOTYARD_API_KEY=... BOTYARD_ORG_ID=... make testacc
func testAccPreCheck(t *testing.T) {
	t.Helper()
	for _, k := range []string{"BOTYARD_API_KEY", "BOTYARD_ORG_ID"} {
		if os.Getenv(k) == "" {
			t.Fatalf("%s must be set for acceptance tests", k)
		}
	}
}

// testAccMcpHeadersConfig renders a vault secret plus a managed-remote server
// that authenticates with it. The secret's *value* is a throwaway string; what
// matters is that `secret_headers` carries the vault key path, never a token.
func testAccMcpHeadersConfig(suffix, endpoint, ackHost, extraStatic string) string {
	return fmt.Sprintf(`
resource "botyard_vault_secret" "acc" {
  key_path     = "acctest.tfheaders.%[1]s.authorization_header"
  display_name = "TF acceptance header %[1]s"
  sensitivity  = "secret"
  secret_value = "Bearer acctest-placeholder-not-a-real-token"
}

resource "botyard_mcp_server" "acc" {
  runtime_kind = "managed_remote"
  name         = "TF acceptance headers %[1]s"
  slug         = "tf-acc-headers-%[1]s"
  endpoint_url = %[2]q

  static_headers = {
    "X-Acc-Test" = "1"
    %[4]s
  }

  secret_headers = {
    "Authorization" = botyard_vault_secret.acc.key_path
  }

  acknowledged_credential_host = %[3]q
}
`, suffix, endpoint, ackHost, extraStatic)
}

// TestAccMcpServer_ManagedRemoteHeaders is the end-to-end proof for this
// feature. In one run it covers all three cases the task asks for:
//
//  1. create an authenticated managed-remote server with a vault-backed header,
//     and confirm the plan is empty afterwards (no perpetual diff from the
//     write-only acknowledgement);
//  2. import it and confirm the headers round-trip;
//  3. re-point endpoint_url at a *different host* with a fresh acknowledgement,
//     and change the static headers.
func TestAccMcpServer_ManagedRemoteHeaders(t *testing.T) {
	suffix := fmt.Sprintf("%d", os.Getpid())
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccMcpHeadersConfig(suffix, "https://mcp.example.com/mcp", "mcp.example.com", ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("botyard_mcp_server.acc", "endpoint_url", "https://mcp.example.com/mcp"),
					resource.TestCheckResourceAttr("botyard_mcp_server.acc", "static_headers.X-Acc-Test", "1"),
					resource.TestCheckResourceAttr("botyard_mcp_server.acc", "secret_headers.Authorization",
						fmt.Sprintf("acctest.tfheaders.%s.authorization_header", suffix)),
					// Write-only: sent with the request, never stored.
					resource.TestCheckNoResourceAttr("botyard_mcp_server.acc", "acknowledged_credential_host"),
				),
			},
			{
				ResourceName:      "botyard_mcp_server.acc",
				ImportState:       true,
				ImportStateVerify: true,
				// acknowledged_credential_host is never returned by the API, so it
				// cannot be verified. observed_state and updated_at move on their
				// own as the control plane brings the server up.
				ImportStateVerifyIgnore: []string{"acknowledged_credential_host", "observed_state", "updated_at"},
			},
			{
				// New host, fresh acknowledgement, and a changed static header.
				Config: testAccMcpHeadersConfig(suffix, "https://mcp.other.example.com/mcp", "mcp.other.example.com",
					`"X-Acc-Extra" = "2"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("botyard_mcp_server.acc", "endpoint_url", "https://mcp.other.example.com/mcp"),
					resource.TestCheckResourceAttr("botyard_mcp_server.acc", "static_headers.X-Acc-Extra", "2"),
				),
			},
		},
	})
}

// TestAccMcpServer_ContainerImageRejectsHeaders proves the container_image
// rejection surfaces as a plan-time error, before anything is sent.
func TestAccMcpServer_ContainerImageRejectsHeaders(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "botyard_mcp_server" "bad" {
  runtime_kind   = "container_image"
  name           = "TF acceptance reject"
  image          = "ghcr.io/example/search-mcp:1.2.0"
  port           = 8080
  static_headers = { "X-Nope" = "1" }
}
`,
				ExpectError: regexp.MustCompile(`static_headers.*managed_remote`),
			},
			{
				Config: `
resource "botyard_mcp_server" "bad" {
  runtime_kind = "container_image"
  name         = "TF acceptance reject"
  image        = "ghcr.io/example/search-mcp:1.2.0"
  port         = 8080
  secret_headers = { "Authorization" = "some.vault.path" }
}
`,
				ExpectError: regexp.MustCompile(`secret_headers.*managed_remote`),
			},
		},
	})
}

// TestAccMcpServer_RejectsPlaintextAuthorization proves the one backend rule the
// provider mirrors fails at plan time rather than as an API 422.
func TestAccMcpServer_RejectsPlaintextAuthorization(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "botyard_mcp_server" "bad" {
  runtime_kind   = "managed_remote"
  name           = "TF acceptance plaintext auth"
  endpoint_url   = "https://mcp.example.com/mcp"
  static_headers = { "Authorization" = "Bearer nope" }
}
`,
				ExpectError: regexp.MustCompile(`Plaintext Authorization header`),
			},
		},
	})
}
