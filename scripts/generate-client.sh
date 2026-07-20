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
KEEP_TAGS=(bots mcp-servers)

# Paths dropped even though their tag is kept: the MCP catalog endpoints pull in
# McpCatalogFormField schemas whose inline enums trip an oapi-codegen
# duplicate-typename bug, and catalog-instantiation isn't part of the
# botyard_mcp_server resource. Re-include when the catalog data source lands.
EXCLUDE_PATHS=(/mcp-servers/catalog /mcp-servers/from-catalog)

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
