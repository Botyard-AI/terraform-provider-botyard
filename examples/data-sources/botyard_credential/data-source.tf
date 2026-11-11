# Look up a credential by slug instead of hard-coding its UUID, which differs
# per organization.
#
# Every organization has the system-created `user-claude-subscription`
# credential. Listing it first in a bot's `llm` scope runs each person's turns
# on their own Claude subscription. It needs a Claude model, and must be at
# ordinal 0. A second `llm` credential below it is the bot's fallback for people
# without a connected Claude. Fallback routing is not live yet: until it is,
# those people get no answer from the bot.
data "botyard_credential" "user_claude" {
  slug = "user-claude-subscription"
}

variable "fallback_llm_credential_id" {
  type        = string
  description = "The bot's own LLM credential (managed credits or an API key) used when a person has no connected Claude."
}

resource "botyard_bot_credential_assignment" "per_user_claude" {
  bot_slug = botyard_bot.example.slug

  credentials = [
    {
      credential_id = data.botyard_credential.user_claude.credential_id
      scope         = "llm"
      ordinal       = 0
      default_model = "claude-opus-4-8"
    },
    {
      credential_id = var.fallback_llm_credential_id
      scope         = "llm"
      ordinal       = 1
    },
  ]
}
