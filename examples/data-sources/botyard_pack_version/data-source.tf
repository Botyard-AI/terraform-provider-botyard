# Read the latest version of a pack by slug...
data "botyard_pack_version" "crm" {
  slug = "crm-assistant"
}

# ...or pin an exact version by id.
data "botyard_pack_version" "crm_pinned" {
  pack_version_id = "00000000-0000-0000-0000-000000000000"
}

# botyard_bot_skill_assignment is exclusive, so a bot's own skills must be
# merged with the pack's. Otherwise the pack's skills would be removed.
resource "botyard_bot_skill_assignment" "example" {
  bot_slug  = botyard_bot.example.slug
  skill_ids = setunion(data.botyard_pack_version.crm.skill_ids, [data.botyard_skill.extra.id])
}
