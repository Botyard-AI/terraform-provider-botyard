# Make one bot a member of another, e.g. so a coordinator bot can reach a
# restricted worker bot.
resource "botyard_bot_member_bot" "coordinator" {
  bot_slug      = botyard_bot.worker.slug
  member_bot_id = botyard_bot.coordinator.id
}
