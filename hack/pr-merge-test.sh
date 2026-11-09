#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/pr-merge.sh offline: `gh` is a stub on PATH answering from files in $FAKE_DIR, and the
# helper scripts (pr-wait, needs-e2e, claim, threads) are stubs that record their calls. The stub answers `gh api`
# with API-shaped JSON (what GitHub prints; a recorded real answer lives in hack/testdata/pr-verdict/) and applies the
# call's own `--jq` filter to it, so the script's parsing is tested, not a pre-chewed list.

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
apijq() { # <file> <gh args...>: print the file through the call's --jq filter
  local f=$1 q=.
  shift
  while [ $# -gt 0 ]; do [ "$1" != --jq ] || q=$2; shift; done
  jq -r "$q" "$f"
}
case "$1 $2" in
"pr view")
  sed "s/\"mergeStateStatus\":\"CLEAN\"/\"mergeStateStatus\":\"${MSTATE:-CLEAN}\"/" "$FAKE_DIR/pr.json" ;;
"pr merge") exit "${MERGE_RC:-0}" ;;
"pr comment") ;;
"api repos/{owner}/{repo}/pulls/"*"/files")
  [ -f "$FAKE_DIR/files" ] || exit 9
  cat "$FAKE_DIR/files" ;;
"api repos/{owner}/{repo}/rules/branches/"*)
  if [ -f "$FAKE_DIR/rules" ]; then cat "$FAKE_DIR/rules"; else exit 1; fi ;;
"api repos/{owner}/{repo}/commits/"*"/status") apijq "$FAKE_DIR/statuses" "$@" ;;
"api repos/{owner}/{repo}/commits/"*"/check-runs"*) apijq "$FAKE_DIR/runs" "$@" ;;
"api repos/{owner}/{repo}/actions/runs"*) [ -f "$FAKE_DIR/wfruns" ] || exit 9; apijq "$FAKE_DIR/wfruns" "$@" ;;
"api graphql")
  if [ -f "$FAKE_DIR/queue" ]; then cat "$FAKE_DIR/queue"; else echo '{"data":{"repository":{"pullRequest":{"isInMergeQueue":false,"mergeQueueEntry":null}}}}'; fi ;;
*) exit 9 ;;
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
  printf '{"state":"OPEN","isDraft":%s,"headRefOid":"abc1234def","headRefName":"feat/x","isCrossRepository":false,"baseRefName":"main","mergeStateStatus":"CLEAN","reviewDecision":"","autoMergeRequest":{"enabledAt":"2026-11-08T10:00:00Z"},"closingIssuesReferences":[{"number":647}]}' "$1" >"$FAKE_DIR/pr.json"
  printf '%s\n' "$2" | jq -sc '{check_runs: .}' >"$FAKE_DIR/runs"
  printf 'hack/x.sh\n' >"$FAKE_DIR/files"
  printf '{"statuses":[{"context":"dco","state":"success","created_at":"2026-11-01T10:00:00Z","target_url":""}]}\n' >"$FAKE_DIR/statuses"
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
# r <name> <started_at> <success|skipped|failure|cancelled|pending> [run id]: one check run as the API prints it.
r() {
  local st=completed c=$3 j
  j=$(($(cat "$tmp/job") + 1))
  echo "$j" >"$tmp/job"
  [ "$3" != pending ] || { st=in_progress c=null; }
  [ "$c" = null ] || c="\"$c\""
  printf '{"id":%s,"name":"%s","status":"%s","conclusion":%s,"started_at":"%s","completed_at":"%s","details_url":"https://github.com/o/r/actions/runs/%s/job/%s"}' \
    "$j" "$1" "$st" "$c" "$2" "$2" "${4:-1}" "$j"
}
echo 100 >"$tmp/job"
# wfrun <id> <path> <title> <created_at> [attempt] [status] [conclusion]: one workflow run as `GET actions/runs?head_sha=` prints it.
wfrun() {
  printf '{"id":%s,"name":"%s","path":".github/workflows/%s","event":"pull_request","display_title":"%s","status":"%s","conclusion":"%s","created_at":"%s","run_attempt":%s}' \
    "$1" "${2%.yml}" "$2" "$3" "${6:-completed}" "${7-success}" "$4" "${5:-1}"
}
wfruns() { local IFS=,; printf '{"total_count":%s,"workflow_runs":[%s]}' "$#" "$*" >"$FAKE_DIR/wfruns"; }

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
grep -q 'NOT GREEN quick checks (failure, run 1)' <<<"$out" || { echo "FAIL: names the red context" >&2; fail=1; }

