# Source a visible catalog template's defaults and wire them explicitly into the
# (exclusive) assignment resources. Hidden/internal templates, including the
# guided-setup wizard bundle, are intentionally not exposed by this data source.
# Keeping visible defaults in configuration leaves the assignment resources as
# the single owner of the bot's tools and skills.
data "botyard_bot_template" "defaults" {
  slug = "coding-agent"
}

resource "botyard_bot_tool_assignment" "example" {
  bot_slug = botyard_bot.example.slug
  tool_ids = data.botyard_bot_template.defaults.tool_ids
}

resource "botyard_bot_skill_assignment" "example" {
  bot_slug  = botyard_bot.example.slug
  skill_ids = data.botyard_bot_template.defaults.skill_ids
}
