# Source the guided-setup onboarding defaults and wire them explicitly into the
# (exclusive) assignment resources. This keeps the defaults visible in config
# rather than applied server-side, so the assignment resources remain the single
# owner of the bot's tools and skills.
data "botyard_bot_template" "defaults" {
  slug = "guided-setup"
}

resource "botyard_bot_tool_assignment" "example" {
  bot_slug = botyard_bot.example.slug
  tool_ids = data.botyard_bot_template.defaults.tool_ids
}

resource "botyard_bot_skill_assignment" "example" {
  bot_slug  = botyard_bot.example.slug
  skill_ids = data.botyard_bot_template.defaults.skill_ids
}