setup false "$(r go 2026-11-01T10:00:00Z success)"
run 7
check "a context that never reported is refused" 1 nomerge

setup false "$(r go 2026-11-01T10:00:00Z failure)
$(r go 2026-11-01T11:00:00Z success)
$(r 'quick checks' 2026-11-01T10:00:00Z success)"
run 7 --admin --approved-at 2026-11-04T01:00:00Z
check "all green (newest run per name) merges" 0 merge
grep -q '^gh pr merge 7 --merge --admin --match-head-commit abc1234def' "$FAKE_DIR/calls" || { echo "FAIL: admin merge pinned to the head" >&2; fail=1; }
grep -q '^claim ' "$FAKE_DIR/calls" && { echo "FAIL: pr-merge calls no claim.sh (ADR 0104: the merge releases the claim)" >&2; fail=1; }
grep -q '^gh pr comment 7 --body .*read 3 required contexts on head .abc1234def' "$FAKE_DIR/calls" || { echo "FAIL: comment names the SHA and the count" >&2; fail=1; }

setup false "$green"
NEEDS_RC=0 run 7
check "without --admin it enables auto-merge" 0 merge
grep -q -- '--auto' "$FAKE_DIR/calls" || { echo "FAIL: --auto" >&2; fail=1; }
grep -q '^wait 7 --checks --require e2e' "$FAKE_DIR/calls" || { echo "FAIL: a needs-e2e change waits with --require e2e" >&2; fail=1; }

# #225: the merge queue, detected from the ruleset at run time
setup false "$green"
echo true >"$FAKE_DIR/rules"
echo '{"data":{"repository":{"pullRequest":{"isInMergeQueue":true,"mergeQueueEntry":{"position":1,"state":"QUEUED"}}}}}' >"$FAKE_DIR/queue"
run 7
check "with a merge queue it enqueues (gh pr merge --auto)" 0 merge
grep -q -- '--merge --auto --match-head-commit abc1234def' "$FAKE_DIR/calls" || { echo "FAIL: queue: --auto pinned to the head" >&2; fail=1; }
grep -q 'added to the merge queue' <<<"$out" || { echo "FAIL: queue: output says queued: $out" >&2; fail=1; }
setup false "$green"
echo true >"$FAKE_DIR/rules"
run 7 --admin
check "with a merge queue --admin still merges directly" 0 merge
grep -q -- '--merge --admin --match-head-commit' "$FAKE_DIR/calls" || { echo "FAIL: queue: admin bypass" >&2; fail=1; }
grep -q 'added to the merge queue' <<<"$out" && { echo "FAIL: queue: admin is not queued" >&2; fail=1; }
grep -q 'skips the merge queue' <<<"$out" && { echo "FAIL: queue: no admin warning when e2e is not needed" >&2; fail=1; }
setup false "$green"
echo true >"$FAKE_DIR/rules"
NEEDS_RC=0 run 7 --admin
check "--admin on a queue with an e2e-relevant change still merges" 0 merge
grep -q 'WARNING: --admin skips the merge queue' <<<"$out" || { echo "FAIL: queue: admin warning for e2e-relevant change: $out" >&2; fail=1; }
setup false "$green"
echo false >"$FAKE_DIR/rules"
NEEDS_RC=0 run 7 --admin
grep -q 'skips the merge queue' <<<"$out" && { echo "FAIL: no queue: no admin warning" >&2; fail=1; }
setup false "$green"
echo false >"$FAKE_DIR/rules"
run 7
check "a ruleset without the queue keeps auto-merge" 0 merge
grep -q 'auto-merge enabled' <<<"$out" || { echo "FAIL: no queue: auto-merge enabled: $out" >&2; fail=1; }
setup false "$green"
run 7
check "an unreadable ruleset reads as no queue" 0 merge
grep -q 'auto-merge enabled' <<<"$out" || { echo "FAIL: unreadable rules: auto-merge enabled: $out" >&2; fail=1; }

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

