# Resolve a single skill's id from its slug, then assign it to a bot.
data "botyard_skill" "email_helper" {
  slug = "email-helper"
}

resource "botyard_bot_skill_assignment" "example" {
  bot_slug  = botyard_bot.example.slug
  skill_ids = [data.botyard_skill.email_helper.id]
}
