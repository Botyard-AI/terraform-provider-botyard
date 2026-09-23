# A skill imported from a GitHub repository and pinned to a tag. Bumping the
# ref (for example `#v1.2.0` to `#v1.3.0`) refreshes the skill in place: it
# keeps its id, so the bot assignment below survives the upgrade.
resource "botyard_skill_import" "deploy" {
  source = "acme/agent-skills/deploy#v1.2.0"
}

resource "botyard_bot_skill_assignment" "deploy" {
  bot_slug  = "release-bot"
  skill_ids = [botyard_skill_import.deploy.id]
}

# A named skill in a repository, imported under a different catalogue name to
# avoid a collision with an existing skill. Changing `name` re-imports.
resource "botyard_skill_import" "review" {
  source = "acme/agent-skills@code-review#v2"
  name   = "Code Review (upstream)"
}

# Private repositories need no credential here. The provider's API key must
# belong to an actor holding `skill:private_source.create`, and the org must
# have a GitHub integration connected.
resource "botyard_skill_import" "internal" {
  source = "acme/private-skills/oncall#2026.09"
}

# Someone edited this skill in Botyard, which detached it from its source.
# Applies fail by default. Set `force = true` for one apply to discard the edit
# and re-attach the skill, then remove it again.
resource "botyard_skill_import" "reattached" {
  source = "acme/agent-skills/triage#v3"
  force  = true
}

output "deploy_commit" {
  value = botyard_skill_import.deploy.commit_sha
}