# 5. --lane is gone (ADR 0104): a usage error, nothing merged
setup false "$green"
run 7 --admin --lane cloud5
check "--lane is a usage error" 64 nomerge
grep -q -- '--lane is gone' <<<"$out" || { echo "FAIL: --lane names why: $out" >&2; fail=1; }

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

# #133: one verdict per workflow run (hack/lib/pr-verdict.sh). The old read took the newest row of each name by
# started_at only, so a cancelled run's row (never started) sorted first and an older green won.
ci_old=$(wfrun 71 ci.yml ci 2026-11-01T10:00:00Z)
ci_new=$(wfrun 72 ci.yml ci 2026-11-01T10:05:00Z)
setup false "$(r go 2026-11-01T10:00:00Z success 71)
$(r go 2026-11-01T10:05:00Z cancelled 72)
$(r 'quick checks' 2026-11-01T10:00:00Z success 71)
$(r 'quick checks' 2026-11-01T10:05:00Z success 72)"
wfruns "$ci_old" "$ci_new"
run 7 --admin
check "#133: a newer cancelled run with an older green is not merged" 1 nomerge
grep -q 'NOT GREEN go (cancelled, run 72)' <<<"$out" || { echo "FAIL: names the cancelled run: $out" >&2; fail=1; }
setup false "$(r go 2026-11-01T10:00:00Z success 71)
$(r go '' cancelled 72)
$(r 'quick checks' 2026-11-01T10:05:00Z success 72)"
wfruns "$ci_old" "$ci_new"
run 7 --admin
check "#133: the same without a start time on the cancelled row" 1 nomerge
setup false "$(r go 2026-11-01T10:00:00Z success 71)
$(r 'quick checks' 2026-11-01T10:00:00Z success 71)"
wfruns "$ci_old" "$(wfrun 72 ci.yml ci 2026-11-01T10:05:00Z 1 queued "")"
run 7 --admin
check "#133: a newer run that has no checks yet: the older green is not read" 1 nomerge
grep -q 'NOT GREEN go (missing)' <<<"$out" || { echo "FAIL: missing, not green: $out" >&2; fail=1; }
setup false "$(r go 2026-11-01T10:00:00Z failure 71)
$(r 'quick checks' 2026-11-01T10:00:00Z failure 71)
$(r go 2026-11-01T10:05:00Z success 72)
$(r 'quick checks' 2026-11-01T10:05:00Z success 72)"
wfruns "$ci_old" "$ci_new"
run 7 --admin
check "#133: a green newer run supersedes a red older run" 0 merge
setup false "$(r go 2026-11-01T10:00:00Z success 71)
$(r go 2026-11-01T10:05:00Z pending 72)
$(r 'quick checks' 2026-11-01T10:05:00Z success 72)"
wfruns "$ci_old" "$ci_new"
run 7 --admin
check "#133: a newer run still in progress is pending, not green" 1 nomerge
# review of 1f76b6ca5: inside one run an unfinished row outranks an older green of the same name
setup false "$(r go 2026-11-01T10:00:00Z success 72)
$(r go 2026-11-01T11:00:00Z pending 72)
$(r 'quick checks' 2026-11-01T10:05:00Z success 72)"
wfruns "$ci_old" "$ci_new"
run 7 --admin
check "#133: a live re-run attempt hides an older green of the same run" 1 nomerge
grep -q 'NOT GREEN go (pending, run 72)' <<<"$out" || { echo "FAIL: names the unfinished row: $out" >&2; fail=1; }
# the newest run ended with no jobs at all (startup_failure): a failure naming the run
setup false "$(r go 2026-11-01T10:00:00Z success 71)
$(r 'quick checks' 2026-11-01T10:00:00Z success 71)"
wfruns "$ci_old" "$(wfrun 72 ci.yml ci 2026-11-01T10:05:00Z 1 completed startup_failure)"
run 7 --admin
check "#133: a completed newest run with no jobs is refused" 1 nomerge
grep -q 'NOT GREEN ci: run has no jobs (startup_failure) (failure, run 72)' <<<"$out" || { echo "FAIL: names the empty run: $out" >&2; fail=1; }
# a re-run attempt inside the chosen run: the newest row of the name decides
setup false "$(r go 2026-11-01T10:05:00Z failure 72)
$(r go 2026-11-01T10:09:00Z success 72)
$(r 'quick checks' 2026-11-01T10:05:00Z success 72)"
wfruns "$ci_old" "$(wfrun 72 ci.yml ci 2026-11-01T10:05:00Z 2)"
run 7 --admin
check "#133: a re-run attempt that passed replaces its failed attempt" 0 merge
# no draft filter: a newer run of ci decides, whatever it was for (a draft now runs nothing, so its run is green)
d_run=$(wfrun 70 ci.yml ci 2026-11-01T10:06:00Z)
setup false "$(r go 2026-11-01T10:00:00Z success 71)
$(r go 2026-11-01T10:06:00Z failure 70)
$(r 'quick checks' 2026-11-01T10:00:00Z success 71)
$(r 'quick checks' 2026-11-01T10:06:00Z success 70)"
wfruns "$ci_old" "$d_run"
run 7 --admin
check "#133: a newer run of ci decides, its red stands" 1 nomerge
# gh cannot list the workflow runs: nothing is dropped and the verdict is stricter, never looser
setup false "$(r go 2026-11-01T10:00:00Z success 71)
$(r go 2026-11-01T10:05:00Z cancelled 72)
$(r 'quick checks' 2026-11-01T10:05:00Z success 72)"
run 7 --admin
check "#133: unreadable workflow runs: the newest row of a name still decides, and says so" 1 nomerge
grep -q 'cannot list the workflow runs' <<<"$out" || { echo "FAIL: says the runs were unreadable" >&2; fail=1; }

