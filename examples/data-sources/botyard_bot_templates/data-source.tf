# List every bot template in the organization for discovery.
data "botyard_bot_templates" "all" {}

output "guided_setup_template_slugs" {
  value = [
    for t in data.botyard_bot_templates.all.bot_templates :
    t.slug if t.supports_guided_setup
  ]
}
