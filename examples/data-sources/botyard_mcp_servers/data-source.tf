# List the organization's MCP servers for discovery.
data "botyard_mcp_servers" "all" {}

output "mcp_server_slugs" {
  value = [for s in data.botyard_mcp_servers.all.mcp_servers : s.slug]
}
