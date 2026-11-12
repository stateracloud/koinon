#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/doc-meta-lint.sh against a throwaway docs directory (ADR 0115).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
lint=$here/doc-meta-lint.sh
work=$(mktemp -d "${TMPDIR:-/tmp}/doc-meta-lint-test.XXXXXX")
trap 'rm -rf "$work"' EXIT
fail=0

expect() { # expect <exit code> <description> [text in the output]
  local got=0 out
  out=$("$lint" "$work" 2>&1) || got=$?
  if [[ $got -ne $1 ]]; then echo "FAIL: $2 (exit $got, want $1): $out"; fail=1; return; fi
  if [[ -n ${3:-} && $out != *"$3"* ]]; then echo "FAIL: $2 (output lacks '$3'): $out"; fail=1; return; fi
  echo "ok: $2"
}

mkdir -p "$work/metadata" "$work/adr" "$work/research" "$work/spikes"
printf '# comment\nsecurity-audit  leak guard\nfrontend-ux  UI\n' >"$work/metadata/areas.txt"
printf '# comment\nleak-guard  x\nsecrets  x\nui  x\negress  x\naudit  x\nci  x\ndocs  x\n' >"$work/metadata/tags.txt"
printf '# 0000. T\n\n- Status: Proposed\n- Date: YYYY-MM-DD\n- Area: one\n- Tags: some\n' >"$work/adr/0000-template.md"
adr() { printf '# 0001. A\n\n- Status: Accepted\n- Date: 2026-11-11\n%s\n## Context\n\n- Area: ui\n' "$1" >"$work/adr/0001-a.md"; }
note() { printf '# Note\n\n%s\n## Findings\n' "$1" >"$work/research/n.md"; }
good_note='- Status: Complete
- Date: 2026-10-23
- Area: security-audit
- Tags: secrets'

adr $'- Area: security-audit\n- Tags: leak-guard, egress'
note "$good_note"
expect 0 "valid ADR and note"
cp "$work/research/n.md" "$work/research/README.md"; printf 'no header\n' >"$work/research/README.md"
expect 0 "README is skipped"

adr '- Tags: leak-guard'
expect 1 "ADR without Area" "needs exactly one '- Area:'"
adr $'- Area: security-audit'
expect 1 "ADR without Tags" "needs exactly one '- Tags:'"
adr $'- Area: backend\n- Tags: leak-guard'
expect 1 "unknown area" "Area 'backend' is not in"
adr $'- Area: security-audit\n- Tags: leak-guard, keys'
expect 1 "unknown tag" "tag 'keys' is not in"
adr $'- Area: security-audit\n- Tags: secrets, secrets'
expect 1 "repeated tag" "listed twice"
adr $'- Area: security-audit\n- Tags: leak-guard, secrets, ui, egress, audit, ci, docs'
expect 1 "seven tags" "one to six tags"
adr $'- Area: security-audit\n- Tags: leak-guard'

note "${good_note/Complete/Done}"
expect 1 "bad research status" "must start with Draft"
note "${good_note/2026-10-23/October 2026}"
expect 1 "bad research date" "must be YYYY-MM-DD"
note "${good_note/Complete/Superseded by [x](x.md)}"
expect 0 "Superseded by is a status"
printf '# Spike\n\nno header\n' >"$work/spikes/s.md"
expect 1 "spike without header" "spikes/s.md"
rm "$work/spikes/s.md"

printf '# 0000. T\n\n- Status: Proposed\n' >"$work/adr/0000-template.md"
expect 1 "template without the fields" "the template must name"
printf '# 0000. T\n\n- Area: one\n- Tags: some\n' >"$work/adr/0000-template.md"

printf 'Bad_Tag  x\n' >>"$work/metadata/tags.txt"
expect 1 "tag not kebab-case" "not lower-case kebab-case"

exit "$fail"
