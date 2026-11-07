# A restricted bot is usable only by its members. Each botyard_bot_member puts
# one person on the list and leaves every other row alone, so members added in
# the Botyard app never show up as drift.
resource "botyard_bot" "assistant" {
  name   = "Assistant"
  access = "restricted"
}

resource "botyard_bot_member" "analyst" {
  bot_slug = botyard_bot.assistant.slug
  user_id  = "5f0c2a4e-9b1d-4e7a-8c3f-2d6b1a9e0f47"
}

# A co-owner can change the bot's access mode and member list in the app.
resource "botyard_bot_member" "lead" {
  bot_slug = botyard_bot.assistant.slug
  user_id  = "9a3e7c1b-2d4f-4b6a-8e0c-7f1d2a3b4c5d"
  role     = "owner"
}
