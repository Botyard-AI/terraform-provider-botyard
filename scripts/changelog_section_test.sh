#!/usr/bin/env bash
# Tests for scripts/changelog-section.sh. Run from anywhere:
#   bash scripts/changelog_section_test.sh
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
script="$here/changelog-section.sh"
repo_changelog="$here/../CHANGELOG.md"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
failures=0

ok() { pass=$((pass + 1)); }
bad() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }

# expect_ok <name> <expected-stdout> <args...>
expect_ok() {
  local name="$1" want="$2"; shift 2
  local got
  if ! got="$(bash "$script" "$@" 2>"$tmp/err")"; then
    bad "$name: exited non-zero: $(cat "$tmp/err")"; return
  fi
  if [ "$got" != "$want" ]; then
    bad "$name: output mismatch"; diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") >&2 || true; return
  fi
  ok
}

# expect_fail <name> <stderr-substring> <args...>
expect_fail() {
  local name="$1" want="$2"; shift 2
  if bash "$script" "$@" >"$tmp/out" 2>"$tmp/err"; then
    bad "$name: expected failure, got success with: $(cat "$tmp/out")"; return
  fi
  if ! grep -qF -- "$want" "$tmp/err"; then
    bad "$name: stderr missing '$want': $(cat "$tmp/err")"; return
  fi
  if [ -s "$tmp/out" ]; then
    bad "$name: printed to stdout on failure: $(cat "$tmp/out")"; return
  fi
  ok
}

cat >"$tmp/CHANGELOG.md" <<'EOF'
# Changelog

## [Unreleased]

### Added

- Something not released yet.

## [1.2.0] - 2026-10-28

### Added

- Feature one. (#40)
  - Nested detail.

  ```terraform
  resource "x" "y" {}
  ```

### Fixed

- A fix. (#41)

## [1.1.0] - 2026-10-01

### Changed

- Last section before the links.

## [1.0.10] - 2026-09-01

### Added

- Must not be matched by 1.0.1.

## [1.0.1] - 2026-08-01

- Plain bullet, no sub-heading.

## [1.0.0] - 2026-07-01

### Added

## [0.9.0]

- No date.

## [0.8.0] - 2026-05-01

## [0.7.0] - 2026-04-01

- First copy.

## [0.7.0] - 2026-04-02

- Second copy.

## [0.6.0] - 2026-03-01

- Final section.

[Unreleased]: https://example.com/compare/v1.2.0...HEAD
[1.2.0]: https://example.com/compare/v1.1.0...v1.2.0
EOF
f="$tmp/CHANGELOG.md"

expect_ok "multi-subsection section, v prefix" "### Added

- Feature one. (#40)
  - Nested detail.

  \`\`\`terraform
  resource \"x\" \"y\" {}
  \`\`\`

### Fixed

- A fix. (#41)" v1.2.0 "$f"
expect_ok "no v prefix" "### Changed

- Last section before the links." 1.1.0 "$f"
expect_ok "1.0.1 is not confused with 1.0.10" "- Plain bullet, no sub-heading." v1.0.1 "$f"
expect_ok "last section stops at link references" "- Final section." v0.6.0 "$f"

expect_fail "missing version" "no \"## [9.9.9] - YYYY-MM-DD\" section" v9.9.9 "$f"
expect_fail "empty section" "section is empty" v0.8.0 "$f"
expect_fail "only sub-headings is empty" "section is empty" v1.0.0 "$f"
expect_fail "missing date" "must be \"## [0.9.0] - YYYY-MM-DD\"" v0.9.0 "$f"
expect_fail "duplicate heading" "more than one" v0.7.0 "$f"
expect_fail "Unreleased is not a version" "invalid version" Unreleased "$f"
expect_fail "pre-release rejected" "invalid version" v1.2.0-rc1 "$f"
expect_fail "leading zero rejected" "invalid version" v1.02.0 "$f"
expect_fail "missing file" "not found" v1.2.0 "$tmp/nope.md"

# The real CHANGELOG.md: it has an Unreleased section and every released tag's
# section extracts cleanly.
if ! grep -q '^## \[Unreleased\]$' "$repo_changelog"; then
  bad "CHANGELOG.md has no '## [Unreleased]' heading"
else
  ok
fi
while read -r v; do
  if bash "$script" "$v" "$repo_changelog" >/dev/null 2>"$tmp/err"; then ok; else bad "CHANGELOG.md $v: $(cat "$tmp/err")"; fi
done < <(grep -Eo '^## \[[0-9]+\.[0-9]+\.[0-9]+\]' "$repo_changelog" | grep -Eo '[0-9]+\.[0-9]+\.[0-9]+')

echo "changelog-section: $pass passed, $failures failed"
[ "$failures" -eq 0 ]
