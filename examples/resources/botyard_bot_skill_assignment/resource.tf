# Manage the complete set of skills assigned to a bot. This resource takes
# exclusive ownership of the bot's skill assignments: any skill assigned outside
# Terraform is removed on apply so the bot converges on skill_ids. Use at most
# one botyard_bot_skill_assignment per bot.
resource "botyard_bot_skill_assignment" "assistant_skills" {
  bot_slug = botyard_bot.assistant.slug

  skill_ids = [
    "b1a7c0de-0000-4000-8000-000000000001",
    "b1a7c0de-0000-4000-8000-000000000002",
  ]
}
