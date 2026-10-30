#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/adr-lint.sh and hack/adr-index.sh against throwaway ADR directories.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
lint=$here/adr-lint.sh
index=$here/adr-index.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0

expect() { # expect <exit code> <description>
  local got=0
  "$lint" "$work" >/dev/null 2>&1 || got=$?
  if [[ $got -ne $1 ]]; then echo "FAIL: $2 (exit $got, want $1)"; fail=1; else echo "ok: $2"; fi
}
adr() { printf '# %s. %s\n\n- Status: %s\n' "$1" "$2" "${3:-Accepted}" >"$work/$1-$4.md"; }

printf '# 0000. T\n\n- Status: Proposed\n' >"$work/0000-template.md"
adr 0001 "First | pipe" Accepted a
adr 0002 Second "Accepted, amended by [0003](0003-c.md)" b
printf '# ADRs\n\ntext\n\n## Index\n\nold\n' >"$work/README.md"
expect 1 "stale index"
"$index" "$work" >/dev/null
expect 0 "generated index"
if grep -qF '| [0001](0001-a.md) | First \| pipe | Accepted |' "$work/README.md"; then echo "ok: row rendered"; else echo "FAIL: row rendered"; fail=1; fi
if grep -q '^text$' "$work/README.md"; then echo "ok: prose above the index kept"; else echo "FAIL: prose kept"; fail=1; fi

adr 0003 Third Accepted c
expect 1 "new ADR not in the index"
"$index" "$work" >/dev/null
expect 0 "index regenerated"

adr 0007 Seventh Accepted g
"$index" "$work" >/dev/null
expect 0 "gaps are allowed"

adr 0003 Dup Accepted d
"$index" "$work" >/dev/null
expect 1 "duplicate number"
rm "$work/0003-d.md"
"$index" "$work" >/dev/null

echo 'edited by hand' >>"$work/README.md"
expect 1 "hand-edited index"
"$index" "$work" >/dev/null

printf 'no header\n' >"$work/0008-x.md"
if "$index" "$work" >/dev/null 2>&1; then echo "FAIL: header required"; fail=1; else echo "ok: header required"; fi
rm "$work/0008-x.md"

exit "$fail"
