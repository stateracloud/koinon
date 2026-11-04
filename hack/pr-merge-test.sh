#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/pr-merge.sh offline: `gh` is a stub on PATH answering from files in $FAKE_DIR, and the three
# helper scripts (pr-wait, needs-e2e, claim) are stubs that record their calls.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/pr-merge-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
export FAKE_DIR="$tmp/fake" PATH="$tmp/bin:$PATH"
export PR_MERGE_WAIT="$tmp/bin/wait" PR_MERGE_NEEDS_E2E="$tmp/bin/needs" PR_MERGE_CLAIM="$tmp/bin/claim"
export PR_MERGE_REQUIRED="$tmp/required.txt"

printf '# comment\naggregator go\nquick checks\nexternal dco\nadvisory flavors\n' >"$PR_MERGE_REQUIRED"

cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "gh $*" >>"$FAKE_DIR/calls"
case "$1 $2" in
"pr view")
  if [[ "$*" == *"--json files"* ]]; then echo "hack/x.sh"; else cat "$FAKE_DIR/pr.json"; fi ;;
"pr merge" | "pr comment") ;;
"api repos/{owner}/{repo}/commits/"*"/status") cat "$FAKE_DIR/statuses" ;;
*) if [ "$1" = api ]; then cat "$FAKE_DIR/runs"; else exit 9; fi ;;
esac
SH
cat >"$tmp/bin/wait" <<'SH'
#!/usr/bin/env bash
echo "wait $*" >>"$FAKE_DIR/calls"
exit "${WAIT_RC:-0}"
SH
cat >"$tmp/bin/needs" <<'SH'
#!/usr/bin/env bash
cat >/dev/null
echo "needs $*" >>"$FAKE_DIR/calls"
exit "${NEEDS_RC:-1}"
SH
cat >"$tmp/bin/claim" <<'SH'
#!/usr/bin/env bash
echo "claim $*" >>"$FAKE_DIR/calls"
SH
chmod +x "$tmp/bin"/*

fail=0
setup() { # <isDraft> <runs rows, one JSON object per line>
  rm -rf "$FAKE_DIR"
  mkdir -p "$FAKE_DIR"
  printf '{"state":"OPEN","isDraft":%s,"headRefOid":"abc1234def","baseRefName":"main","closingIssuesReferences":[{"number":647}]}' "$1" >"$FAKE_DIR/pr.json"
  printf '%s\n' "$2" >"$FAKE_DIR/runs"
  printf '{"name":"dco","ts":"2026-11-01T10:00:00Z","v":"success"}\n' >"$FAKE_DIR/statuses"
}
run() {
  rc=0
  out=$("$repo/hack/pr-merge.sh" "$@" 2>&1) || rc=$?
}
check() { # <description> <want rc> <want grep on calls: merge|nomerge>
  local merged=nomerge
  grep -q '^gh pr merge' "$FAKE_DIR/calls" 2>/dev/null && merged=merge
  if [ "$rc" = "$2" ] && [ "$merged" = "$3" ]; then
    echo "ok: $1"
  else
    printf 'FAIL: %s\n  want rc=%s %s, got rc=%s %s\n  %s\n' "$1" "$2" "$3" "$rc" "$merged" "$out" >&2
    fail=1
  fi
}
r() { printf '{"name":"%s","ts":"%s","v":"%s"}' "$1" "$2" "$3"; }

green="$(r go 2026-11-01T10:00:00Z success)
$(r 'quick checks' 2026-11-01T10:00:00Z skipped)"

setup true "$green"
run 7
check "a draft is refused" 5 nomerge
grep -q '^wait' "$FAKE_DIR/calls" 2>/dev/null && { echo "FAIL: a draft is not waited on" >&2; fail=1; }

setup false "$(r go 2026-11-01T10:00:00Z success)
$(r 'quick checks' 2026-11-01T10:00:00Z failure)"
run 7
check "one red context is refused" 1 nomerge
grep -q 'NOT GREEN quick checks (failure)' <<<"$out" || { echo "FAIL: names the red context" >&2; fail=1; }

setup false "$(r go 2026-11-01T10:00:00Z success)"
run 7
check "a context that never reported is refused" 1 nomerge

setup false "$(r go 2026-11-01T10:00:00Z failure)
$(r go 2026-11-01T11:00:00Z success)
$(r 'quick checks' 2026-11-01T10:00:00Z success)"
run 7 --admin --approved-at 2026-11-04T01:00:00Z
check "all green (newest run per name) merges" 0 merge
grep -q '^gh pr merge 7 --merge --admin --match-head-commit abc1234def' "$FAKE_DIR/calls" || { echo "FAIL: admin merge pinned to the head" >&2; fail=1; }
grep -q '^claim review 647' "$FAKE_DIR/calls" || { echo "FAIL: claim review for the closed issue" >&2; fail=1; }
grep -q '^gh pr comment 7 --body .*read 3 required contexts on head .abc1234def' "$FAKE_DIR/calls" || { echo "FAIL: comment names the SHA and the count" >&2; fail=1; }

setup false "$green"
NEEDS_RC=0 run 7
check "without --admin it enables auto-merge" 0 merge
grep -q -- '--auto' "$FAKE_DIR/calls" || { echo "FAIL: --auto" >&2; fail=1; }
grep -q '^wait 7 --checks --require e2e' "$FAKE_DIR/calls" || { echo "FAIL: a needs-e2e change waits with --require e2e" >&2; fail=1; }

setup false "$green"
WAIT_RC=1 run 7
check "pr-wait failing stops the merge" 1 nomerge

setup false "$green"
run 7 8
check "usage" 64 nomerge

exit "$fail"