# #133: a recorded real answer (PR #386's head while it was a draft, before drafts ran nothing: its ci run went red,
# dco green, secrets red). There is no draft filter any more: the red ci run is the newest run of ci, so go is red,
# never green; dco is green.
# shellcheck disable=SC1091
. "$repo/hack/lib/pr-verdict.sh"
fx=$repo/hack/testdata/pr-verdict
setup false ""
jq -c '.check_runs[]' "$fx/check-runs-386-draft-era.json" | jq -sc '{check_runs: .}' >"$FAKE_DIR/runs"
cp "$fx/status-386-draft-era.json" "$FAKE_DIR/statuses"
cp "$fx/runs-386-draft-era.json" "$FAKE_DIR/wfruns"
run 7 --admin
check "#133 recorded answer: a draft-era head is not merge evidence" 1 nomerge
grep -q 'NOT GREEN go (failure' <<<"$out" || { echo "FAIL: recorded: go is red: $out" >&2; fail=1; }
grep -q 'NOT GREEN dco' <<<"$out" && { echo "FAIL: recorded: dco is green" >&2; fail=1; }
# the same rows through the verdict function: the red go is named
rows=$(jq -c '[.check_runs[] | {name, status: (.status|ascii_upcase), conclusion: ((.conclusion // "")|ascii_upcase), startedAt: .started_at, completedAt: .completed_at, detailsUrl: .details_url, workflowName: ""}]' "$fx/check-runs-386-draft-era.json")
v=$(prv_judge "$rows" "$(jq -c '[.workflow_runs[] | {id, path, name, event, display_title, status, conclusion, created_at, run_attempt}]' "$fx/runs-386-draft-era.json")" "" go "quick checks" dco)
[ "$v" = "$(printf 'go\tfailure\t42471681674\nquick checks\tsuccess\t42471681674\ndco\tsuccess\t42471681684')" ] || { echo "FAIL: recorded, verdict: $v" >&2; fail=1; }

