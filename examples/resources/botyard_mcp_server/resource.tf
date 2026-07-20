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
