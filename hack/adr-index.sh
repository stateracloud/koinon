#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Generates the index table of docs/adr/README.md from each ADR's header: the title of the first line
# (`# NNNN. Title`) and the `- Status:` line. Everything above `## Index` in the README is kept.
# Nobody edits the table by hand, so two pull requests that each add an ADR no longer conflict on it
# (they still conflict on a number: reserve it with `hack/claim.sh adr`).
#
# Usage: hack/adr-index.sh [--check] [adr-dir]    (default: write docs/adr/README.md)
#   --check   exit 1 when the README is not what this script would write (CI: adr-lint)
set -euo pipefail

check=0
if [[ ${1:-} == --check ]]; then check=1; shift; fi
dir=${1:-$(cd "$(dirname "$0")/.." && pwd)/docs/adr}
readme=$dir/README.md
[[ -r $readme ]] || { echo "adr-index: $readme is missing" >&2; exit 1; }

table() {
  echo '| ADR | Title | Status |'
  echo '| --- | --- | --- |'
  local f n title status
  for f in $(cd "$dir" && /bin/ls | grep -E '^[0-9]{4}-.*\.md$' | grep -v '^0000-' | sort); do
    n=${f:0:4}
    title=$(sed -n '1s/^# [0-9]\{4\}\. *//p' "$dir/$f")
    status=$(sed -n 's/^- Status: *//p' "$dir/$f" | head -1)
    [[ -n $title ]] || { echo "adr-index: $f has no '# $n. Title' first line" >&2; return 1; }
    [[ -n $status ]] || { echo "adr-index: $f has no '- Status:' line" >&2; return 1; }
    title=${title//|/\\|}
    status=${status//|/\\|}
    echo "| [$n]($f) | $title | $status |"
  done
}

want=$(mktemp)
trap 'rm -f "$want"' EXIT
{
  sed -n '1,/^## Index$/p' "$readme"
  echo
  table
} >"$want"

if ((check)); then
  if diff -u "$readme" "$want" >/dev/null; then echo "adr-index: README index is current"; exit 0; fi
  echo "adr-index: docs/adr/README.md is stale; run hack/adr-index.sh and commit the result" >&2
  diff -u "$readme" "$want" | head -20 >&2 || true
  exit 1
fi
cp "$want" "$readme"
echo "adr-index: wrote $readme"