# #133: a dispatched e2e-kind run (81) failed, e2e-lab passed, and the PR run's (82) `e2e result` was re-run to green: the
# re-run keeps its created_at, but GitHub's newest check run per name is green, and so is the verdict.
e2e_row() { printf '{"name":"e2e result","status":"COMPLETED","conclusion":"%s","startedAt":"%s","completedAt":"%s","detailsUrl":"https://github.com/o/r/actions/runs/%s/job/9","workflowName":""}' "$1" "$2" "$2" "$3"; }
e2e_rows=$(printf '%s\n' "$(e2e_row FAILURE 2026-11-01T10:20:00Z 81)" "$(e2e_row FAILURE 2026-11-01T10:50:00Z 82)" "$(e2e_row SUCCESS 2026-11-01T11:00:00Z 81)" | jq -sc .)
e2e_runs="[$(wfrun 81 e2e-kind.yml e2e-kind 2026-11-01T10:00:00Z 2),$(wfrun 82 e2e-kind.yml e2e-kind 2026-11-01T10:30:00Z)]"
v=$(prv_judge "$e2e_rows" "$e2e_runs" "quickstart" "e2e result")
[ "$v" = "$(printf 'e2e result\tsuccess\t81')" ] || { echo "FAIL: e2e result re-run green after a red dispatched run: $v" >&2; fail=1; }

# #133: what GitHub did is what is printed. A queue repository, auto-merge accepted, but the PR waits for a required
# review (required_reviewers): it is NOT in the queue (2026-11-07 printed "added to the merge queue" for it).
setup false "$green"
echo true >"$FAKE_DIR/rules"
sed -i.bak 's/"reviewDecision":""/"reviewDecision":"REVIEW_REQUIRED"/;s/"mergeStateStatus":"CLEAN"/"mergeStateStatus":"BLOCKED"/' "$FAKE_DIR/pr.json"
export THREADS_RC=0 MSTATE=BLOCKED; run 7; unset THREADS_RC MSTATE
check "#133: queue repository, PR blocked by a required review: merge command ok" 0 merge
grep -q 'added to the merge queue' <<<"$out" && { echo "FAIL: not queued, must not say queued: $out" >&2; fail=1; }
grep -q 'NOT queued.*required review' <<<"$out" || { echo "FAIL: says it waits for a required review: $out" >&2; fail=1; }
grep -q 'NOT queued' "$FAKE_DIR/calls" || { echo "FAIL: the PR comment is honest too" >&2; fail=1; }
setup false "$green"
sed -i.bak 's/"autoMergeRequest":{"enabledAt":"2026-11-08T10:00:00Z"}/"autoMergeRequest":null/' "$FAKE_DIR/pr.json"
run 7
grep -q 'has not armed auto-merge' <<<"$out" || { echo "FAIL: no auto-merge armed is said: $out" >&2; fail=1; }
# review of 1f76b6ca5: a required review with NO auto-merge request must not say "armed"
setup false "$green"
sed -i.bak 's/"reviewDecision":""/"reviewDecision":"REVIEW_REQUIRED"/;s/"autoMergeRequest":{"enabledAt":"2026-11-08T10:00:00Z"}/"autoMergeRequest":null/' "$FAKE_DIR/pr.json"
run 7
grep -q 'armed but' <<<"$out" && { echo "FAIL: no auto-merge request, must not say armed: $out" >&2; fail=1; }
grep -q 'auto-merge is NOT armed.*review is required' <<<"$out" || { echo "FAIL: says not armed, review required: $out" >&2; fail=1; }

exit "$fail"
