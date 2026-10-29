#!/usr/bin/env bash
# Tests for scripts/changelog-section.sh. Run from anywhere:
#   bash scripts/changelog_section_test.sh
#
# The whole suite runs once per awk implementation found (mawk, gawk,
# busybox awk, BSD/one-true-awk), because they differ in how they treat
# escapes and regexes. Set AWKS to a space-separated list of awk binaries to
# override discovery (e.g. AWKS="/usr/bin/mawk /opt/gawk/bin/gawk").
set -euo pipefail

if [ -z "${CHANGELOG_TEST_AWK_RUN:-}" ]; then
  here="$(cd "$(dirname "$0")" && pwd)"
  awks="${AWKS:-}"
  if [ -z "$awks" ]; then
    for cand in mawk gawk original-awk nawk; do
      p="$(command -v "$cand" || true)"
      [ -n "$p" ] && awks="$awks $p"
    done
    if command -v busybox >/dev/null && busybox awk 'BEGIN{}' 2>/dev/null; then
      awks="$awks busybox"
    fi
  fi
  [ -n "$awks" ] || { echo "no awk implementation found" >&2; exit 1; }
  shim_root="$(mktemp -d)"
  trap 'rm -rf "$shim_root"' EXIT
  rc=0 n=0
  for a in $awks; do
    n=$((n + 1))
    shim="$shim_root/$n"
    mkdir -p "$shim"
    if [ "$a" = busybox ]; then
      printf '#!/bin/sh\nexec busybox awk "$@"\n' > "$shim/awk"
    else
      printf '#!/bin/sh\nexec %s "$@"\n' "$a" > "$shim/awk"
    fi
    chmod +x "$shim/awk"
    echo "--- awk: $a ($(PATH="$shim:$PATH" awk --version 2>&1 | head -n1 || true))"
    PATH="$shim:$PATH" CHANGELOG_TEST_AWK_RUN=1 bash "$here/$(basename "$0")" || rc=1
  done
  exit "$rc"
fi

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

## [4x5x6] - 2026-02-01

- Malformed separators must never match 4.5.6.

## [0.5.0] - 2026-01-01

- Has a duplicated link reference.

## [0.4.0] - 2025-12-01

- Has a non-https link reference.

[Unreleased]: https://example.com/compare/v1.2.0...HEAD
[1.2.0]: https://example.com/compare/v1.1.0...v1.2.0
[1.0.1]: https://example.com/compare/v1.0.0...v1.0.1
[0.6.0]: https://example.com/releases/tag/v0.6.0
[0.5.0]: https://example.com/a
[0.5.0]: https://example.com/b
[0.4.0]: http://example.com/insecure
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
# Regression (PR 37 review): gawk decoded a "\." passed via -v to ".", so a
# regex-built match accepted "## [1x2x0]" for 1.2.0. Matching is literal now.
expect_fail "malformed separators never match" "no \"## [4.5.6]" v4.5.6 "$f"

# --footer: the compare link is the version's committed link reference.
expect_ok "footer from link reference" "### Added

- Feature one. (#40)
  - Nested detail.

  \`\`\`terraform
  resource \"x\" \"y\" {}
  \`\`\`

### Fixed

- A fix. (#41)

**Full changes:** https://example.com/compare/v1.1.0...v1.2.0" --footer v1.2.0 "$f"
expect_ok "footer release-tag link" "- Final section.

**Full changes:** https://example.com/releases/tag/v0.6.0" --footer 0.6.0 "$f"
expect_ok "1.0.1 link not confused with 1.0.10" "- Plain bullet, no sub-heading.

**Full changes:** https://example.com/compare/v1.0.0...v1.0.1" --footer v1.0.1 "$f"
expect_fail "footer needs a link reference" "no \"[1.1.0]: <url>\" link reference" --footer v1.1.0 "$f"
expect_fail "footer rejects duplicate link reference" "more than one \"[0.5.0]: <url>\"" --footer v0.5.0 "$f"
expect_fail "footer rejects non-https link" "not an https URL" --footer v0.4.0 "$f"

# Regression (PR 37 review): the notes, footer included, must not depend on
# which tags exist when the release runs. Same file in a git repo, before and
# after adding an intervening tag: byte-identical output.
repo="$tmp/repo"
mkdir -p "$repo"
cp "$f" "$repo/CHANGELOG.md"
(
  cd "$repo"
  git init -q
  git -c user.name=t -c user.email=t@t add CHANGELOG.md
  git -c user.name=t -c user.email=t@t commit -qm init
  git tag v1.1.0
)
before="$(cd "$repo" && bash "$script" --footer v1.2.0 CHANGELOG.md)"
(cd "$repo" && git tag v1.1.5 && git tag v1.2.0)
after="$(cd "$repo" && bash "$script" --footer v1.2.0 CHANGELOG.md)"
if [ "$before" = "$after" ]; then ok; else bad "notes changed after adding tags"; fi

# The real CHANGELOG.md: it has an Unreleased section and every released tag's
# section extracts cleanly.
if ! grep -q '^## \[Unreleased\]$' "$repo_changelog"; then
  bad "CHANGELOG.md has no '## [Unreleased]' heading"
else
  ok
fi
while read -r v; do
  if bash "$script" --footer "$v" "$repo_changelog" >/dev/null 2>"$tmp/err"; then ok; else bad "CHANGELOG.md $v: $(cat "$tmp/err")"; fi
done < <(grep -Eo '^## \[[0-9]+\.[0-9]+\.[0-9]+\]' "$repo_changelog" | grep -Eo '[0-9]+\.[0-9]+\.[0-9]+')

echo "changelog-section: $pass passed, $failures failed"
[ "$failures" -eq 0 ]
