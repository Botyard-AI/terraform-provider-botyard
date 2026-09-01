# Skills are imported by their slug (the API's URL identifier), which is derived
# from the name at creation and stays stable across renames. Only custom skills
# can be imported — the API refuses to update or delete platform-provided ones,
# so importing those fails by design.
terraform import botyard_skill.release_runbook release-runbook
