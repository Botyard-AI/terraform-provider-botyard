# Look up a single bot by slug within the configured organization.
data "botyard_bot" "example" {
  slug = "my-bot"
}

output "bot_id" {
  value = data.botyard_bot.example.id
}
