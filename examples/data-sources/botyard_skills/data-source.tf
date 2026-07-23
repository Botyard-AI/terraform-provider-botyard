# List the skill catalogue and collect the ids of every org-scoped skill.
data "botyard_skills" "all" {}

locals {
  org_skill_ids = [
    for s in data.botyard_skills.all.skills : s.id if s.scope == "org"
  ]
}
