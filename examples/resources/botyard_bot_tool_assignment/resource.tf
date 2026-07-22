# Manage the complete set of tools assigned to a bot. This resource takes
# exclusive ownership of the bot's tool assignments: any tool assigned outside
# Terraform is removed on apply so the bot converges on tool_ids. Use at most one
# botyard_bot_tool_assignment per bot. Per-tool-domain settings live on the
# botyard_bot config block, not here.
resource "botyard_bot_tool_assignment" "assistant_tools" {
  bot_slug = botyard_bot.assistant.slug

  tool_ids = [
    "c0a70011-0000-4000-8000-000000000001",
    "c0a70011-0000-4000-8000-000000000002",
  ]
}
