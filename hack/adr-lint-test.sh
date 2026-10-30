#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/adr-lint.sh against throwaway ADR directories.
set -euo pipefail

lint=$(cd "$(dirname "$0")" && pwd)/adr-lint.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail=0

expect() { # expect <exit code> <description>
  local got=0
  "$lint" "$work" >/dev/null 2>&1 || got=$?
  if [[ $got -ne $1 ]]; then echo "FAIL: $2 (exit $got, want $1)"; fail=1; else echo "ok: $2"; fi
}

touch "$work/0000-template.md" "$work/0001-a.md" "$work/0002-b.md"
cat >"$work/README.md" <<'R'
| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-a.md) | A | Accepted |
| [0002](0002-b.md) | B | Accepted |
R
expect 0 "consistent directory"

touch "$work/0003-c.md"
expect 1 "ADR without an index row"
echo '| [0003](0003-c.md) | C | Accepted |' >>"$work/README.md"
expect 0 "row added"

touch "$work/0003-d.md"
expect 1 "duplicate number"
rm "$work/0003-d.md"

touch "$work/0005-e.md"
echo '| [0005](0005-e.md) | E | Accepted |' >>"$work/README.md"
expect 1 "gap in numbers"
rm "$work/0005-e.md"
sed -i.bak '/0005/d' "$work/README.md" && rm "$work/README.md.bak"
expect 0 "gap closed"

echo '| [0004](0004-gone.md) | G | Accepted |' >>"$work/README.md"
expect 1 "row for a missing file"
sed -i.bak '/0004/d' "$work/README.md" && rm "$work/README.md.bak"

echo '| [0002](0003-c.md) | mislabelled | Accepted |' >>"$work/README.md"
expect 1 "row text does not match its link"

exit "$fail"
