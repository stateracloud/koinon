#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/pr-merge.sh offline: `gh` is a stub on PATH answering from files in $FAKE_DIR, and the
# helper scripts (pr-wait, threads) are stubs that record their calls. The stub answers `gh api`
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
export PR_MERGE_WAIT="$tmp/bin/wait"
export PR_MERGE_THREADS="$tmp/bin/threads" PR_MERGE_TEST=1 GH_REPO=o/r
export PR_MERGE_CLAIM="$tmp/bin/claimshow"
export PR_MERGE_REQUIRED="$tmp/required.txt"

printf '# comment\naggregator go\nquick checks\nexternal dco\nadvisory harnesses\n' >"$PR_MERGE_REQUIRED"

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
"api user") echo "${ME_LOGIN:-me}" ;;
"pr view")
  sed "s/\"mergeStateStatus\":\"CLEAN\"/\"mergeStateStatus\":\"${MSTATE:-CLEAN}\"/" "$FAKE_DIR/pr.json" ;;
"pr merge") exit "${MERGE_RC:-0}" ;;
"pr comment") exit "${COMMENT_RC:-0}" ;;
"pr edit") exit "${EDIT_RC:-0}" ;;
"issue create") [ -z "${ISSUE_NOLABEL_FAILS:-}" ] || [[ $* != *--label* ]] || exit 1; exit 0 ;;
"api repos/{owner}/{repo}/pulls/"*"/files")
  [ -f "$FAKE_DIR/files" ] || exit 9
  cat "$FAKE_DIR/files" ;;
"api repos/{owner}/{repo}/rules/branches/"*)
  if [ -f "$FAKE_DIR/rules" ]; then cat "$FAKE_DIR/rules"; else exit 1; fi ;;
"api -H")
  ref=${!#}
  ref=${ref##*ref=}
  [ -f "$FAKE_DIR/req-$ref" ] || exit 1
  cat "$FAKE_DIR/req-$ref" ;;
"api repos/{owner}/{repo}/commits/"*"/status") apijq "$FAKE_DIR/statuses" "$@" ;;
"api repos/{owner}/{repo}/commits/"*"/check-runs"*) apijq "$FAKE_DIR/runs" "$@" ;;
"api repos/{owner}/{repo}/actions/runs"*) [ -f "$FAKE_DIR/wfruns" ] || exit 9; apijq "$FAKE_DIR/wfruns" "$@" ;;
"api graphql")
  if [[ $* == *enqueuePullRequest* ]]; then [ -z "${JUMP_FAILS:-}" ] || exit 1; exit 0; fi
  if [ -f "$FAKE_DIR/queue" ]; then cat "$FAKE_DIR/queue"; else echo '{"data":{"repository":{"pullRequest":{"isInMergeQueue":false,"mergeQueueEntry":null}}}}'; fi ;;
*) exit 9 ;;
esac
SH
cat >"$tmp/bin/git" <<'SH'
#!/usr/bin/env bash
# ls-remote answers from $FAKE_DIR/remote; fetch and show answer the base's e2e rule files.
case $1 in
ls-remote)
  [ -f "$FAKE_DIR/remote" ] || exit 2
  printf '%s\t%s\n' "$(cat "$FAKE_DIR/remote")" "$3" ;;
fetch) echo "git $*" >>"$FAKE_DIR/calls"; exit "${FETCH_RC:-0}" ;;
show)
  echo "git $*" >>"$FAKE_DIR/calls"
  [ -z "${SHOW_FAIL:-}" ] || case $2 in *:"$SHOW_FAIL") exit 128 ;; esac
  case $2 in
  *:hack/needs-e2e.sh) printf '#!/usr/bin/env bash\ncat >/dev/null\nexit 1\n' ;;
  *:hack/ci-changes.sh) [ -n "${CICH_BODY:-}" ] && cat "$CICH_BODY" || exit 128 ;;
  *:hack/paths-lib.sh | *:hack/paths.rules) printf '# stub\n' ;;
  *:.github/CODEOWNERS) printf '# Sensitive paths\n/AGENTS.md @procoduck\n' ;;
  *) exit 128 ;;
  esac ;;
