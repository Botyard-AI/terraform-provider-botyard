# A weekday check-in: wake the bot with a prompt at 09:00 Copenhagen time.
# conversation_id is left out, so Botyard creates a dedicated conversation and
# the resource adopts it.
resource "botyard_bot_schedule" "standup" {
  bot_slug = botyard_bot.assistant.slug
  title    = "Weekday stand-up"

  cron = {
    cron     = "0 9 * * 1-5"
    timezone = "Europe/Copenhagen"
  }

  prompt = "Post the stand-up summary for yesterday's merged PRs."
}

# A command on the bot's runtime every 15 minutes. Command schedules need the
# `schedule:schedule_command.create` / `.update` permission on the API key's
# role, on top of the schedule permissions.
resource "botyard_bot_schedule" "sync" {
  bot_slug                 = botyard_bot.assistant.slug
  title                    = "Upstream sync"
  max_consecutive_failures = 5 # auto-pause after 5 failures; the next apply resumes it

  interval = {
    interval_seconds = 900
    anchor           = "2026-01-01T00:00:00Z"
  }

  command = {
    command           = "./scripts/sync.sh"
    cwd               = "jobs"
    timeout_seconds   = 600
    log_path          = "/home/openclaw/logs/sync.log"
    concurrency_group = "sync"

    # Values come from the Runtime Vault at each run; only the key path is here.
    secret_env = [{ name = "SYNC_TOKEN", secret_key_path = "sync.token" }]

    on_outcome = {
      failure = {
        retry = { max = 2, backoff_seconds = 60 }
        then  = { wake_bot = { message = "Sync failed with exit code {exit_code}:\n{log_tail}", include_log_tail = 20 } }
      }
    }
  }
}
