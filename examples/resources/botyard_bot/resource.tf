# A bot's core identity. Creating this persists the bot's desired-state record
# and triggers provisioner reconciliation. The slug is derived from the name and
# is exported as a computed attribute.
resource "botyard_bot" "support" {
  name        = "Support Assistant"
  description = "Answers customer questions in the help center."
}

# Runtime placement (runtime_class, storage_class, runtime_privilege_mode),
# namespace, and control-plane state are read-only, exported for reference.
output "support_bot_slug" {
  value = botyard_bot.support.slug
}
