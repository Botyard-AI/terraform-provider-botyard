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
KEEP_TAGS=(bots mcp-servers secret-policies skills bot-tools credentials tools bot-templates)

# Paths dropped even though their tag is kept:
#   - The MCP catalog endpoints pull in McpCatalogFormField schemas whose inline
#     enums trip an oapi-codegen duplicate-typename bug, and catalog
#     instantiation isn't part of the botyard_mcp_server resource.
#   - The bot-scoped secret-policies endpoints model a bot reading its own
#     variables (a different access pattern); the botyard_vault_secret resource
#     only needs the org-scoped policy CRUD + bot-links endpoints.
#   - The org-scoped /skills and /credentials LIST endpoints (GET) back the
#     read-only botyard_skills / botyard_skill / botyard_credentials discovery
#     data sources, so the exact list paths are KEPT. Their sibling POST create
#     operations on the SAME path are dropped via EXCLUDE_OPERATIONS below (the
#     data sources only read; create_credentials in particular carries raw
#     api_key/oauth_token in CredentialCreate — keeping the generated client
#     read-only avoids pulling that secret-bearing schema in). The remaining
#     skills/credentials sub-paths stay excluded:
#       * /skills/search + /skills/{skill_slug} — authoring/single-skill CRUD;
#         the singular botyard_skill data source filters the list by slug.
#       * org credential CRUD-by-id + presets/oauth/test; the secret-bearing
#         bot-private create/delete; reorder; the per-link model PATCH — all out
#         of scope for read-only discovery (see the credential_assignment notes).
# Re-include either when the corresponding (authoring/secret-bearing) surface is
# built.
EXCLUDE_PATHS=(
  "/v1/orgs/{org_id}/mcp-servers/catalog"
  "/v1/orgs/{org_id}/mcp-servers/from-catalog"
  "/v1/orgs/{org_id}/bots/{bot_slug}/secret-policies"
  "/v1/orgs/{org_id}/bots/{bot_slug}/secret-policies/{policy_id}"
  "/v1/orgs/{org_id}/skills/search"
  "/v1/orgs/{org_id}/skills/{skill_slug}"
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

# Write operations dropped from an otherwise-kept path: the discovery data
# sources read the /skills and /credentials list endpoints (GET) but never
# create, so their POST create operations are excluded to keep the generated
# client read-only for these catalogs (and to avoid pulling the secret-bearing
# CredentialCreate schema).
EXCLUDE_OPERATIONS=(
  "create_skill_v1_orgs__org_id__skills_post"
  "create_credentials_v1_orgs__org_id__credentials_post"
)

OAPI_CODEGEN_VERSION="v2.4.1"
DOWN_CONVERT_VERSION="0.14.1"

tmp_norm="$(mktemp --suffix=.json)"
tmp_30="$(mktemp --suffix=.json)"
trap 'rm -f "$tmp_norm" "$tmp_30"' EXIT

echo ">> normalize + prune (tags: ${KEEP_TAGS[*]}; exclude paths: ${EXCLUDE_PATHS[*]}; exclude ops: ${EXCLUDE_OPERATIONS[*]})"
python3 "$ROOT/scripts/openapi-normalize.py" "$SPEC" "$tmp_norm" \
  --keep-tags "${KEEP_TAGS[@]}" \
  --exclude-paths "${EXCLUDE_PATHS[@]}" \
  --exclude-operations "${EXCLUDE_OPERATIONS[@]}"

echo ">> down-convert 3.1 -> 3.0"
npx --yes "@apiture/openapi-down-convert@${DOWN_CONVERT_VERSION}" --input "$tmp_norm" --output "$tmp_30"

echo ">> generate client"
( cd "$CLIENT_DIR" && go run "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@${OAPI_CODEGEN_VERSION}" -config config.yaml "$tmp_30" )

echo ">> gofmt"
gofmt -w "$CLIENT_DIR/botyard.gen.go"
echo ">> done: $CLIENT_DIR/botyard.gen.go"
