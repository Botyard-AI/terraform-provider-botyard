# Every recurring schedule on a bot, including ones the bot created itself and
# ones no Terraform configuration manages.
data "botyard_bot_schedules" "assistant" {
  bot_slug = "my-assistant"
}

output "next_runs" {
  value = {
    for s in data.botyard_bot_schedules.assistant.schedules :
    coalesce(s.title, s.id) => s.next_fire_at if s.enabled
  }
}
