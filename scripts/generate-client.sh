#!/usr/bin/env bash
# Regenerate internal/client/botyard.gen.go from the vendored OpenAPI spec.
#
# Pipeline (see scripts/openapi-normalize.py for the why):
#   1. normalize+prune the vendored 3.1 spec to the tags the provider covers
#   2. down-convert 3.1 -> 3.0 (openapi-down-convert via npx)
#   3. oapi-codegen -> internal/client/botyard.gen.go
#
# Requirements: python3, node/npx, and Go (go run fetches oapi-codegen).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLIENT_DIR="$ROOT/internal/client"
SPEC="$CLIENT_DIR/openapi.json"

# Tags the generated client covers. Widen as resources are added.
KEEP_TAGS=(bots mcp-servers secret-policies skills bot-tools credentials)

# Paths dropped even though their tag is kept:
#   - The MCP catalog endpoints pull in McpCatalogFormField schemas whose inline
#     enums trip an oapi-codegen duplicate-typename bug, and catalog
#     instantiation isn't part of the botyard_mcp_server resource.
#   - The bot-scoped secret-policies endpoints model a bot reading its own
#     variables (a different access pattern); the botyard_vault_secret resource
#     only needs the org-scoped policy CRUD + bot-links endpoints.
#   - The org-scoped /skills catalogue endpoints (authoring skills, not
#     assigning them) are a different surface from bot_skill_assignment, which
#     only touches the bot-scoped .../bots/{bot_slug}/skills assignment
#     endpoints. Re-include when a skill-catalogue resource is built.
#   - The `credentials` tag is broad: it covers the org-scoped credential CRUD
#     (create/list/get/update/delete/presets/oauth/test) AND the secret-bearing
#     bot-private credential paths AND the reorder / per-link-model bot paths.
#     The botyard_bot_credential_assignment resource only assigns EXISTING org
#     credentials to a bot, so it keeps just the bot-scoped assignment surface:
#     GET+PUT .../bots/{bot_slug}/credentials and DELETE
#     .../bots/{bot_slug}/credentials/{credential_id}. Everything else under the
#     tag is excluded — the org CRUD + presets/oauth/test are out of scope; the
#     bot-private create/delete carry raw api_key/oauth_token (secrets, deferred
#     to a future write-only/ephemeral resource); reorder is redundant (assign
#     sets ordinals directly); and the per-link model PATCH is deferred. Re-add
#     when a credential-management (secret-bearing) resource is built.
# Re-include either when the corresponding surface is built.
EXCLUDE_PATHS=(
  "/v1/orgs/{org_id}/mcp-servers/catalog"
  "/v1/orgs/{org_id}/mcp-servers/from-catalog"
  "/v1/orgs/{org_id}/bots/{bot_slug}/secret-policies"
  "/v1/orgs/{org_id}/bots/{bot_slug}/secret-policies/{policy_id}"
  "/v1/orgs/{org_id}/skills"
  "/v1/orgs/{org_id}/skills/search"
  "/v1/orgs/{org_id}/skills/{skill_slug}"
  "/v1/orgs/{org_id}/credentials"
  "/v1/orgs/{org_id}/credentials/presets"
  "/v1/orgs/{org_id}/credentials/{credential_id}"
  "/v1/orgs/{org_id}/credentials/{credential_id}/oauth/start"
  "/v1/orgs/{org_id}/credentials/{credential_id}/oauth/tokens"
  "/v1/orgs/{org_id}/credentials/{credential_id}/test"
  "/v1/orgs/{org_id}/bots/{bot_slug}/credentials/private"
  "/v1/orgs/{org_id}/bots/{bot_slug}/credentials/private/{credential_id}"
  "/v1/orgs/{org_id}/bots/{bot_slug}/credentials/reorder"
  "/v1/orgs/{org_id}/bots/{bot_slug}/credentials/{credential_id}/model"
)

OAPI_CODEGEN_VERSION="v2.4.1"
DOWN_CONVERT_VERSION="0.14.1"

tmp_norm="$(mktemp --suffix=.json)"
tmp_30="$(mktemp --suffix=.json)"
trap 'rm -f "$tmp_norm" "$tmp_30"' EXIT

echo ">> normalize + prune (tags: ${KEEP_TAGS[*]}; exclude: ${EXCLUDE_PATHS[*]})"
python3 "$ROOT/scripts/openapi-normalize.py" "$SPEC" "$tmp_norm" \
  --keep-tags "${KEEP_TAGS[@]}" --exclude-paths "${EXCLUDE_PATHS[@]}"

echo ">> down-convert 3.1 -> 3.0"
npx --yes "@apiture/openapi-down-convert@${DOWN_CONVERT_VERSION}" --input "$tmp_norm" --output "$tmp_30"

echo ">> generate client"
( cd "$CLIENT_DIR" && go run "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@${OAPI_CODEGEN_VERSION}" -config config.yaml "$tmp_30" )

echo ">> gofmt"
gofmt -w "$CLIENT_DIR/botyard.gen.go"
echo ">> done: $CLIENT_DIR/botyard.gen.go"
