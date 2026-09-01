# A custom skill: reusable instructions an agent loads on demand. The catalogue
# also holds platform-provided skills; those are read-only (use the
# `botyard_skill` data source for them).

# Inline content, for a short single-file skill.
resource "botyard_skill" "release_runbook" {
  name    = "Release Runbook"
  summary = "How to cut, verify and announce a Botyard release."

  files = [
    {
      filename = "SKILL.md"
      content  = <<-EOT
        # Release Runbook

        1. Cut a tag from `main` once CI is green.
        2. Wait for the release workflow to publish artifacts.
        3. Announce the release in #releases with the changelog link.
      EOT
    },
  ]
}

# Multi-file skill loaded from disk — the usual shape for anything substantial.
# Terraform owns the whole file set: dropping a file here deletes it.
resource "botyard_skill" "incident_response" {
  name    = "Incident Response"
  summary = "Triage, mitigate and write up a production incident."
  scope   = "org"

  files = [
    {
      filename = "SKILL.md"
      content  = file("${path.module}/skills/incident-response/SKILL.md")
    },
    {
      filename = "references/severity-matrix.md"
      content  = file("${path.module}/skills/incident-response/references/severity-matrix.md")
    },
  ]
}

# Assign the skill to a bot.
resource "botyard_bot_skill_assignment" "ops_bot" {
  bot_slug  = "ops-bot"
  skill_ids = [botyard_skill.incident_response.id]
}
