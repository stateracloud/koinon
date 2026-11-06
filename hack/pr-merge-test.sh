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
export PR_MERGE_THREADS="$tmp/bin/threads"
export PR_MERGE_REQUIRED="$tmp/required.txt"

printf '# comment\naggregator go\nquick checks\nexternal dco\nadvisory flavors\n' >"$PR_MERGE_REQUIRED"

cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "gh $*" >>"$FAKE_DIR/calls"
case "$1 $2" in
"pr view")
  sed "s/\"mergeStateStatus\":\"CLEAN\"/\"mergeStateStatus\":\"${MSTATE:-CLEAN}\"/" "$FAKE_DIR/pr.json" ;;
"pr merge") exit "${MERGE_RC:-0}" ;;
"pr comment") ;;
"api repos/{owner}/{repo}/pulls/"*"/files")
  [ -f "$FAKE_DIR/files" ] || exit 9
  cat "$FAKE_DIR/files" ;;
"api repos/{owner}/{repo}/commits/"*"/status") cat "$FAKE_DIR/statuses" ;;
*) if [ "$1" = api ]; then cat "$FAKE_DIR/runs"; else exit 9; fi ;;
esac
SH
cat >"$tmp/bin/git" <<'SH'
#!/usr/bin/env bash
[ "$1" = ls-remote ] && [ -f "$FAKE_DIR/remote" ] || exit 2
printf '%s\t%s\n' "$(cat "$FAKE_DIR/remote")" "$3"
SH
cat >"$tmp/bin/wait" <<'SH'
#!/usr/bin/env bash
echo "wait $*" >>"$FAKE_DIR/calls"
if [ -f "$FAKE_DIR/close-after-wait" ]; then sed -i.bak 's/"OPEN"/"CLOSED"/' "$FAKE_DIR/pr.json"; fi
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
if [ "$1" = mine ] && [ -f "$FAKE_DIR/mine" ]; then cat "$FAKE_DIR/mine"; fi
SH
cat >"$tmp/bin/threads" <<'SH'
#!/usr/bin/env bash
echo "threads $*" >>"$FAKE_DIR/calls"
[ "${THREADS_RC:-0}" != 7 ] || echo "UNRESOLVED hack/x.sh:42 deepsource-io id=PRRT_2"
exit "${THREADS_RC:-0}"
SH
chmod +x "$tmp/bin"/*

fail=0
setup() { # <isDraft> <runs rows, one JSON object per line>
  rm -rf "$FAKE_DIR"
  mkdir -p "$FAKE_DIR"
  printf '{"state":"OPEN","isDraft":%s,"headRefOid":"abc1234def","headRefName":"feat/x","isCrossRepository":false,"baseRefName":"main","mergeStateStatus":"CLEAN","closingIssuesReferences":[{"number":647}]}' "$1" >"$FAKE_DIR/pr.json"
  printf '%s\n' "$2" >"$FAKE_DIR/runs"
  printf 'hack/x.sh\n' >"$FAKE_DIR/files"
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

# 1. the e2e decision fails closed
setup false "$green"
rm "$FAKE_DIR/files"
NEEDS_RC=1 run 7
check "an unreadable file list still merges, but requires e2e" 0 merge
grep -q '^wait 7 --checks --require e2e' "$FAKE_DIR/calls" || { echo "FAIL: unreadable file list requires e2e" >&2; fail=1; }
setup false "$green"
: >"$FAKE_DIR/files"
NEEDS_RC=1 run 7
grep -q '^wait 7 --checks --require e2e' "$FAKE_DIR/calls" || { echo "FAIL: empty file list requires e2e" >&2; fail=1; }
setup false "$green"
NEEDS_RC=1 run 7
grep -q 'pulls/7/files --paginate' "$FAKE_DIR/calls" || { echo "FAIL: file list is paginated" >&2; fail=1; }
grep -q -- '--require' "$FAKE_DIR/calls" && { echo "FAIL: a not-needed change adds no --require" >&2; fail=1; }

# 2. claim review only after a successful merge; early exit when no longer OPEN; --auto wording
setup false "$green"
MERGE_RC=1 run 7
check "a failed merge fails" 1 merge
grep -q '^claim review' "$FAKE_DIR/calls" && { echo "FAIL: no claim review after a failed merge" >&2; fail=1; }
setup false "$green"
touch "$FAKE_DIR/close-after-wait"
run 7
check "a PR closed while waiting is not merged" 3 nomerge
grep -q '^claim review' "$FAKE_DIR/calls" && { echo "FAIL: no claim review for a closed PR" >&2; fail=1; }
setup false "$green"
run 7
grep -q 'auto-merge enabled' <<<"$out" || { echo "FAIL: --auto says auto-merge enabled" >&2; fail=1; }
grep -q 'then auto-merge enabled' "$FAKE_DIR/calls" || { echo "FAIL: comment says auto-merge enabled" >&2; fail=1; }

# 5. the claim's lane is passed to claim.sh review
setup false "$green"
printf '#647\tactive\tlease 2026-11-04T12:00:00Z\tpr 0\tme/host/sess#l226\n#648\tactive\tlease x\tpr 0\tme/host/sess\n' >"$FAKE_DIR/mine"
run 7 --admin
check "lane claim merges" 0 merge
grep -q '^claim review 647 --lane l226$' "$FAKE_DIR/calls" || { echo "FAIL: lane read from claim.sh mine" >&2; fail=1; }
setup false "$green"
run 7 --admin --lane cloud5
grep -q '^claim review 647 --lane cloud5$' "$FAKE_DIR/calls" || { echo "FAIL: --lane passed through" >&2; fail=1; }

# 6. a stale PR head
setup false "$green"
printf 'eac36103ffff' >"$FAKE_DIR/remote"
run 7 --admin
check "a stale head is refused" 6 nomerge
grep -q 'STALE-HEAD' <<<"$out" || { echo "FAIL: STALE-HEAD message" >&2; fail=1; }
setup false "$green"
printf 'abc1234def' >"$FAKE_DIR/remote"
run 7 --admin
check "a matching remote head merges" 0 merge

# 7. unresolved review threads (#187): pr-wait's exit 7 passes through; a BLOCKED re-read looks again
setup false "$green"
WAIT_RC=7 run 7
check "pr-wait exit 7 (unresolved threads) stops the merge" 7 nomerge
setup false "$green"
MSTATE=BLOCKED THREADS_RC=7 run 7 --admin
check "BLOCKED by unresolved threads right before the merge is refused" 7 nomerge
grep -q 'UNRESOLVED hack/x.sh:42 deepsource-io' <<<"$out" || { echo "FAIL: lists the thread" >&2; fail=1; }
grep -q 'BLOCKED by unresolved review threads' <<<"$out" || { echo "FAIL: names the block" >&2; fail=1; }
grep -q '^claim review' "$FAKE_DIR/calls" && { echo "FAIL: no claim review when blocked by threads" >&2; fail=1; }
setup false "$green"
MSTATE=BLOCKED THREADS_RC=0 run 7 --admin
check "BLOCKED without unresolved threads still merges" 0 merge
setup false "$green"
MSTATE=BLOCKED THREADS_RC=2 run 7 --admin
check "BLOCKED with unreadable threads is refused (fail closed)" 2 nomerge
grep -q 'cannot read review threads; not merging' <<<"$out" || { echo "FAIL: names the unreadable threads" >&2; fail=1; }
setup false "$green"
THREADS_RC=7 run 7 --admin
check "a CLEAN PR with unresolved threads is refused (threads are always read)" 7 nomerge
grep -q '^threads' "$FAKE_DIR/calls" || { echo "FAIL: threads not read on a CLEAN PR" >&2; fail=1; }
setup false "$green"
MSTATE=UNKNOWN THREADS_RC=7 run 7 --admin
check "an UNKNOWN PR with unresolved threads is refused under --admin" 7 nomerge

exit "$fail"
