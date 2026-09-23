# Imported skills are imported by slug. Every provenance attribute (commit_sha,
# source_url, ref, path, source_kind, imported_at) is read back, and `source` is
# rebuilt as `owner/repo[/path][#ref]`. Use that same form in configuration so
# the first plan is empty.
#
# A skill authored in Botyard (never imported) can be adopted too: it has no
# provenance, so the first apply fails until you set `force = true`, which
# replaces its content with `source` while keeping its id.
terraform import botyard_skill_import.deploy deploy
