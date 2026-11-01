resource "botyard_bot" "assistant" {
  name         = "Native Assistant"
  hosting_type = "hosted"
  harness      = "botyard_native"
}

# Manage the full soul, not native_config. External edits (including pack
# changes) are detected on refresh and overwritten on the next apply.
resource "botyard_bot_soul" "assistant" {
  bot_slug = botyard_bot.assistant.slug
  content  = file("${path.module}/SOUL.md")
}

# Removing this resource leaves the live soul and its history intact.
# Native soul content must contain non-whitespace text; it cannot be cleared.
