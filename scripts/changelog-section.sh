#!/usr/bin/env bash
# Print the body of one version's section from CHANGELOG.md (Keep a Changelog).
#
#   scripts/changelog-section.sh <version> [changelog-path]
#
# <version> may carry a leading "v" (v0.6.0 and 0.6.0 are the same). The
# section is everything between "## [X.Y.Z] - YYYY-MM-DD" and the next "## "
# heading (or the trailing link-reference block), with surrounding blank lines
# trimmed. The heading itself is not printed.
#
# Fails (exit 1, reason on stderr) when the heading is missing, appears more
# than once, has no YYYY-MM-DD date, or the section has no content besides
# "###" sub-headings. The release workflow runs this BEFORE tagging, so a
# release without notes never gets a tag.
set -euo pipefail

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: $0 <version> [changelog-path]" >&2
  exit 2
fi

version="${1#v}"
changelog="${2:-CHANGELOG.md}"

if ! printf '%s' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
  echo "invalid version '$1' (expected [v]MAJOR.MINOR.PATCH)" >&2
  exit 2
fi
if [ ! -f "$changelog" ]; then
  echo "$changelog not found" >&2
  exit 1
fi

# Dots are the only regex metacharacter a validated version can contain.
escaped="${version//./\\.}"

awk -v ver="$version" -v esc="$escaped" -v file="$changelog" '
  function fail(msg) { print file ": " msg > "/dev/stderr"; failed = 1; exit 1 }
  # Heading for this version: "## [X.Y.Z]" followed by end of line or a space.
  $0 ~ ("^## \\[" esc "\\]( |$)") {
    found++
    if (found > 1) fail("more than one \"## [" ver "]\" heading")
    if ($0 !~ ("^## \\[" esc "\\] - [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9][ \t]*$"))
      fail("heading \"" $0 "\" must be \"## [" ver "] - YYYY-MM-DD\"")
    in_section = 1
    next
  }
  in_section && (/^## / || /^\[[^]]+\]: /) { in_section = 0 }
  in_section { lines[++n] = $0 }
  END {
    if (failed) exit 1
    if (!found) fail("no \"## [" ver "] - YYYY-MM-DD\" section; move the Unreleased entries into it before releasing")
    first = 1; while (first <= n && lines[first] ~ /^[ \t]*$/) first++
    last = n;  while (last >= first && lines[last] ~ /^[ \t]*$/) last--
    content = 0
    for (i = first; i <= last; i++)
      if (lines[i] !~ /^[ \t]*$/ && lines[i] !~ /^###/) content = 1
    if (!content) fail("the \"## [" ver "]\" section is empty")
    for (i = first; i <= last; i++) print lines[i]
  }
' "$changelog"
