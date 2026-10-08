---
page_title: "Botyard Provider"
description: |-
  The Botyard provider manages Botyard platform resources through the public API. Authenticate with an organization-scoped API key; every resource is scoped to the configured organization.
---

# Botyard Provider

[Botyard](https://botyard.io) is a platform for running shared, governed AI
agents ("bots") that belong to the company rather than to one person's laptop.
Bots run in managed runtimes, use organization-owned credentials and Runtime
Vault secrets, and are equipped with skills, tools and MCP servers that an
organization controls centrally. The platform documentation lives at
[docs.botyard.io](https://docs.botyard.io).

The Botyard provider manages Botyard platform resources through the public API. Authenticate with an organization-scoped API key; every resource is scoped to the configured organization.

See [Manage Botyard with Terraform](https://docs.botyard.io/docs/terraform) in
the Botyard docs for a walkthrough. The provider source is at
[github.com/Botyard-AI/terraform-provider-botyard](https://github.com/Botyard-AI/terraform-provider-botyard).

## Supported resources

The provider currently supports the following resources. Other Botyard
features are not yet managed through Terraform; use the Botyard app or API for
those.

| Resource | Manages | Concept docs |
| --- | --- | --- |
| [`botyard_bot`](https://registry.terraform.io/providers/Botyard-AI/botyard/latest/docs/resources/bot) | A bot's identity and configuration overrides | [Getting started](https://docs.botyard.io/docs/getting-started) |
| [`botyard_bot_credential_assignment`](https://registry.terraform.io/providers/Botyard-AI/botyard/latest/docs/resources/bot_credential_assignment) | Which organization credentials a bot uses, per scope | [Provider credentials](https://docs.botyard.io/docs/provider-credentials) |
| [`botyard_bot_skill_assignment`](https://registry.terraform.io/providers/Botyard-AI/botyard/latest/docs/resources/bot_skill_assignment) | Which skills are assigned to a bot | — |
| [`botyard_bot_tool_assignment`](https://registry.terraform.io/providers/Botyard-AI/botyard/latest/docs/resources/bot_tool_assignment) | Which tools are assigned to a bot | — |
| [`botyard_skill`](https://registry.terraform.io/providers/Botyard-AI/botyard/latest/docs/resources/skill) | An organization skill and its files | — |
| [`botyard_mcp_server`](https://registry.terraform.io/providers/Botyard-AI/botyard/latest/docs/resources/mcp_server) | An organization MCP server | [MCP setup templates](https://docs.botyard.io/docs/mcp-setup-templates) |
| [`botyard_mcp_server_member`](https://registry.terraform.io/providers/Botyard-AI/botyard/latest/docs/resources/mcp_server_member) | One member of an MCP server's member list | [MCP setup templates](https://docs.botyard.io/docs/mcp-setup-templates) |
| [`botyard_vault_secret`](https://registry.terraform.io/providers/Botyard-AI/botyard/latest/docs/resources/vault_secret) | A Runtime Vault secret and its access rules | [Runtime Vault](https://docs.botyard.io/docs/runtime-vault) |

Data sources are available for looking up bots, bot templates, credentials, MCP
servers, skills and tools; see the navigation for the list.

## Example Usage

```terraform
terraform {
  required_providers {
    botyard = {
      source  = "Botyard-AI/botyard"
      version = "~> 0.4"
    }
  }
}

# Authenticate with an organization-scoped API key. Prefer environment
# variables so the key never lands in Terraform configuration or state:
#
#   export BOTYARD_API_KEY="byk_..."
#   export BOTYARD_ORG_ID="00000000-0000-0000-0000-000000000000"
#   export BOTYARD_ENDPOINT="https://api.botyard.io" # optional; this is the default
provider "botyard" {}
```

## Quickstart

This configuration creates a bot, an organization skill, assigns the skill to
the bot, and stores a Runtime Vault secret the bot can read. It runs end to end
with `terraform init && terraform apply` once `BOTYARD_API_KEY` and
`BOTYARD_ORG_ID` are set, and `terraform destroy` removes everything it
created.

```terraform
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
```

<!-- schema generated by tfplugindocs -->
## Schema

### Optional

- `api_key` (String, Sensitive) Organization-scoped Botyard API key (`byk_...`). May also be set via the `BOTYARD_API_KEY` environment variable. Prefer the environment variable so the key does not land in Terraform configuration or state.
- `endpoint` (String) Base URL of the Botyard API. May also be set via the `BOTYARD_ENDPOINT` environment variable. Defaults to `https://api.botyard.io`.
- `org_id` (String) Botyard organization ID that resources are managed within. May also be set via the `BOTYARD_ORG_ID` environment variable.
