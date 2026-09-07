# A container-image MCP server: Botyard runs the server as a pod in-cluster.
resource "botyard_mcp_server" "search" {
  runtime_kind = "container_image"
  name         = "Web Search"
  image        = "ghcr.io/example/search-mcp:1.2.0"
  port         = 8080

  # Non-sensitive configuration.
  env_plaintext = {
    SEARCH_REGION = "us-east-1"
  }

  # Sensitive values are supplied as Runtime Vault key-path *pointers*, never as
  # raw secrets — Botyard resolves them at runtime.
  env_secret_refs = {
    SEARCH_API_KEY = "search.api_key"
  }
}

# A managed-remote MCP server: Botyard proxies to a vendor-hosted endpoint.
resource "botyard_mcp_server" "vendor" {
  runtime_kind = "managed_remote"
  name         = "Vendor MCP"
  endpoint_url = "https://mcp.vendor.example.com"
}

# A managed-remote MCP server behind a bearer token.
#
# The token itself never appears in Terraform. `secret_headers` holds Runtime
# Vault *key paths*; Botyard resolves each one and sends the stored value as the
# header, so the secret at `mcp.posthog.authorization_header` must already read
# "Bearer <token>" — a bare token will be sent bare, and the vendor will answer
# 401.
#
# `acknowledged_credential_host` is your written acceptance that Botyard may
# send org credentials to that host. It is write-only: it is sent with the
# request, recorded in the audit trail, and never stored in Terraform state.
# Requires Terraform 1.11 or later.
resource "botyard_mcp_server" "posthog" {
  runtime_kind = "managed_remote"
  name         = "PostHog"
  endpoint_url = "https://mcp.posthog.com/mcp"

  static_headers = {
    "X-Api-Version" = "2026-09-01"
  }

  secret_headers = {
    "Authorization" = "mcp.posthog.authorization_header"
  }

  acknowledged_credential_host = "mcp.posthog.com"
}
