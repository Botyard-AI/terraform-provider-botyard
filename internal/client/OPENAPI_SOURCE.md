# Vendored public API provenance

The committed `openapi.json` is copied, without hand edits, from
`Botyard-AI/botyard` commit `62672fdc42472a69103c50336702b69552527fe3`,
`docs-site/content/api/openapi.json` (API prerequisite PR
https://github.com/Botyard-AI/botyard/pull/5152).

The file was exported through the monorepo's canonical OpenAPI generator.
The client is regenerated through `make generate` /
`scripts/generate-client.sh`, including its existing normalization and pruning.

The workforce membership resource requires this API's immutable-board-ID
preconditions. It first verifies a guarded read acknowledgement and sends the
expected ID in the strict body of every write. Old API replicas reject the
unknown body field before mutation. Query-only writes do not provide that
old-server fence.

This spec sync depends on the existing pack-skill decoder change in provider
PR https://github.com/Botyard-AI/terraform-provider-botyard/pull/54; that change
is inherited as the PR's base, not reimplemented in the workforce diff.
Deployment of the API and release of the provider remain separate human gates.