*) exit 2 ;;
esac
SH
cat >"$tmp/bin/claimshow" <<'SH'
#!/usr/bin/env bash
# claim.sh show <N> stand-in: CLAIM_HOLDER unset prints "no claim"; else a held claim in claim.sh's format.
echo "showclaim $*" >>"$FAKE_DIR/calls"
if [ -n "${CLAIM_FAIL:-}" ]; then exit 1; fi
if [ -z "${CLAIM_HOLDER:-}" ]; then echo "#$2: no claim"; exit 0; fi
printf '#%s: held\n  holder: %s\n  lane:   x\n' "$2" "$CLAIM_HOLDER"
SH
cat >"$tmp/bin/wait" <<'SH'
#!/usr/bin/env bash
echo "wait $*" >>"$FAKE_DIR/calls"
if [ -f "$FAKE_DIR/close-after-wait" ]; then sed -i.bak 's/"OPEN"/"CLOSED"/' "$FAKE_DIR/pr.json"; fi
exit "${WAIT_RC:-0}"
SH
cat >"$tmp/bin/threads" <<'SH'
#!/usr/bin/env bash
echo "threads $*" >>"$FAKE_DIR/calls"
[ "${THREADS_RC:-0}" != 7 ] || echo "UNRESOLVED hack/x.sh:42 deepsource-io id=PRRT_2"
exit "${THREADS_RC:-0}"
SH
cat >"$tmp/bin/needs" <<'SH'
#!/usr/bin/env bash
cat >/dev/null
echo "needs $*" >>"$FAKE_DIR/calls"
exit "${NEEDS_RC:-1}"
SH
cat >"$tmp/bin/cich" <<'SH'
#!/usr/bin/env bash
cat >/dev/null
# Without the plumbing flag the real script answers true for a ci.yml change (the `everything` rule).
if [ "${CI_CHANGES_IGNORE_PLUMBING:-}" = 1 ]; then echo "harnesses=${HARNESSES:-false}"; else echo "harnesses=${HARNESSES_ALL:-true}"; fi
SH
chmod +x "$tmp/bin"/*
export PR_MERGE_NEEDS_E2E="$tmp/bin/needs" PR_MERGE_CI_CHANGES="$tmp/bin/cich"

fail=0
setup() { # <isDraft> <runs rows, one JSON object per line>
  rm -rf "$FAKE_DIR"
  mkdir -p "$FAKE_DIR"
  printf '{"id":"PR_node7","state":"OPEN","isDraft":%s,"headRefOid":"abc1234def","headRefName":"feat/x","isCrossRepository":false,"baseRefName":"main","mergeStateStatus":"CLEAN","reviewDecision":"","autoMergeRequest":{"enabledAt":"2026-11-08T10:00:00Z"},"closingIssuesReferences":[{"number":647}]}' "$1" >"$FAKE_DIR/pr.json"
  printf '%s\n' "$2" | jq -sc '{check_runs: .}' >"$FAKE_DIR/runs"
  printf '.github/workflows/x.yml\nhack/x.sh\n' >"$FAKE_DIR/files"
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
run 7
check "without --admin it enables auto-merge" 0 merge
grep -q -- '--auto' "$FAKE_DIR/calls" || { echo "FAIL: --auto" >&2; fail=1; }
grep -q -- '--require' "$FAKE_DIR/calls" && { echo "FAIL: pr-wait gets no --require (no e2e gate on the head, ADR 0105)" >&2; fail=1; }
grep -q 'pulls/7/files' "$FAKE_DIR/calls" && { echo "FAIL: the default route reads no file list" >&2; fail=1; }
grep -q '^wait 7 --checks$' "$FAKE_DIR/calls" || { echo "FAIL: waits with --checks only" >&2; fail=1; }

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
check "with a merge queue --admin on a .github/ PR still merges directly" 0 merge
grep -q -- '--merge --admin --match-head-commit' "$FAKE_DIR/calls" || { echo "FAIL: queue: admin bypass" >&2; fail=1; }
grep -q 'added to the merge queue' <<<"$out" && { echo "FAIL: queue: admin is not queued" >&2; fail=1; }
grep -q 'skips the merge queue and so the queue e2e' <<<"$out" || { echo "FAIL: admin says it skips the queue e2e: $out" >&2; fail=1; }
# --admin stays fail-closed for the e2e: an e2e-relevant .github PR needs a green quickstart on the head
setup false "$green"
NEEDS_RC=0 run 7 --admin
check "--admin, e2e needed, no quickstart: refused (10)" 10 nomerge
grep -q 'no green quickstart (missing)' <<<"$out" || { echo "FAIL: refusal names the missing quickstart: $out" >&2; fail=1; }
setup false "$green
$(r quickstart 2026-11-01T10:00:00Z failure)"
NEEDS_RC=0 run 7 --admin
check "--admin, e2e needed, red quickstart: refused (10)" 10 nomerge
setup false "$green
$(r quickstart 2026-11-01T10:00:00Z skipped)"
NEEDS_RC=0 run 7 --admin
check "--admin, e2e needed, skipped quickstart: refused (10)" 10 nomerge
setup false "$green
$(r quickstart 2026-11-01T10:00:00Z success)"
NEEDS_RC=0 run 7 --admin
check "--admin, e2e needed, green quickstart: merges" 0 merge
setup false "$green"
NEEDS_RC=2 run 7 --admin
check "--admin, needs-e2e cannot say (2): treated as needed, refused (10)" 10 nomerge
setup false "$green"
NEEDS_RC=1 run 7 --admin
check "--admin, .github only, e2e not needed: merges without a quickstart" 0 merge
# --admin and harnesses: the head only has the green skip, so a change that affects the harnesses suite is refused
setup false "$green
$(r harnesses 2026-11-01T10:00:00Z skipped)"
NEEDS_RC=1 HARNESSES=true run 7 --admin
check "--admin, harnesses affected, skipped harnesses: refused (10)" 10 nomerge
{ grep -q 'changes harness inputs and .github paths together' <<<"$out" && grep -q 'split it' <<<"$out"; } || { echo "FAIL: harnesses refusal says to split the pull request: $out" >&2; fail=1; }
setup false "$green
$(r harnesses 2026-11-01T10:00:00Z success)"
NEEDS_RC=1 HARNESSES=true run 7 --admin
check "--admin, harnesses affected, real harnesses success: merges" 0 merge
# A ci.yml-only change (harnesses=true only through the plumbing rule) is not held for harnesses.
setup false "$green
$(r harnesses 2026-11-01T10:00:00Z skipped)"
NEEDS_RC=1 HARNESSES=false HARNESSES_ALL=true run 7 --admin
check "--admin, ci.yml-only change, skipped harnesses: merges" 0 merge
# Real harness inputs together with a .github path: refused unless a real harnesses success exists.
setup false "$green
$(r harnesses 2026-11-01T10:00:00Z skipped)"
NEEDS_RC=1 HARNESSES=true HARNESSES_ALL=true run 7 --admin
check "--admin, harness input plus .github path, skipped harnesses: refused (10)" 10 nomerge
setup false "$green
$(r harnesses 2026-11-01T10:00:00Z skipped)"
NEEDS_RC=1 HARNESSES=false run 7 --admin
check "--admin, harnesses not affected: merges" 0 merge
setup false "$green"
NEEDS_RC=0 run 7 --break-glass "queue down" --approved-at 2026-11-10T09:00:00Z
check "--break-glass does not need the quickstart" 0 merge
# ADR 0105: --admin only for a PR with a .github/ path
setup false "$green"
echo true >"$FAKE_DIR/rules"
printf 'hack/x.sh\nAGENTS.md\n' >"$FAKE_DIR/files"
run 7 --admin
check "--admin on a PR with no .github/ path is refused (9)" 9 nomerge
grep -q 'merge queue.*--break-glass\|--break-glass' <<<"$out" || { echo "FAIL: --admin refusal names the queue and --break-glass: $out" >&2; fail=1; }
grep -q '^wait' "$FAKE_DIR/calls" 2>/dev/null && { echo "FAIL: the refusal comes before any wait" >&2; fail=1; }
setup false "$green"
printf 'docs/.github/x.md\nhack/github/y\n' >"$FAKE_DIR/files"
run 7 --admin
check "--admin: a path that only contains .github/ is not under .github/" 9 nomerge
setup false "$green"
rm "$FAKE_DIR/files"
run 7 --admin
check "--admin with an unreadable file list is a tool failure (2)" 2 nomerge
setup false "$green"
: >"$FAKE_DIR/files"
run 7 --admin
check "--admin with an empty file list is a tool failure (2)" 2 nomerge
# --jump: the front of the queue through GraphQL, pinned to the head; needs the queue rule
setup false "$green"
echo true >"$FAKE_DIR/rules"
echo '{"data":{"repository":{"pullRequest":{"isInMergeQueue":true,"mergeQueueEntry":{"position":1,"state":"QUEUED"}}}}}' >"$FAKE_DIR/queue"
run 7 --jump
check "--jump enqueues through GraphQL and runs gh pr merge never" 0 nomerge
grep -q 'api graphql -f query=mutation.*enqueuePullRequest.*jump: true.*-f id=PR_node7 -f oid=abc1234def' "$FAKE_DIR/calls" || { echo "FAIL: --jump mutation with the node id and the head oid" >&2; fail=1; }
grep -q 'added to the merge queue' <<<"$out" || { echo "FAIL: --jump says queued: $out" >&2; fail=1; }
grep -q 'Jumped to the front' "$FAKE_DIR/calls" || { echo "FAIL: --jump comment" >&2; fail=1; }
setup false "$green"
echo false >"$FAKE_DIR/rules"
run 7 --jump
check "--jump without a queue rule is refused (2)" 2 nomerge
setup false "$green"
echo true >"$FAKE_DIR/rules"
JUMP_FAILS=1 run 7 --jump
check "--jump: a failing mutation fails" 1 nomerge
setup false "$green"
run 7 --jump --admin
check "--jump and --admin are exclusive (64)" 64 nomerge
# --e2e: the label before the enqueue; too late once queued
setup false "$green"
echo true >"$FAKE_DIR/rules"
run 7 --e2e
check "--e2e labels, then enqueues" 0 merge
grep -q '^gh pr edit 7 --add-label e2e' "$FAKE_DIR/calls" || { echo "FAIL: --e2e adds the label" >&2; fail=1; }
setup false "$green"
echo true >"$FAKE_DIR/rules"
echo '{"data":{"repository":{"pullRequest":{"isInMergeQueue":true,"mergeQueueEntry":{"position":1,"state":"QUEUED"}}}}}' >"$FAKE_DIR/queue"
run 7 --e2e
check "--e2e on a queued PR is refused (2): the label is read when the entry is built" 2 nomerge
grep -q 'Remove the PR from the queue' <<<"$out" || { echo "FAIL: --e2e says how: $out" >&2; fail=1; }
setup false "$green"
run 7 --e2e --admin
check "--e2e with --admin is a usage error" 64 nomerge
# --break-glass: needs a reason and --approved-at, merges with --admin, opens the issue
setup false "$green"
run 7 --break-glass "queue down" 
check "--break-glass without --approved-at is a usage error (64)" 64 nomerge
setup false "$green"
run 7 --break-glass "" --approved-at 2026-11-10T09:00:00Z
check "--break-glass with an empty reason is a usage error (64)" 64 nomerge
setup false "$green"
printf 'hack/x.sh\n' >"$FAKE_DIR/files"
run 7 --break-glass "GitHub merge queue outage" --approved-at 2026-11-10T09:00:00Z
check "--break-glass merges a PR with no .github/ path" 0 merge
grep -q -- '--merge --admin --match-head-commit abc1234def' "$FAKE_DIR/calls" || { echo "FAIL: break glass merges with --admin" >&2; fail=1; }
grep -q '^review ' "$FAKE_DIR/calls" && { echo "FAIL: break glass ran a review gate" >&2; fail=1; }
grep -q '^gh issue create --title break-glass: PR 7 merged without the merge queue (abc1234)' "$FAKE_DIR/calls" || { echo "FAIL: break glass opens the issue" >&2; fail=1; }
grep -q -- ' --label break-glass$' "$FAKE_DIR/calls" || { echo "FAIL: ... labelled break-glass" >&2; fail=1; }
grep -q 'GitHub merge queue outage' "$FAKE_DIR/calls" || { echo "FAIL: the reason is recorded" >&2; fail=1; }
grep -q 'BREAK GLASS: GitHub merge queue outage' "$FAKE_DIR/calls" || { echo "FAIL: the PR comment says break glass" >&2; fail=1; }
setup false "$green"
run 7 --break-glass "queue down" --approved-at 2026-11-10T09:00:00Z
check "--break-glass needs no review verdict" 0 merge
setup false "$green"
WAIT_RC=1 run 7 --break-glass "queue down" --approved-at 2026-11-10T09:00:00Z
check "--break-glass: a failed pr-wait --checks is a warning, not a stop" 0 merge
grep -q 'WARNING: pr-wait.sh --checks exited 1' <<<"$out" || { echo "FAIL: break glass warns about the wait: $out" >&2; fail=1; }
grep -q 'Checks not green at merge: pr-wait.sh --checks exited 1' "$FAKE_DIR/calls" || { echo "FAIL: the warning is in the break-glass issue" >&2; fail=1; }
grep -q '^gh pr comment .*Checks not green when merged: pr-wait.sh --checks exited 1' "$FAKE_DIR/calls" || { echo "FAIL: the warning is in the PR comment" >&2; fail=1; }
setup false "$(r go 2026-11-01T10:00:00Z failure)"
run 7 --break-glass "runners down" --approved-at 2026-11-10T09:00:00Z
check "--break-glass: red or missing required contexts are a warning too" 0 merge
grep -q 'Checks not green at merge: .*required context(s) not green' "$FAKE_DIR/calls" || { echo "FAIL: not-green contexts are in the issue" >&2; fail=1; }
setup false "$green"
COMMENT_RC=1 run 7 --break-glass "queue down" --approved-at 2026-11-10T09:00:00Z
check "--break-glass: a failed PR comment is a warning, the merge stands" 0 merge
grep -q '^gh issue create' "$FAKE_DIR/calls" || { echo "FAIL: the issue is opened though the comment failed" >&2; fail=1; }
setup false "$green"
run 7 --break-glass "queue down" --approved-at 2026-11-10T09:00:00Z
if [ "$(grep -n '^gh issue create' "$FAKE_DIR/calls" | head -1 | cut -d: -f1)" -lt "$(grep -n '^gh pr comment' "$FAKE_DIR/calls" | head -1 | cut -d: -f1)" ]; then :; else echo "FAIL: the issue comes before the comment" >&2; fail=1; fi
setup false "$green"
WAIT_RC=3 run 7 --break-glass "queue down" --approved-at 2026-11-10T09:00:00Z
check "--break-glass: a closed PR (pr-wait 3) still stops" 3 nomerge
setup false "$green"
WAIT_RC=5 run 7 --break-glass "queue down" --approved-at 2026-11-10T09:00:00Z
check "--break-glass: a draft (pr-wait 5) still stops" 5 nomerge
setup false "$green"
run 7 --break-glass "queue down" --approved-at 2026-11-10T09:00:00Z
grep -q 'Checks not green at merge: none' "$FAKE_DIR/calls" || { echo "FAIL: a clean break glass says none" >&2; fail=1; }
setup false "$green"
ISSUE_NOLABEL_FAILS=1 run 7 --break-glass "queue down" --approved-at 2026-11-10T09:00:00Z
check "--break-glass: a missing label does not stop the merge or the issue" 0 merge
[ "$(grep -c '^gh issue create' "$FAKE_DIR/calls")" = 2 ] || { echo "FAIL: the unlabelled retry opens the issue" >&2; fail=1; }
setup false "$green"
run 7 --break-glass "queue down" --approved-at 2026-11-10T09:00:00Z --admin
check "--break-glass and --admin are exclusive (64)" 64 nomerge
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

# 2. early exit when no longer OPEN; --auto wording
setup false "$green"
MERGE_RC=1 run 7
check "a failed merge fails" 1 merge
setup false "$green"
touch "$FAKE_DIR/close-after-wait"
run 7
check "a PR closed while waiting is not merged" 3 nomerge
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
# the draft-era run stops counting once the PR is ready, whatever its age
d_run=$(wfrun 70 ci.yml "ci (draft)" 2026-11-01T10:06:00Z)
setup false "$(r go 2026-11-01T10:00:00Z success 71)
$(r go 2026-11-01T10:06:00Z failure 70)
$(r 'quick checks' 2026-11-01T10:00:00Z success 71)
$(r 'quick checks' 2026-11-01T10:06:00Z success 70)"
wfruns "$ci_old" "$d_run"
run 7 --admin
check "#133: a newer draft-era run is ignored on a ready PR (the ready run's green stands)" 0 merge
# gh cannot list the workflow runs: nothing is dropped and the verdict is stricter, never looser
setup false "$(r go 2026-11-01T10:00:00Z success 71)
$(r go 2026-11-01T10:05:00Z cancelled 72)
$(r 'quick checks' 2026-11-01T10:05:00Z success 72)"
run 7 --admin
check "#133: unreadable workflow runs: the newest row of a name still decides, and says so" 1 nomerge
grep -q 'cannot list the workflow runs' <<<"$out" || { echo "FAIL: says the runs were unreadable" >&2; fail=1; }

# #133: a recorded real answer (PR #386's head while it was a draft: ci (draft) went red, dco green, secrets red).
# Read as a ready PR: the draft run does not count, so go and quick checks are missing, never green; dco is green.
# shellcheck disable=SC1091
. "$repo/hack/lib/pr-verdict.sh"
fx=$repo/hack/testdata/pr-verdict
setup false ""
jq -c '.check_runs[]' "$fx/check-runs-386-draft-era.json" | jq -sc '{check_runs: .}' >"$FAKE_DIR/runs"
cp "$fx/status-386-draft-era.json" "$FAKE_DIR/statuses"
cp "$fx/runs-386-draft-era.json" "$FAKE_DIR/wfruns"
run 7 --admin
check "#133 recorded answer: a draft-era head is not merge evidence" 1 nomerge
grep -q 'NOT GREEN go (missing)' <<<"$out" || { echo "FAIL: recorded: go missing: $out" >&2; fail=1; }
grep -q 'NOT GREEN dco' <<<"$out" && { echo "FAIL: recorded: dco is green" >&2; fail=1; }
# the same answer read as a draft PR: the draft run counts and its red go is named
rows=$(jq -c '[.check_runs[] | {name, status: (.status|ascii_upcase), conclusion: ((.conclusion // "")|ascii_upcase), startedAt: .started_at, completedAt: .completed_at, detailsUrl: .details_url, workflowName: ""}]' "$fx/check-runs-386-draft-era.json")
v=$(prv_judge "$rows" "$(jq -c '[.workflow_runs[] | {id, path, name, event, display_title, status, conclusion, created_at, run_attempt}]' "$fx/runs-386-draft-era.json")" true go "quick checks" dco)
[ "$v" = "$(printf 'go\tfailure\t42471681674\nquick checks\tsuccess\t42471681674\ndco\tsuccess\t42471681684')" ] || { echo "FAIL: recorded, draft read: $v" >&2; fail=1; }

# #133, ADR 0105: e2e-kind is judged like any workflow. A dispatched run (81) re-run to green does not outvote the
# newer PR run (82): the newest run of the workflow decides, so its red `e2e result` is the verdict.
# (#399 comment fix: the green row is in run 81, not 82.)
e2e_row() { printf '{"name":"e2e result","status":"COMPLETED","conclusion":"%s","startedAt":"%s","completedAt":"%s","detailsUrl":"https://github.com/o/r/actions/runs/%s/job/9","workflowName":""}' "$1" "$2" "$2" "$3"; }
e2e_rows=$(printf '%s\n' "$(e2e_row FAILURE 2026-11-01T10:20:00Z 81)" "$(e2e_row FAILURE 2026-11-01T10:50:00Z 82)" "$(e2e_row SUCCESS 2026-11-01T11:00:00Z 81)" | jq -sc .)
e2e_runs="[$(wfrun 81 e2e-kind.yml e2e-kind 2026-11-01T10:00:00Z 2),$(wfrun 82 e2e-kind.yml e2e-kind 2026-11-01T10:30:00Z)]"
v=$(prv_judge "$e2e_rows" "$e2e_runs" false "e2e result")
[ "$v" = "$(printf 'e2e result\tfailure\t82')" ] || { echo "FAIL: the newest e2e-kind run decides, an older run's re-run green does not: $v" >&2; fail=1; }

# #399: within one run the newest finished row decides, cancelled included (an older green must not win).
setup false "$(r go 2026-11-01T10:00:00Z success 72)
$(r go 2026-11-01T11:00:00Z cancelled 72)
$(r 'quick checks' 2026-11-01T10:05:00Z success 72)"
wfruns "$ci_old" "$ci_new"
run 7 --admin
check "#399: a newer cancelled row in the same run beats an older green" 1 nomerge
grep -q 'NOT GREEN go (cancelled, run 72)' <<<"$out" || { echo "FAIL: newest cancelled decides: $out" >&2; fail=1; }

# #399: prv_empty only covers workflows that carry a required context (advisory startup_failure is ignored).
setup false "$(r go 2026-11-01T10:00:00Z success 72)
$(r 'quick checks' 2026-11-01T10:05:00Z success 72)"
wfruns "$ci_new" "$(wfrun 90 mirror.yml mirror 2026-11-01T10:06:00Z 1 completed startup_failure)"
run 7 --admin
check "#399: an advisory workflow's empty run does not block the merge" 0 merge

# #399: a commit status whose target_url contains /runs/<id> is not dropped by the run filter.
rows=$(jq -nc '[
  {"name":"go","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-01T10:05:00Z","completedAt":"2026-11-01T10:05:00Z","detailsUrl":"https://github.com/o/r/actions/runs/72/job/1","workflowName":"ci"},
  {"name":"quick checks","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-01T10:05:00Z","completedAt":"2026-11-01T10:05:00Z","detailsUrl":"https://github.com/o/r/actions/runs/72/job/2","workflowName":"ci"},
  {"name":"dco","state":"SUCCESS","startedAt":"2026-11-01T10:00:00Z","targetUrl":"https://github.com/o/r/actions/runs/71/job/1"}
]')
v=$(prv_judge "$rows" "[$(wfrun 71 ci.yml ci 2026-11-01T10:00:00Z),$(wfrun 72 ci.yml ci 2026-11-01T10:05:00Z)]" false go "quick checks" dco)
if [ "$v" = "$(printf 'go\tsuccess\t72\nquick checks\tsuccess\t72\ndco\tsuccess\t71')" ]; then
  echo "ok: #399 status with /runs/id is kept by the run filter"
else
  echo "FAIL: #399 status with /runs/id is kept: $v" >&2; fail=1
fi

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

# ADR 0103 is deprecated (#546): --admin and --break-glass never run a review gate.
setup false "$green"
run 7 --admin
check "--admin of a sensitive PR with no review verdict proceeds" 0 merge
grep -q '^review ' "$FAKE_DIR/calls" && { echo "FAIL: a review gate ran under --admin" >&2; fail=1; }
grep -q 'review-gate\|review-verdict' <<<"$out" && { echo "FAIL: the output mentions the review gate: $out" >&2; fail=1; }
# --admin still decides the e2e from the base's rule: fetch the base, read the rule from it (fail closed).
setup false "$green"
FETCH_RC=1 PR_MERGE_NEEDS_E2E="" run 7 --admin
check "a failed base fetch is a tool failure" 2 nomerge
setup false "$green"
SHOW_FAIL=hack/paths.rules PR_MERGE_NEEDS_E2E="" run 7 --admin
check "a base rule file git show cannot read is a tool failure" 2 nomerge
setup false "$green"
PR_MERGE_NEEDS_E2E="" run 7 --admin
check "the base rule path: fetch ok, rule read from the base, e2e not needed: merges" 0 merge
grep -q '^git fetch .*origin main' "$FAKE_DIR/calls" || { echo "FAIL: the base was not fetched" >&2; fail=1; }
grep -q '^git show refs/remotes/origin/main:hack/needs-e2e.sh' "$FAKE_DIR/calls" || { echo "FAIL: needs-e2e.sh not shown from the base" >&2; fail=1; }

# The harnesses guard reads ci-changes.sh from the base too (PR_MERGE_CI_CHANGES="" selects the real copy path).
setup false "$green"
PR_MERGE_CI_CHANGES="" SHOW_FAIL=hack/ci-changes.sh PR_MERGE_NEEDS_E2E="" run 7 --admin
check "a base ci-changes.sh git show cannot read is a tool failure" 2 nomerge
# A base copy that echoes its environment: the plumbing flag and the root reach it, caller overrides do not.
# shellcheck disable=SC2016 # the $FAKE_DIR must expand in the fake script, not here
printf '#!/usr/bin/env bash\ncat >/dev/null\nenv >"$FAKE_DIR/cich-env"\necho harnesses=false\n' >"$tmp/cich-env.sh"
setup false "$green"
HACK_PATHS_RULES=/nonexistent CI_CHANGES_HARNESSES_DIRS=nonexistent CICH_BODY="$tmp/cich-env.sh" PR_MERGE_CI_CHANGES="" PR_MERGE_NEEDS_E2E="" run 7 --admin
check "the base copy of ci-changes.sh answers harnesses=false: merges" 0 merge
grep -qx 'CI_CHANGES_IGNORE_PLUMBING=1' "$FAKE_DIR/cich-env" || { echo "FAIL: the base copy did not get CI_CHANGES_IGNORE_PLUMBING=1" >&2; fail=1; }
grep -qx "CI_CHANGES_ROOT=$repo" "$FAKE_DIR/cich-env" || { echo "FAIL: the base copy did not get CI_CHANGES_ROOT=$repo" >&2; fail=1; }
grep -q '^HACK_PATHS_RULES=\|^CI_CHANGES_HARNESSES_DIRS=' "$FAKE_DIR/cich-env" && { echo "FAIL: a caller override reached the base copy" >&2; fail=1; }
# A base copy that errors or prints nothing means "harnesses needed": the head has no harnesses success, so refuse.
printf '#!/usr/bin/env bash\ncat >/dev/null\nexit 3\n' >"$tmp/cich-err.sh"
printf '#!/usr/bin/env bash\ncat >/dev/null\n' >"$tmp/cich-empty.sh"
for b in err empty; do
  setup false "$green"
  CICH_BODY="$tmp/cich-$b.sh" PR_MERGE_CI_CHANGES="" PR_MERGE_NEEDS_E2E="" run 7 --admin
  check "a base copy that gives no answer ($b) is treated as harnesses needed" 10 nomerge
done

# #971: the required list is the base branch's on the remote, or the PR head's when the PR changes
# hack/ci-required.txt; never the local checkout. PR_MERGE_REQUIRED (the override every test above uses) is unset.
req_run() { # <files of the PR, \n separated>
  setup false "$green"
  printf '%b' "$1" >"$FAKE_DIR/files"
  printf 'aggregator go\nquick checks\nexternal dco\nharnesses\n' >"$FAKE_DIR/req-main"
  printf 'aggregator go\nquick checks\nexternal dco\nharnesses-new\n' >"$FAKE_DIR/req-abc1234def"
  rc=0
  out=$(env -u PR_MERGE_REQUIRED "$repo/hack/pr-merge.sh" 7 2>&1) || rc=$?
}
req_run 'hack/x.sh\n'
check "the base's list is read from the remote: a context the head lacks is refused" 1 nomerge
grep -q 'NOT GREEN harnesses (missing)' <<<"$out" || { echo "FAIL: the base's harnesses is judged: $out" >&2; fail=1; }
grep -q 'ref=main' "$FAKE_DIR/calls" || { echo "FAIL: reads the list at the base branch" >&2; fail=1; }
req_run 'hack/x.sh\nhack/ci-required.txt\n'
check "a PR that changes hack/ci-required.txt is judged by its own list (a rename)" 1 nomerge
{ grep -q 'NOT GREEN harnesses-new (missing)' <<<"$out" && ! grep -q 'NOT GREEN harnesses (' <<<"$out"; } || { echo "FAIL: the head's list decides: $out" >&2; fail=1; }
grep -q 'ref=abc1234def' "$FAKE_DIR/calls" || { echo "FAIL: reads the list at the head" >&2; fail=1; }
setup false "$green
$(r harnesses-new 2026-11-01T10:00:00Z skipped)"
printf 'hack/ci-required.txt\n' >"$FAKE_DIR/files"
printf 'aggregator go\nquick checks\nexternal dco\nharnesses-new\n' >"$FAKE_DIR/req-abc1234def"
rc=0
out=$(env -u PR_MERGE_REQUIRED "$repo/hack/pr-merge.sh" 7 2>&1) || rc=$?
check "the renaming PR merges once the new name is green" 0 merge
setup false "$green"
rc=0
out=$(env -u PR_MERGE_REQUIRED "$repo/hack/pr-merge.sh" 7 2>&1) || rc=$?
check "an unreadable list refuses (fail closed)" 2 nomerge

# Claim warning (#961): warn only, never refuse; silent for no claim or the caller's own lane.
for c in "other lane|me#lane2|1" "other login|you#main|1" "other login, old holder|you/host/s1#main|1" "own lane|me#main|0" "own lane, no lane suffix|me|0" "no claim||0"; do
  IFS='|' read -r name holder want <<<"$c"
  setup false "$green"
  CLAIM_HOLDER=$holder run 7
  check "claim warning ($name): still merges" 0 merge
  got=0
  grep -q 'WARNING: #647 is claimed by' <<<"$out" && got=1
  [ "$got" = "$want" ] || { echo "FAIL: claim warning ($name): want $want, got $got: $out" >&2; fail=1; }
  [ "$want" = 0 ] || grep -q "claimed by $holder, .*ask the holder or your human" <<<"$out" || { echo "FAIL: warning names the holder ($name)" >&2; fail=1; }
done
setup false "$green"
CLAIM_FAIL=1 run 7
check "claim show fails: still merges" 0 merge
grep -q 'pr-merge: note: cannot read the claim on #647; check it with claim.sh show 647' <<<"$out" || { echo "FAIL: unreadable claim prints a note: $out" >&2; fail=1; }
KOINON_LANE=lane2 CLAIM_HOLDER="me#lane2" run 7
grep -q 'WARNING' <<<"$out" && { echo "FAIL: KOINON_LANE=lane2 is the own lane" >&2; fail=1; }

exit "$fail"
