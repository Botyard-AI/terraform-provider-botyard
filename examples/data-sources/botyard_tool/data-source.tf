# Resolve a single tool's id from its composite slug, then assign it to a bot.
data "botyard_tool" "list_repos" {
  slug = "mcp:botyard:github_list_repos"
}

resource "botyard_bot_tool_assignment" "example" {
  bot_slug = botyard_bot.example.slug
  tool_ids = [data.botyard_tool.list_repos.id]
}
