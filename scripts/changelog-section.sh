#!/usr/bin/env bash
# Print the body of one version's section from CHANGELOG.md (Keep a Changelog).
#
#   scripts/changelog-section.sh [--footer] <version> [changelog-path]
#
# <version> may carry a leading "v" (v0.6.0 and 0.6.0 are the same). The
# section is everything between "## [X.Y.Z] - YYYY-MM-DD" and the next "## "
# heading (or the trailing link-reference block), with surrounding blank lines
# trimmed. The heading itself is not printed.
#
# --footer appends "**Full changes:** <url>", where <url> is the version's
# committed link reference ("[X.Y.Z]: <url>") at the bottom of the file. The
# footer therefore depends only on the file at the released commit, never on
# which tags exist when the release runs, so a resumed run is byte-identical.
#
# Fails (exit 1, reason on stderr) when the heading is missing, appears more
# than once, has no YYYY-MM-DD date, or the section has no content besides
# "###" sub-headings; with --footer, also when the link reference is missing,
# duplicated or not an https URL. The release workflow runs this BEFORE
# tagging, so a release without notes never gets a tag.
#
# Versions are compared as literal strings, never as regexes, so awk
# implementations' differing escape handling cannot loosen the match.
set -euo pipefail

footer=0
if [ "${1:-}" = "--footer" ]; then
  footer=1
  shift
fi

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: $0 [--footer] <version> [changelog-path]" >&2
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

awk -v ver="$version" -v file="$changelog" -v footer="$footer" '
  function fail(msg) { print file ": " msg > "/dev/stderr"; failed = 1; exit 1 }
  # Literal "[<ver>]" prefix test: no regex is built from the version.
  function starts_with(s, p) { return substr(s, 1, length(p)) == p }

  # Heading for this version: "## [X.Y.Z]" followed by end of line or a space.
  starts_with($0, "## [" ver "]") {
    rest = substr($0, length("## [" ver "]") + 1)
    if (rest == "" || substr(rest, 1, 1) == " ") {
      found++
      if (found > 1) fail("more than one \"## [" ver "]\" heading")
      if (rest !~ /^ - [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9][ \t]*$/)
        fail("heading \"" $0 "\" must be \"## [" ver "] - YYYY-MM-DD\"")
      in_section = 1
      next
    }
  }
  in_section && (/^## / || /^\[[^]]+\]: /) { in_section = 0 }
  in_section { lines[++n] = $0 }

  # Link reference for this version: "[X.Y.Z]: <url>".
  starts_with($0, "[" ver "]: ") {
    links++
    link = substr($0, length("[" ver "]: ") + 1)
    sub(/[ \t]+$/, "", link)
  }

  END {
    if (failed) exit 1
    if (!found) fail("no \"## [" ver "] - YYYY-MM-DD\" section; move the Unreleased entries into it before releasing")
    first = 1; while (first <= n && lines[first] ~ /^[ \t]*$/) first++
    last = n;  while (last >= first && lines[last] ~ /^[ \t]*$/) last--
    content = 0
    for (i = first; i <= last; i++)
      if (lines[i] !~ /^[ \t]*$/ && lines[i] !~ /^###/) content = 1
    if (!content) fail("the \"## [" ver "]\" section is empty")
    if (footer) {
      if (links == 0) fail("no \"[" ver "]: <url>\" link reference at the bottom of the file")
      if (links > 1) fail("more than one \"[" ver "]: <url>\" link reference")
      if (link !~ /^https:\/\/[^ \t]+$/) fail("link reference for [" ver "] is not an https URL: \"" link "\"")
    }
    for (i = first; i <= last; i++) print lines[i]
    if (footer) { print ""; print "**Full changes:** " link }
  }
' "$changelog"
