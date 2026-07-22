# Assign existing organization credentials to a bot. Assignments are scoped and
# ordered within a scope by an explicit ordinal (0 = highest priority).
#
# This resource takes exclusive ownership of the scopes it declares: any
# credential assigned to one of these scopes outside Terraform is removed on
# apply. It assigns existing credentials only — it does not create them.

resource "botyard_bot_credential_assignment" "example" {
  bot_slug = botyard_bot.example.slug

  credentials = [
    # Primary LLM credential, tried first.
    {
      credential_id = "cred_anthropic_primary"
      scope         = "llm"
      ordinal       = 0
      default_model = "claude-opus-4-8"
    },
    # Fallback LLM credential.
    {
      credential_id = "cred_openai_fallback"
      scope         = "llm"
      ordinal       = 1
    },
    # Web search credential.
    {
      credential_id = "cred_brave_search"
      scope         = "web_search"
      ordinal       = 0
    },
  ]
}
