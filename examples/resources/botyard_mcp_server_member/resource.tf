# MCP servers are private: a principal reaches a server and its tools only if it
# is on the server's member list (or the server's `access` is "open").
resource "botyard_mcp_server" "vendor" {
  runtime_kind = "managed_remote"
  name         = "Vendor MCP"
  endpoint_url = "https://mcp.vendor.example.com"
}

# Once the server has discovered its tools, look one up and assign it to a bot.
data "botyard_tool" "vendor_search" {
  slug = "mcp:vendor-mcp:search"
}

resource "botyard_bot_tool_assignment" "assistant_tools" {
  bot_slug = botyard_bot.assistant.slug
  tool_ids = [data.botyard_tool.vendor_search.id]
}

# Assigning one of the server's tools already makes Botyard add the bot as a
# `member`. Declaring the membership as well makes the bot's access explicit,
# keeps it if the assignment is later removed, and is where a different role
# goes. The existing row is adopted, not duplicated.
resource "botyard_mcp_server_member" "assistant" {
  mcp_server_id = botyard_mcp_server.vendor.id
  actor_type    = "bot"
  actor_id      = botyard_bot.assistant.id

  depends_on = [botyard_bot_tool_assignment.assistant_tools]
}

# A human co-owner, who can manage the member list and access mode in the app.
resource "botyard_mcp_server_member" "platform_lead" {
  mcp_server_id = botyard_mcp_server.vendor.id
  actor_type    = "user"
  actor_id      = "5f0c2a4e-9b1d-4e7a-8c3f-2d6b1a9e0f47"
  role          = "owner"
}
