# Quickstart: a runnable configuration.
#
# Creates a bot, a custom skill assigned to it, and a Runtime Vault entry the
# bot is allowed to lease. Run it against an organization you can experiment in:
#
#   export BOTYARD_API_KEY="byk_..."   # organization-scoped API key
#   export BOTYARD_ORG_ID="00000000-0000-0000-0000-000000000000"
#   terraform init && terraform apply
#
# `terraform destroy` removes everything it created. Requires Terraform 1.11+
# (`secret_value` is a write-only attribute).

terraform {
  required_version = ">= 1.11"

  required_providers {
    botyard = {
      source  = "Botyard-AI/botyard"
      version = "~> 0.4"
    }
  }
}

provider "botyard" {}

# The bot. Creating it triggers provisioning; the slug is derived from the name.
resource "botyard_bot" "assistant" {
  name        = "Terraform Quickstart Assistant"
  description = "Created by the terraform-provider-botyard quickstart."
}

# A custom skill: instructions the bot loads on demand.
resource "botyard_skill" "release_checklist" {
  name    = "Release Checklist"
  summary = "Steps to follow before announcing a release."

  files = [
    {
      filename = "SKILL.md"
      content  = <<-EOT
        # Release Checklist

        1. Confirm CI is green on the release commit.
        2. Check the changelog covers every user-facing change.
        3. Announce the release with a link to the changelog.
      EOT
    },
  ]
}

# Give the bot that skill. This resource owns the bot's full skill set.
resource "botyard_bot_skill_assignment" "assistant" {
  bot_slug  = botyard_bot.assistant.slug
  skill_ids = [botyard_skill.release_checklist.id]
}

# A Runtime Vault entry only this bot may lease at runtime.
resource "botyard_vault_secret" "status_page_url" {
  key_path     = "quickstart.status_page_url"
  display_name = "Status page URL"
  sensitivity  = "plain"
  secret_value = "https://status.example.com"
  bot_ids      = [botyard_bot.assistant.id]
}

output "bot_slug" {
  value = botyard_bot.assistant.slug
}
