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

# On a bot created from a pack, the pack's tools are part of the exclusive set.
# Keep them by adding the pack version's tool_ids; otherwise the plan warns and
# the apply unassigns them.
data "botyard_pack_version" "crm" {
  slug = "crm-assistant"
}

resource "botyard_bot_tool_assignment" "crm_bot_tools" {
  bot_slug = botyard_bot.crm.slug

  tool_ids = setunion(data.botyard_pack_version.crm.tool_ids, [
    "c0a70011-0000-4000-8000-000000000001",
  ])
}
