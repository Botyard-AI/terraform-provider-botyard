# A bot's core identity. Creating this persists the bot's desired-state record
# and triggers provisioner reconciliation. The slug is derived from the name and
# is exported as a computed attribute.
resource "botyard_bot" "support" {
  name        = "Support Assistant"
  description = "Answers customer questions in the help center."
}

# A bot with OpenClaw config overrides. `config` is a nested attribute, so it
# uses object syntax (`config = { ... }`). Only the fields you set are applied
# over OpenClaw's defaults; omitted fields keep their server default.
resource "botyard_bot" "researcher" {
  name        = "Research Assistant"
  description = "Runs deep research tasks."

  config = {
    system_prompt_mode = "botyard"
    thinking_default   = "high"
    reasoning_default  = "stream"

    model = {
      primary = {
        provider = "botyard"
        model    = "gpt-5.4"
      }
    }

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
  }
}

# Runtime placement (runtime_class, storage_class, runtime_privilege_mode),
# namespace, and control-plane state are read-only, exported for reference.
output "support_bot_slug" {
  value = botyard_bot.support.slug
}
