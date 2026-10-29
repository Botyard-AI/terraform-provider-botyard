# A bot's core identity. Creating this persists the bot's desired-state record
# and triggers provisioner reconciliation. The slug is derived from the name and
# is exported as a computed attribute.
resource "botyard_bot" "support" {
  name        = "Support Assistant"
  description = "Answers customer questions in the help center."

  # Hide skill and tool suggestions for this bot.
  suggestions_opt_out = true
}

# A bot with OpenClaw config overrides. `config` is a nested attribute, so it
# uses object syntax (`config = { ... }`). Only the fields you set are applied
# over OpenClaw's defaults; omitted fields keep their server default.
#
# The model a bot runs on is not part of `config`: it is derived from the bot's
# LLM credential links. Set it with `botyard_bot_credential_assignment`
# (`scope = "llm"`, ordered by `ordinal`, with an optional `default_model`).
resource "botyard_bot" "researcher" {
  name        = "Research Assistant"
  description = "Runs deep research tasks."

  config = {
    system_prompt_mode = "botyard"
    thinking_default   = "high"
    reasoning_default  = "stream"

    identity = {
      emoji = "🔬"
      theme = "dark"
    }

    heartbeat = {
      every            = "30m"
      light_context    = true
      isolated_session = true

      active_hours = {
        from_time = "09:00"
        to_time   = "17:00"
        timezone  = "America/New_York"
      }
    }

    compaction = {
      reserve_tokens            = 24000
      truncate_after_compaction = true
    }

    session = {
      write_lock_max_hold_ms = 300000
    }

    # Defer tool schemas behind a search surface instead of inlining them.
    tool_search = {
      mode = "auto"
    }
  }

  # Size the bot above its plan preset. Only the quantities set here are
  # managed; the others keep their current values. Changing them restarts the
  # bot.
  resources = {
    cpu_limit    = "2"
    memory_limit = "4Gi"
  }
}

# A bot created from a Bot Pack. The pack is installed when the bot is
# created; removing the `pack` block later uninstalls it and keeps the bot.
# `settings` and `connections` apply at creation only. Connection ids are the
# pack version's `pack_version_connection_id`s, as shown by the API's
# connections preview (`POST /v1/orgs/{org_id}/packs/connections/preview`).
data "botyard_pack_version" "crm" {
  slug = "crm-assistant"
}

resource "botyard_mcp_server" "crm" {
  # ... an org MCP server for the pack's CRM connection
}

resource "botyard_bot" "crm" {
  name = "CRM Assistant"

  pack = {
    pack_version_id = data.botyard_pack_version.crm.pack_version_id

    settings = [
      { path = "tool_search", choice = "custom", custom = { value = jsonencode({ mode = "auto" }) } },
    ]

    connections = [
      # Bind a Terraform-managed server...
      {
        pack_version_connection_id = "00000000-0000-0000-0000-000000000001"
        choice                     = "use_server"
        use_server                 = { mcp_server_id = botyard_mcp_server.crm.id }
      },
      # ...connect a new account with Runtime Vault references (not OAuth:
      # connect those in the app first, then use `use_server`)...
      {
        pack_version_connection_id = "00000000-0000-0000-0000-000000000002"
        choice                     = "create"
        create = {
          accept_metered = true
          setup = {
            kind            = "credential_form"
            credential_form = { vault_entry_refs = { api_key = "00000000-0000-0000-0000-0000000000aa" } }
          }
        }
      },
      # ...or leave one for later.
      {
        pack_version_connection_id = "00000000-0000-0000-0000-000000000003"
        choice                     = "leave_pending"
      },
    ]
  }
}

# Runtime placement (runtime_class, storage_class, runtime_privilege_mode),
# namespace, and control-plane state are read-only, exported for reference.
output "support_bot_slug" {
  value = botyard_bot.support.slug
}

# ---------------------------------------------------------------------------
# A hosted-native bot.
#
# Native bots run on Botyard's own agent loop instead of OpenClaw, and are
# selected by the hosting pair `hosting_type = "hosted"` + `harness =
# "botyard_native"`. They carry a different config shape, so they use the
# `native_config` block — declaring both `config` and `native_config`, or
# pairing a block with the wrong harness, fails at plan time.
#
# `harness` and `hosting_type` are immutable: changing either forces a new bot.
resource "botyard_bot" "triage" {
  name        = "Triage Agent"
  description = "Triages inbound reports on the native runtime."

  hosting_type = "hosted"
  harness      = "botyard_native"

  # A native bot's instructions (its soul) live in its bot files, not here.
  native_config = {
    tool_search = {
      mode                   = "auto"
      auto_threshold_percent = 40
    }

    # Declared, this block owns all three fields: one you leave out is reset
    # to the platform default.
    tool_images = {
      max_dimension_px = 1568
      default_detail   = "high"
    }
  }
}

# The model a native bot runs on is NOT set on the bot: the platform derives it
# from the bot's LLM credential links, so you set it here and read it back from
# `native_config.model_name`.
resource "botyard_bot_credential_assignment" "triage" {
  bot_slug = botyard_bot.triage.slug

  credentials = [
    {
      credential_id = "cred_anthropic_primary"
      scope         = "llm"
      ordinal       = 0
      default_model = "claude-opus-4-8"
    },
  ]
}

resource "botyard_bot_skill_assignment" "triage" {
  bot_slug = botyard_bot.triage.slug

  skill_ids = [
    "b1a7c0de-0000-4000-8000-000000000001",
  ]
}

# Read-only projections of the platform-owned fields: the model comes from the
# credential assignment above, and the host policy is normalized by the API.
output "triage_model" {
  value = botyard_bot.triage.native_config.model_name
}

output "triage_host_policy" {
  value = botyard_bot.triage.native_config.host_policy_json
}
