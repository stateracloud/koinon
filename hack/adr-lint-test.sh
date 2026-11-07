#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/adr-lint.sh and hack/adr-index.sh against throwaway ADR directories.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
lint=$here/adr-lint.sh
index=$here/adr-index.sh
work=$(mktemp -d "${TMPDIR:-/tmp}/adr-lint-test.XXXXXX")
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
adr 0002 Second "Accepted, amended by [0001](0001-a.md)" b
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

# Status values and links (old issue 525).
expect_out() { # expect_out <exit code> <description> <text in the output>
  local got=0 out
  out=$("$lint" "$work" 2>&1) || got=$?
  if [[ $got -ne $1 || $out != *"$3"* ]]; then
    echo "FAIL: $2 (exit $got, want $1 and '$3')"
    printf '%s\n' "$out" | sed 's/^/  | /'
    fail=1
  else
    echo "ok: $2"
  fi
}
reindex() { "$index" "$work" >/dev/null; }

adr 0009 Ninth "Agreed (by procoduck)" i
reindex
expect_out 1 "unknown status value" "0009-i.md: Status must start with"
adr 0009 Ninth "Acceptedish" i
reindex
expect_out 1 "a status value is a whole word" "0009-i.md: Status must start with"
adr 0009 Ninth "Accepted; amended 2026-11-02 (see below)" i
reindex
expect 0 "free text after the status value"
for s in Deprecated Rejected "Rejected (2026-11-02)"; do
  adr 0009 Ninth "$s" i
  reindex
  expect 0 "status $s"
done

adr 0009 Ninth "Proposed" i
reindex
expect_out 0 "still Proposed: a warning, not a failure" "warning: ADR 0009 is still Proposed"
printf '# 0000. T\n\n- Status: Proposed | Accepted | Superseded by [NNNN](NNNN-title.md)\n' >"$work/0000-template.md"
adr 0009 Ninth Accepted i
reindex
expect 0 "the template is not checked"
if "$lint" "$work" 2>&1 | grep -q warning; then echo "FAIL: no warning without a Proposed ADR"; fail=1; else echo "ok: no warning without a Proposed ADR"; fi

adr 0009 Ninth "Superseded by [0007](0007-g.md) and [0003](0003-c.md)" i
reindex
expect 0 "superseded, with links to files that exist, shown in the index"
if grep -qF '| [0009](0009-i.md) | Ninth | Superseded by' "$work/README.md"; then echo "ok: index row says superseded"; else echo "FAIL: index row"; fail=1; fi
adr 0009 Ninth "Superseded by [0042](0042-gone.md)" i
reindex
expect_out 1 "superseded by a file that does not exist" "link [0042](0042-gone.md): 0042-gone.md does not exist"
adr 0009 Ninth "Superseded by [0003](0007-g.md)" i
reindex
expect_out 1 "link text and file disagree" "the text says 0003, the file is 0007"
adr 0009 Ninth "Superseded by ADR 7" i
reindex
expect_out 1 "superseded without a link" "Superseded by names no ADR link"
adr 0009 Ninth "Superseded by [0007](0007-g.md)" i
reindex
sed -i.bak 's/^\(| \[0009\](0009-i.md) | Ninth | \)Superseded by.*/\1Accepted |/' "$work/README.md" && rm -f "$work/README.md.bak"
expect_out 1 "superseded, but the index says otherwise" "0009-i.md is superseded, but the README index does not say so"

adr 0009 Ninth Accepted i
printf -- '- Relates to: [0001](0001-a.md), [#1](https://example.invalid/1) (an issue), and\n  [0023](0023-observability.md) on the next line\n' >>"$work/0009-i.md"
reindex
expect_out 1 "Relates to: a link on a continuation line to a missing file" "link [0023](0023-observability.md): 0023-observability.md does not exist"
sed -i.bak 's/\[0023\](0023-observability.md)/[0007](0007-g.md#section)/' "$work/0009-i.md" && rm -f "$work/0009-i.md.bak"
reindex
expect 0 "Relates to: links that exist (an anchor is fine; issue links are not ADRs)"
printf '\nBody text links [0099](0099-nowhere.md) and is not checked.\n' >>"$work/0009-i.md"
reindex
expect 0 "links outside Status and Relates to are not checked"

exit "$fail"
