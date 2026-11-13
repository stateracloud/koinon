#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/pr-wait.sh offline: `gh` is a stub on PATH that answers `gh pr view` with the next JSON
# state of a scripted sequence (the last one repeats) and records every call. `gh run list` answers
# with the next line of $FAKE_DIR/runs (unused since ADR 0105; the last line repeats,
# none when the file is absent).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/pr-wait-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
export FAKE_DIR="$tmp/seq" PATH="$tmp/bin:$PATH" PR_THREADS_RETRY_SLEEPS="0 0 0"

cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_DIR/calls"
if [ "$1" = api ] && [[ $2 == */actions/runs* ]]; then # the workflow runs on the head, as the API prints them ({"workflow_runs":[...]}): $FAKE_DIR/wfruns, else gh "fails"
  [ -f "$FAKE_DIR/wfruns" ] || exit 9
  q=""
  while [ $# -gt 0 ]; do [ "$1" != --jq ] || q=$2; shift; done
  jq -r "${q:-.}" "$FAKE_DIR/wfruns"
  exit 0
fi
if [ "$1 $2" = "run list" ]; then
  [ -f "$FAKE_DIR/runs" ] || exit 0
  m=$(($(cat "$FAKE_DIR/rn" 2>/dev/null || echo 0) + 1))
  echo "$m" >"$FAKE_DIR/rn"
  sed -n "${m}p" "$FAKE_DIR/runs" | grep . || tail -n 1 "$FAKE_DIR/runs"
  exit 0
fi
if [ "$1" = api ] && [[ $* == *contents/hack/ci-required.txt* ]]; then # the head's hack/ci-required.txt: $FAKE_DIR/ci-required.txt, else unreadable
  [ -f "$FAKE_DIR/ci-required.txt" ] || exit 9
  cat "$FAKE_DIR/ci-required.txt"
  exit 0
fi
if [ "$1" = api ] && [[ $2 == */rules/branches/* ]]; then # the ruleset's required contexts, one per line
  [ ! -f "$FAKE_DIR/rules-fail" ] || exit 9
  # No $FAKE_DIR/rules: every check of the current PR json is required (most tests judge all of them).
  if [ ! -f "$FAKE_DIR/rules" ]; then
    n=$(cat "$FAKE_DIR/n" 2>/dev/null || echo 1)
    f="$FAKE_DIR/$n.json"
    [ -f "$f" ] || f=$(ls "$FAKE_DIR"/*.json | sort -V | tail -n 1)
    jq -r '(.statusCheckRollup // [])[] | .name // .context' "$f" | grep -vx -e quickstart -e e2e-lab | sort -u
    exit 0
  fi
  cat "$FAKE_DIR/rules"
  exit 0
fi
if [ "$1 $2" = "api graphql" ] && [[ $* == *mergeQueueEntry* ]]; then # the queue state: $FAKE_DIR/queue (JSON, one answer per call, the last repeats), else "not in a queue"
  [ -f "$FAKE_DIR/queue" ] || { echo '{"data":{"repository":{"pullRequest":{"isInMergeQueue":false,"mergeQueueEntry":null}}}}'; exit 0; }
  m=$(($(cat "$FAKE_DIR/qn" 2>/dev/null || echo 0) + 1))
  echo "$m" >"$FAKE_DIR/qn"
  l=$(sed -n "${m}p" "$FAKE_DIR/queue" | grep . || tail -n 1 "$FAKE_DIR/queue")
  [ "$l" != FAIL ] || exit 1 # an API failure
  echo "$l"
  exit 0
fi
if [ "$1 $2" = "api graphql" ]; then # the review threads (hack/pr-threads.sh): $FAKE_DIR/threads holds the pages as gh prints them
  [ -f "$FAKE_DIR/threads" ] || exit 9
  cat "$FAKE_DIR/threads"
  exit 0
fi
if [ "$1" = api ]; then # gh api .../check-runs --jq ...: $FAKE_DIR/rows holds the --jq output (one row per line)
  [ -f "$FAKE_DIR/rows" ] || exit 9
  cat "$FAKE_DIR/rows"
  exit 0
fi
[ "$1 $2" = "pr view" ] || exit 9
n=$(($(cat "$FAKE_DIR/n" 2>/dev/null || echo 0) + 1))
echo "$n" >"$FAKE_DIR/n"
f="$FAKE_DIR/$n.json"
[ -f "$f" ] || f=$(ls "$FAKE_DIR"/*.json | sort -V | tail -n 1)
cat "$f"
SH
chmod +x "$tmp/bin/gh"

fail=0
# seq <json>...: a fresh scripted sequence.
seq_set() {
  rm -rf "$FAKE_DIR"
  mkdir -p "$FAKE_DIR"
  local i=0 j
  for j in "$@"; do
    i=$((i + 1))
    printf '%s' "$j" >"$FAKE_DIR/$i.json"
  done
}
# run <args...>: sets $out and $rc.
run() {
  rc=0
  out=$("$repo/hack/pr-wait.sh" "$@" 2>&1) || rc=$?
}
expect() { # <description> <want rc> <want output>
  if [ "$rc" = "$2" ] && [ "$out" = "$3" ]; then
    echo "ok: $1"
  else
    printf 'FAIL: %s\n  want rc=%s out=%q\n  got  rc=%s out=%q\n' "$1" "$2" "$3" "$rc" "$out" >&2
    fail=1
  fi
}

cr() { # <name> <status> <conclusion> <run id>
  printf '{"name":"%s","status":"%s","conclusion":"%s","detailsUrl":"https://github.com/o/r/actions/runs/%s/job/9"}' "$1" "$2" "$3" "$4"
}
cr_at() { # <name> <conclusion> <run id> <startedAt>
  printf '{"name":"%s","status":"COMPLETED","conclusion":"%s","startedAt":"%s","detailsUrl":"https://github.com/o/r/actions/runs/%s/job/9"}' "$1" "$2" "$4" "$3"
}
pr() {
  local mc=null
  [ -z "$3" ] || mc="{\"oid\":\"$3\"}"
  printf '{"state":"%s","mergeStateStatus":"%s","mergeCommit":%s,"statusCheckRollup":[%s]}' "$1" "$2" "$mc" "$4"
}

# page <thread nodes, comma separated> <hasNextPage>: one GraphQL reviewThreads page, as gh api prints it.
page() { printf '{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":%s,"endCursor":"c1"},"nodes":[%s]}}}}}\n' "$2" "$1"; }
thread() { # <id> <resolved> <path> <line|null> <outdated> <author|null>
  local a=null
  [ "$6" = null ] || a="{\"login\":\"$6\"}"
  printf '{"id":"%s","isResolved":%s,"isOutdated":%s,"path":"%s","line":%s,"originalLine":null,"comments":{"nodes":[{"author":%s}]}}' "$1" "$2" "$5" "$3" "$4" "$a"
}

go_p=$(cr go IN_PROGRESS "" 11)
web_p=$(cr web QUEUED "" 12)
lint_ok=$(cr lint COMPLETED SUCCESS 13)
go_ok=$(cr go COMPLETED SUCCESS 11)
web_ok=$(cr web COMPLETED SUCCESS 12)

# 1. merged after progress; repeated identical states print once
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_p,$web_p")" "$(pr OPEN CLEAN "" "$lint_ok,$go_p,$web_p")" \
  "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_p")" "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")" \
  "$(pr MERGED CLEAN abc1234def "$lint_ok,$go_ok,$web_ok")"
run 7 --interval 0
expect "merged: one line per change" 0 "checks 1/3 pending: go,web
checks 2/3 pending: web
checks 3/3 green
MERGED abc1234"
if grep -q -v -e '^pr view 7 --json' -e '^api repos/{owner}/{repo}/rules/branches/' -e '^api graphql .*mergeQueueEntry' "$FAKE_DIR/calls"; then
  echo "FAIL: only gh pr view is called" >&2
  fail=1
else echo "ok: only gh pr view is called"; fi

# 2. --checks exits 0 when all finished, before the merge
seq_set "$(pr OPEN BLOCKED "" "$lint_ok,$go_p")" "$(pr OPEN BLOCKED "" "$lint_ok,$go_ok")"
page '' false >"$FAKE_DIR/threads" # BLOCKED for another reason: no unresolved thread, still green
run 7 --checks --interval 0
expect "--checks all green" 0 "checks 1/2 pending: go
checks 2/2 green"

# 3. a failed check: exit 1 with the run id
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_p")" "$(pr OPEN CLEAN "" "$lint_ok,$(cr go COMPLETED FAILURE 123)")"
run 7 --interval 0
expect "failed check" 1 "checks 1/2 pending: go
FAILED go (run 123)"

# 4. a failed commit status (no run url)
seq_set "$(pr OPEN CLEAN "" '{"context":"claim","state":"FAILURE","targetUrl":"https://example.invalid/x"}')"
run 7 --checks --interval 0
expect "failed status context" 1 "FAILED claim"

# 5. closed without merge
seq_set "$(pr OPEN CLEAN "" "$go_p")" "$(pr CLOSED UNKNOWN "" "$go_p")"
run 7 --interval 0
expect "closed" 3 "checks 0/1 pending: go
CLOSED"

# 6. merge conflict
seq_set "$(pr OPEN DIRTY "" "$go_p")"
run 7 --interval 0
expect "conflict" 4 "conflict: merge origin/main into the branch, resolve it and push"

# 7. timeout (still pending)
seq_set "$(pr OPEN BLOCKED "" "$go_p")"
run 7 --timeout 2s --interval 1
expect "timeout" 2 "checks 0/1 pending: go
TIMEOUT"

# 10. --require: missing, skipped, failed, green (ADR 0105 removed the e2e special cases: --require e2e, e2e-lab,
# any-green quickstart and the wait for a live e2e-kind run. The queue's quickstart is the proof.)
prh() { # <rollup>: an open PR whose head is abc1234…
  printf '{"state":"OPEN","mergeStateStatus":"CLEAN","mergeCommit":null,"headRefOid":"abc1234def","statusCheckRollup":[%s]}' "$1"
}
q_ok=$(cr_at quickstart SUCCESS 22 2026-11-01T10:05:00Z)
# ADR 0105: a PR's own quickstart is advisory. Red or pending, it is printed and never decides; --require e2e is gone.
q_red=$(cr_at quickstart FAILURE 24 2026-11-01T10:20:00Z)
rm -f "$FAKE_DIR/rules"
seq_set "$(prh "$lint_ok,$(cr go COMPLETED SUCCESS 11),$q_red")"
printf 'go\nlint\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0
expect "a red PR quickstart is advisory: printed, and the checks are green" 0 "advisory quickstart failed
checks 2/2 green"
seq_set "$(prh "$lint_ok,$(cr go COMPLETED SUCCESS 11),$(cr quickstart IN_PROGRESS "" 24)")"
printf 'go\nlint\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0
expect "a pending PR quickstart is advisory too" 0 "advisory quickstart pending
checks 2/2 green"
run 7 --checks --require e2e --interval 0
expect "--require e2e is refused (ADR 0105)" 64 "pr-wait: --require e2e is gone (ADR 0105): the merge queue's quickstart is the e2e proof"
# A required name judged on the newest run only: a rollup with one red and no wfruns still fails (strict).
seq_set "$(prh "$lint_ok,$q_ok,$q_red")"
printf '\n' >"$FAKE_DIR/runs"
run 7 --checks --require quickstart --interval 0
expect "an explicit --require quickstart is a plain required check: a red run fails it" 1 "FAILED quickstart (run 24)
REQUIRED quickstart failed"


usage_line="usage: hack/pr-wait.sh <PR> [--merged] [--checks] [--require CHECK] [--timeout DURATION] [--interval SECONDS]"
e2e_ok=$(cr "e2e result" COMPLETED SUCCESS 14)
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok")"
run 7 --checks --require "e2e result" --interval 0
expect "required check missing" 1 "REQUIRED e2e result missing"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$(cr "e2e result" COMPLETED SKIPPED 14)")"
run 7 --checks --require "e2e result" --interval 0
expect "required check skipped" 1 "REQUIRED e2e result skipped
only skipped runs of e2e result on this head"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$(cr "e2e result" COMPLETED FAILURE 14)")"
run 7 --checks --require "e2e result" --interval 0
expect "required check failed" 1 "FAILED e2e result (run 14)
REQUIRED e2e result failed"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_p")" "$(pr OPEN CLEAN "" "$lint_ok,$go_ok")"
run 7 --checks --require "e2e result" --require go --interval 0
expect "required waits for the others, then reports missing" 1 "checks 1/2 pending: go
REQUIRED e2e result missing"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$e2e_ok")"
run 7 --checks --require "e2e result" --require go --interval 0
expect "required checks green" 0 "checks 3/3 green"
run 7 --require go --interval 0
expect "--require needs --checks" 64 "$usage_line"

# 7b. old issue 562: the ruleset's required contexts must exist on the head. A queued workflow run has no
# jobs yet, so a rollup of finished checks is not "all green" while a required context is absent.
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok")" "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")"
printf 'lint\ngo\nweb\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0
expect "--checks waits for a required context that has not appeared" 0 "checks 2/2 done; waiting for required checks not on the head yet: web
checks 3/3 green"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok")"
printf 'lint\ngo\nweb\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0 --timeout 0
expect "--checks never reports green while a required context is absent" 2 "checks 2/2 done; waiting for required checks not on the head yet: web
TIMEOUT"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")"
printf 'lint\ngo\nweb\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0
expect "--checks green when every required context exists" 0 "checks 3/3 green"
# #971: a ruleset context the head's own list does not define never reports: say so at once, do not wait.
pr_h() { printf '{"state":"OPEN","mergeStateStatus":"CLEAN","mergeCommit":null,"headRefOid":"abc1234def","statusCheckRollup":[%s]}' "$1"; }
seq_set "$(pr_h "$lint_ok,$go_ok")"
printf 'lint\ngo\nflavors\n' >"$FAKE_DIR/rules"
printf 'aggregator go\nquick lint\nharnesses\nadvisory fuzz\n' >"$FAKE_DIR/ci-required.txt"
run 7 --checks --interval 0 --timeout 0
expect "--checks reports a required context the head does not define" 1 "UNDEFINED flavors: required by the ruleset, but hack/ci-required.txt on the head abc1234 does not list it, so no run of this head will report it. A renamed or new required check needs the ruleset and the PR changed in the right order (docs/dev/ci.md \"Renaming a required check\")"
printf 'aggregator go\nlint\nflavors\n' >"$FAKE_DIR/ci-required.txt"
run 7 --checks --interval 0 --timeout 0
expect "--checks still waits for a context the head defines" 2 "checks 2/2 done; waiting for required checks not on the head yet: flavors
TIMEOUT"
printf 'advisory flavors\n' >"$FAKE_DIR/ci-required.txt"
run 7 --checks --interval 0 --timeout 0
expect "an advisory name counts as defined" 2 "checks 2/2 done; waiting for required checks not on the head yet: flavors
TIMEOUT"
rm "$FAKE_DIR/ci-required.txt"
run 7 --checks --interval 0 --timeout 0
expect "an unreadable head list changes nothing" 2 "checks 2/2 done; waiting for required checks not on the head yet: flavors
TIMEOUT"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok")"
run 7 --interval 0 --timeout 0
expect "without --checks a required context absent from the head does not wait" 2 "checks 2/2 green
TIMEOUT"

# 7b2. only required checks decide: third-party checks (DeepSource, CodeRabbit, Socket) are advisory
printf 'lint\ngo\n' >"$FAKE_DIR/rules"
ds_bad='{"context":"DeepSource: Go","state":"FAILURE","targetUrl":"https://deepsource.io/x"}' # a StatusContext, as live
ds_pend='{"context":"CodeRabbit","state":"PENDING","targetUrl":"https://coderabbit.ai/x"}'
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$ds_bad")"
printf 'lint\ngo\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0
expect "a red third-party check with every required check green: advisory, exits 0" 0 "advisory DeepSource: Go failed
checks 2/2 green"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$ds_pend")"
printf 'lint\ngo\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0
expect "a pending advisory check does not keep the wait going" 0 "advisory CodeRabbit pending
checks 2/2 green"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$(cr go COMPLETED FAILURE 11),$ds_bad")"
printf 'lint\ngo\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0
expect "a red required check still exits 1 (advisory ones are listed apart)" 1 "advisory DeepSource: Go failed
FAILED go (run 11)"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$ds_bad")"
touch "$FAKE_DIR/rules-fail"
run 7 --checks --interval 0
expect "required contexts unreadable: fail closed, exit 2" 2 "pr-wait: cannot read the required status checks of the base branch; not judging checks"
[ "$(grep -c '/rules/branches/' "$FAKE_DIR/calls")" = 4 ] || { echo "FAIL: the rules read is tried 4 times" >&2; fail=1; }
# (#201) --merged does not need the rules: an unreadable read is noted once and the wait goes on to the merge.
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_p")" "$(pr OPEN CLEAN "" "$lint_ok,$(cr go COMPLETED FAILURE 11)")" \
  "$(pr MERGED CLEAN abc1234def "$lint_ok,$go_ok")"
touch "$FAKE_DIR/rules-fail"
run 7 --interval 0
expect "--merged with the rules unreadable: no exit 2, waits for the merge" 0 "pr-wait: cannot read the required status checks of the base branch; waiting for the merge without judging checks
advisory go pending
checks 0/0
advisory go failed
MERGED abc1234"
[ "$(grep -c '/rules/branches/' "$FAKE_DIR/calls")" = 4 ] || { echo "FAIL: --merged tries the rules read once (4 calls), not on every poll" >&2; fail=1; }
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_p")" "$(pr CLOSED UNKNOWN "" "$lint_ok,$go_p")"
touch "$FAKE_DIR/rules-fail"
run 7 --interval 0
expect "--merged with the rules unreadable: a close still exits 3" 3 "pr-wait: cannot read the required status checks of the base branch; waiting for the merge without judging checks
advisory go pending
checks 0/0
CLOSED"

# 7c. (old issue 647) a draft is never green; after ready_for_review the draft's runs are ignored
seq_set '{"state":"OPEN","isDraft":true,"mergeStateStatus":"BLOCKED","mergeCommit":null,"statusCheckRollup":[{"name":"quick checks","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-01T10:00:00Z"}]}'
run 7 --checks --interval 0
expect "--checks on a draft: DRAFT, exit 5" 5 "DRAFT: not tested, mark ready for review (hack/ship.sh --ready, gh pr ready)"
rdh() { # <rollup>: a ready PR with a head commit
  printf '{"state":"OPEN","isDraft":false,"mergeStateStatus":"CLEAN","mergeCommit":null,"headRefOid":"abc1234","statusCheckRollup":[%s]}' "$1"
}
cr_ci() { cr_at "$@" | sed 's/^{/{"workflowName":"ci",/'; }
# wfrun <id> <path> <title> <created_at> [attempt] [status] [conclusion]: one workflow run object as `GET actions/runs?head_sha=` prints it.
wfrun() {
  printf '{"id":%s,"name":"%s","path":".github/workflows/%s","event":"%s","display_title":"%s","status":"%s","conclusion":"%s","created_at":"%s","run_started_at":"%s","run_attempt":%s}' \
    "$1" "${2%.yml}" "$2" "${8:-pull_request}" "$3" "${6:-completed}" "${7-success}" "$4" "$4" "${5:-1}"
}
# wfruns <run object>...: the answer of the runs listing.
wfruns() { local IFS=,; printf '{"total_count":%s,"workflow_runs":[%s]}' "$#" "$*" >"$FAKE_DIR/wfruns"; }
# ciruns <id:title>...: ci.yml runs on the head, created one minute apart in the order given.
ciruns() {
  local -a objs=(); local r i=0
  for r in "$@"; do i=$((i + 1)); objs+=("$(wfrun "${r%%:*}" ci.yml "${r#*:}" "2026-11-01T10:0$i:00Z")"); done
  wfruns "${objs[@]}"
}
# (#218, #133) a ready PR drops the check runs of the runs titled `ci (draft)`, by run id, never by time.
# Cases of 2026-11-06: #233 (draft run 96 s before ready), #229 (13 s before), #231 (the same shape, no --checks).
d_go=$(cr_ci go FAILURE 61 2026-11-01T10:00:00Z)
d_web=$(cr_ci web FAILURE 61 2026-11-01T10:00:01Z)
r_go=$(cr_ci go SUCCESS 62 2026-11-01T10:01:36Z)
r_web=$(cr_ci web SUCCESS 62 2026-11-01T10:01:37Z)
seq_set "$(rdh "$d_go,$d_web,$r_go,$r_web")"
printf 'go\nweb\n' >"$FAKE_DIR/rules"
ciruns "61:ci (draft)" "62:ci"
run 7 --checks --interval 0
expect "#233: a draft run 96 s before the ready run is dropped by its title" 0 "checks 2/2 green"
d13_go=$(cr_ci go FAILURE 61 2026-11-01T10:00:00Z)
r13_go=$(cr_ci go SUCCESS 62 2026-11-01T10:00:13Z)
seq_set "$(rdh "$d13_go,$r13_go")"
printf 'go\n' >"$FAKE_DIR/rules"
ciruns "61:ci (draft)" "62:ci"
run 7 --checks --interval 0
expect "#229: a draft run 13 s before the ready run is dropped by its title" 0 "checks 1/1 green"
# #231: pr-merge calls pr-wait without --checks; the draft run's red must not print FAILED
seq_set "$(rdh "$d_go,$d_web,$r_go,$r_web")" '{"state":"MERGED","mergeCommit":{"oid":"abcdef0123"},"statusCheckRollup":[]}'
printf 'go\nweb\n' >"$FAKE_DIR/rules"
ciruns "61:ci (draft)" "62:ci"
run 7 --interval 0
expect "#231: without --checks the draft run is not reported FAILED" 0 "checks 2/2 green
MERGED abcdef0"
# the ready run is not there yet: wait, never pass
seq_set "$(rdh "$d_go")" "$(rdh "$d_go")" "$(rdh "$d_go,$r_go")"
printf 'go\n' >"$FAKE_DIR/rules"
ciruns "61:ci (draft)"
run 7 --checks --interval 0
expect "ready run missing: waits for the ready run" 0 "waiting for the ready run
checks 1/1 green"
seq_set "$(rdh "$d_go")"
printf 'go\n' >"$FAKE_DIR/rules"
ciruns "61:ci (draft)"
run 7 --checks --timeout 1s --interval 1
expect "ready run missing for good: times out, never passes" 2 "waiting for the ready run
TIMEOUT"
# the ready run is red or cancelled: not green
r_bad=$(cr_ci go FAILURE 62 2026-11-01T10:01:36Z)
seq_set "$(rdh "$d_go,$r_bad")"
printf 'go\n' >"$FAKE_DIR/rules"
ciruns "61:ci (draft)" "62:ci"
run 7 --checks --interval 0
expect "ready run red: FAILED on the ready run, not the draft run" 1 "FAILED go (run 62)"
r_cx=$(cr_ci go CANCELLED 62 2026-11-01T10:01:36Z)
seq_set "$(rdh "$d_go,$r_cx")"
printf 'go\n' >"$FAKE_DIR/rules"
ciruns "61:ci (draft)" "62:ci"
run 7 --checks --interval 0
expect "ready run cancelled: FAILED, never green" 1 "FAILED go (run 62)"
# a draft run that is green and a ready run that is red: the red one decides
d_ok=$(cr_ci go SUCCESS 61 2026-11-01T10:00:00Z)
seq_set "$(rdh "$d_ok,$r_bad")"
printf 'go\n' >"$FAKE_DIR/rules"
ciruns "61:ci (draft)" "62:ci"
run 7 --checks --interval 0
expect "draft green, ready red: red" 1 "FAILED go (run 62)"
# a by-hand re-run of a draft run (#676) replays draft=true and keeps the draft title: ignored
rr_go=$(cr_ci go FAILURE 61 2026-11-01T10:10:00Z)
seq_set "$(rdh "$rr_go,$r_go")"
printf 'go\n' >"$FAKE_DIR/rules"
ciruns "61:ci (draft)" "62:ci"
run 7 --checks --interval 0
expect "a re-run draft run is ignored, not reported FAILED" 0 "checks 1/1 green"
# gh cannot list the runs: nothing is dropped, so the draft run's red is never read as green
seq_set "$(rdh "$d_go,$r_go")"
printf 'go\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0
expect "runs unreadable: the draft run is not dropped (red, not green)" 1 "FAILED go (run 61)"
# dco has no ready run: its draft-era verdict stands
dco_old=$(cr_at dco SUCCESS 63 2026-11-01T10:00:00Z | sed 's/^{/{"workflowName":"dco",/')
seq_set "$(rdh "$d_go,$r_go,$dco_old")"
printf 'go\ndco\n' >"$FAKE_DIR/rules"
ciruns "61:ci (draft)" "62:ci"
run 7 --checks --interval 0
expect "a draft-era dco run still counts" 0 "checks 2/2 green"
rm -f "$FAKE_DIR/rules" "$FAKE_DIR/wfruns"

# (PR #521) a newer workflow_dispatch run of the same workflow on the head is no part of the PR verdict: it must not
# supersede the pull_request run and hide its required "e2e result" row (pr-wait waited forever on "checks missing").
e2e_pr=$(cr "e2e result" COMPLETED SUCCESS 42963673597)
seq_set "$(rdh "$e2e_pr")"
printf 'e2e result\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 42963673597 e2e-kind.yml e2e-kind 2026-11-11T10:00:00Z)" \
  "$(wfrun 42963683121 e2e-kind.yml e2e-kind 2026-11-11T10:01:00Z 1 completed success workflow_dispatch)"
run 7 --checks --interval 1 --timeout 3s
expect "#521: a newer workflow_dispatch run does not hide the pull_request run's required row" 0 "checks 1/1 green"
rm -f "$FAKE_DIR/rules" "$FAKE_DIR/wfruns"

# (PR #545) an explicit --require name is judged on its newest run of any event: a newer FAILED workflow_dispatch run
# (hack/e2e-remote.sh) is read, not hidden behind the older green pull_request run; a dispatch-only run is found.
qs_pr=$(cr quickstart COMPLETED SUCCESS 500)
qs_dx=$(cr quickstart COMPLETED FAILURE 501)
seq_set "$(rdh "$qs_pr")"
printf '%s\n%s\n' "$qs_pr" "$qs_dx" >"$FAKE_DIR/rows"
printf 'quickstart\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 500 e2e-kind.yml e2e-kind 2026-11-11T10:00:00Z)" \
  "$(wfrun 501 e2e-kind.yml e2e-kind 2026-11-11T10:01:00Z 1 completed failure workflow_dispatch)"
run 7 --checks --require quickstart --interval 1 --timeout 3s
expect "#545: --require reads a newer failed workflow_dispatch run, not the older green pull_request run" 1 "FAILED quickstart (run 501)
REQUIRED quickstart failed"
seq_set "$(rdh "")"
printf '%s\n' "$qs_dx" >"$FAKE_DIR/rows"
printf 'quickstart\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 501 e2e-kind.yml e2e-kind 2026-11-11T10:01:00Z 1 completed failure workflow_dispatch)"
run 7 --checks --require quickstart --interval 1 --timeout 3s
expect "#545: --require finds a check that only a workflow_dispatch run has" 1 "FAILED quickstart (run 501)
REQUIRED quickstart failed"
rm -f "$FAKE_DIR/rules" "$FAKE_DIR/wfruns" "$FAKE_DIR/rows"

# (#133) one verdict per workflow run: the newest run of a workflow decides, not the newest row of a name.
# The bug these pin: an older green `go` outvoted a newer cancelled run, because a cancelled row never counts as a failure.
o_go=$(cr_ci go SUCCESS 71 2026-11-01T10:00:00Z)
n_cx=$(cr_ci go CANCELLED 72 2026-11-01T10:05:00Z)
seq_set "$(rdh "$o_go,$n_cx")"
printf 'go\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 71 ci.yml ci 2026-11-01T10:00:00Z)" "$(wfrun 72 ci.yml ci 2026-11-01T10:04:00Z)"
run 7 --checks --interval 0
expect "#133: a newer cancelled run with an older green is not green" 1 "FAILED go (run 72)"
# a never-started cancelled row (null startedAt in the GraphQL rollup) sorts by completedAt, not first
n_nostart='{"workflowName":"ci","name":"go","status":"COMPLETED","conclusion":"CANCELLED","startedAt":null,"completedAt":"2026-11-01T10:05:00Z","detailsUrl":"https://github.com/o/r/actions/runs/72/job/9"}'
seq_set "$(rdh "$o_go,$n_nostart")"
printf 'go\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 71 ci.yml ci 2026-11-01T10:00:00Z)" "$(wfrun 72 ci.yml ci 2026-11-01T10:04:00Z)"
run 7 --checks --interval 0
expect "#133: a cancelled row with no startedAt does not lose to an older green" 1 "FAILED go (run 72)"
# the newest run is queued with no checks yet: the older green is not read; wait for it
seq_set "$(rdh "$o_go")" "$(rdh "$o_go")" "$(rdh "$o_go,$(cr_ci go SUCCESS 72 2026-11-01T10:05:00Z)")"
printf 'go\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 71 ci.yml ci 2026-11-01T10:00:00Z)" "$(wfrun 72 ci.yml ci 2026-11-01T10:04:00Z 1 queued "")"
run 7 --checks --interval 0
expect "#133: a newer run without checks is waited for, the older green is not used" 0 "waiting for the newest run of each workflow
checks 1/1 green"
# a red older run and a green newer run: green (re-run after a fix)
o_red=$(cr_ci go FAILURE 71 2026-11-01T10:00:00Z)
n_ok=$(cr_ci go SUCCESS 72 2026-11-01T10:05:00Z)
seq_set "$(rdh "$o_red,$n_ok")"
printf 'go\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 71 ci.yml ci 2026-11-01T10:00:00Z)" "$(wfrun 72 ci.yml ci 2026-11-01T10:04:00Z)"
run 7 --checks --interval 0
expect "#133: a green newer run supersedes a red older run" 0 "checks 1/1 green"
# a re-run attempt of the same run: the live attempt is waited for, the finished one is not final
a2=$(cr_ci go IN_PROGRESS 72 2026-11-01T10:07:00Z | sed 's/"status":"COMPLETED"/"status":"IN_PROGRESS"/;s/"conclusion":"IN_PROGRESS"/"conclusion":""/')
seq_set "$(rdh "$n_cx,$a2")"
printf 'go\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 72 ci.yml ci 2026-11-01T10:04:00Z)"
run 7 --checks --timeout 1s --interval 1
expect "#133: a live re-run attempt outranks the cancelled one" 2 "checks 0/1 pending: go
TIMEOUT"
# review of 1f76b6ca5: one run, `go` green at 10:00 and a live re-run attempt at 11:00: the unfinished row wins, never green
g_old=$(cr_ci go SUCCESS 72 2026-11-01T10:00:00Z)
g_live=$(cr_ci go IN_PROGRESS 72 2026-11-01T11:00:00Z | sed 's/"status":"COMPLETED"/"status":"IN_PROGRESS"/;s/"conclusion":"IN_PROGRESS"/"conclusion":""/')
seq_set "$(rdh "$g_old,$g_live")"
printf 'go\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 72 ci.yml ci 2026-11-01T10:04:00Z)"
run 7 --checks --timeout 1s --interval 1
expect "#133: an unfinished row in the run outranks an older green of the same name" 2 "checks 0/1 pending: go
TIMEOUT"
# the newest run ended without a single job (startup_failure): a failure naming the run, not a wait until the timeout
seq_set "$(rdh "$o_go")"
printf 'go\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 71 ci.yml ci 2026-11-01T10:00:00Z)" "$(wfrun 72 ci.yml ci 2026-11-01T10:04:00Z 1 completed startup_failure)"
run 7 --checks --timeout 1s --interval 1
expect "#133: a completed newest run with no jobs fails, naming the run" 1 "FAILED ci: run 72 ended (startup_failure) with no jobs"
# a successful run of another workflow with no check run in the rollup (a workflow_dispatch on the head)
# is not part of the verdict: no FAILED line
seq_set "$(rdh "$o_go")"
printf 'go\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 71 ci.yml ci 2026-11-01T10:00:00Z)" "$(wfrun 73 mirror.yml mirror 2026-11-01T10:04:00Z 1 completed success)"
run 7 --checks --timeout 1s --interval 1
if [ "$rc" != 0 ] || grep -q 'FAILED' <<<"$out"; then
  echo "FAIL: a successful run with no check runs is ignored: rc=$rc $out" >&2; fail=1
else echo "ok: a successful run with no check runs is not part of the verdict"; fi
# the e2e workflow is judged like any other (ADR 0105): the newest run decides, an older run's green quickstart does not count
qs_old=$(cr_at quickstart SUCCESS 81 2026-11-01T10:00:00Z | sed 's/^{/{"workflowName":"e2e-kind",/')
qs_new=$(cr_at quickstart SKIPPED 82 2026-11-01T10:05:00Z | sed 's/^{/{"workflowName":"e2e-kind",/')
seq_set "$(rdh "$r_go,$qs_old,$qs_new")"
printf 'go\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 62 ci.yml ci 2026-11-01T10:00:00Z)" "$(wfrun 81 e2e-kind.yml e2e-kind 2026-11-01T10:00:00Z)" "$(wfrun 82 e2e-kind.yml e2e-kind 2026-11-01T10:05:00Z)"
run 7 --checks --require quickstart --interval 0
expect "#133: an older e2e-kind run's green quickstart does not outvote the newest run's skip" 1 "REQUIRED quickstart skipped
only skipped runs of quickstart on this head"
# a dispatched e2e-kind run (81) red, the PR run's (82) `e2e result` re-run to green: the newest row of the name decides
e_dis=$(cr_at "e2e result" FAILURE 81 2026-11-01T10:20:00Z | sed 's/^{/{"workflowName":"e2e-kind",/')
e_pr1=$(cr_at "e2e result" FAILURE 82 2026-11-01T10:50:00Z | sed 's/^{/{"workflowName":"e2e-kind",/')
e_pr2=$(cr_at "e2e result" SUCCESS 82 2026-11-01T11:00:00Z | sed 's/^{/{"workflowName":"e2e-kind",/')
seq_set "$(rdh "$r_go,$e_dis,$e_pr1,$e_pr2")"
printf 'go\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 62 ci.yml ci 2026-11-01T10:00:00Z)" "$(wfrun 81 e2e-kind.yml e2e-kind 2026-11-01T10:00:00Z)" "$(wfrun 82 e2e-kind.yml e2e-kind 2026-11-01T10:30:00Z)"
run 7 --checks --require "e2e result" --interval 0
expect "#133: a re-run green e2e result of the PR run outranks a red dispatched run" 0 "checks 2/2 green"
# #399: within one run a newer cancelled row beats an older green of the same name
g_same=$(cr_ci go SUCCESS 72 2026-11-01T10:00:00Z)
c_same=$(cr_ci go CANCELLED 72 2026-11-01T11:00:00Z)
seq_set "$(rdh "$g_same,$c_same")"
printf 'go\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 72 ci.yml ci 2026-11-01T10:04:00Z)"
run 7 --checks --interval 0
expect "#399: a newer cancelled row in the same run beats an older green" 1 "FAILED go (run 72)"
# #399: an advisory workflow's startup_failure empty run does not fail when go is the only required context
seq_set "$(rdh "$r_go")"
printf 'go\n' >"$FAKE_DIR/rules"
wfruns "$(wfrun 62 ci.yml ci 2026-11-01T10:00:00Z)" "$(wfrun 90 mirror.yml mirror 2026-11-01T10:06:00Z 1 completed startup_failure)"
run 7 --checks --interval 0
expect "#399: an advisory empty run does not fail pr-wait" 0 "checks 1/1 green"
rm -f "$FAKE_DIR/rules" "$FAKE_DIR/wfruns"

# 7d. (old issue 661) a stale PR head: GitHub's headRefOid is not origin's branch tip
cat >"$tmp/bin/git" <<'SH'
#!/usr/bin/env bash
[ "$1" = ls-remote ] && [ -f "$FAKE_DIR/remote" ] || exit 2
m=$(($(cat "$FAKE_DIR/gn" 2>/dev/null || echo 0) + 1))
echo "$m" >"$FAKE_DIR/gn"
r=$(sed -n "${m}p" "$FAKE_DIR/remote" | grep . || tail -n 1 "$FAKE_DIR/remote")
printf '%s\t%s\n' "$r" "$3"
SH
chmod +x "$tmp/bin/git"
sh_pr() { # <head sha>
  printf '{"state":"OPEN","isDraft":false,"mergeStateStatus":"CLEAN","mergeCommit":null,"headRefName":"feat/x","headRefOid":"%s","baseRefName":"main","statusCheckRollup":[%s]}' "$1" "$go_ok"
}
seq_set "$(sh_pr 52d44399aaaa)"
printf 'eac36103bbbb\n' >"$FAKE_DIR/remote"
PR_WAIT_STALE_GRACE=0 run 7 --checks --interval 0
expect "a stale head exits 6 STALE-HEAD" 6 "STALE-HEAD: PR 7 head is 52d4439 but origin/feat/x is eac3610; GitHub has not picked up the push. Re-sync: gh api -X PATCH repos/{owner}/{repo}/pulls/7 -f base=main"
seq_set "$(sh_pr 52d44399aaaa)" "$(sh_pr eac36103bbbb)"
printf 'eac36103bbbb\n' >"$FAKE_DIR/remote"
PR_WAIT_STALE_GRACE=600 run 7 --checks --interval 0
expect "a head that catches up within the grace is judged" 0 "waiting: PR head 52d4439 is not origin/feat/x eac3610 yet
checks 1/1 green"
seq_set "$(sh_pr 52d44399aaaa)"
printf '52d44399aaaa\n' >"$FAKE_DIR/remote"
run 7 --checks --interval 0
expect "a matching head is judged" 0 "checks 1/1 green"
rm -f "$tmp/bin/git"

# 7b. BLOCKED with every check green: the unresolved review threads (#187)
seq_set "$(pr OPEN BLOCKED "" "$go_ok")"
{ page "$(thread PRRT_1 true a.go 3 false coderabbitai),$(thread PRRT_2 false hack/x.sh 42 false deepsource-io)" true
  page "$(thread PRRT_3 false docs/y.md null true coderabbitai),$(thread PRRT_4 false z.go 7 false null)" false; } >"$FAKE_DIR/threads"
run 7 --checks --interval 0
expect "BLOCKED and green lists the unresolved threads, over two pages, and exits 7" 7 "checks 1/1 green
UNRESOLVED hack/x.sh:42 deepsource-io id=PRRT_2
UNRESOLVED docs/y.md:? coderabbitai (outdated) id=PRRT_3
UNRESOLVED z.go:7 ghost id=PRRT_4
BLOCKED: PR 7 has unresolved review threads; reply and resolve each one (docs/dev/ci.md \"Merging\"), then run this again"
seq_set "$(pr OPEN BLOCKED "" "$go_ok")" # no threads file: the graphql call fails
run 7 --checks --interval 0
expect "BLOCKED, green, threads unreadable: exits 2, never ready" 2 "checks 1/1 green
pr-wait: PR 7 is BLOCKED and its review threads could not be read: pr-threads: cannot read the review threads of PR 7
pr-wait: cannot read review threads; not ready"
seq_set "$(pr OPEN CLEAN "" "$go_ok")"
page "$(thread PRRT_2 false hack/x.sh 42 false deepsource-io)" false >"$FAKE_DIR/threads"
run 7 --checks --interval 0
expect "a CLEAN PR does not look at threads" 0 "checks 1/1 green"
seq_set "$(pr OPEN BLOCKED "" "$go_p")"
page "$(thread PRRT_2 false hack/x.sh 42 false deepsource-io)" false >"$FAKE_DIR/threads"
run 7 --checks --timeout 1s --interval 1
expect "BLOCKED with a check still pending keeps waiting (no thread read)" 2 "checks 0/1 pending: go
TIMEOUT"

# 7c. hack/pr-threads.sh on its own
page "$(thread PRRT_1 true a.go 3 false x)" false >"$FAKE_DIR/threads"
rc=0
out=$("$repo/hack/pr-threads.sh" 7 2>&1) || rc=$?
expect "pr-threads: every thread resolved" 0 ""
page "$(thread PRRT_2 false hack/x.sh 42 false deepsource-io)" false >"$FAKE_DIR/threads"
rc=0
out=$("$repo/hack/pr-threads.sh" 7 2>&1) || rc=$?
expect "pr-threads: one unresolved thread exits 7" 7 "UNRESOLVED hack/x.sh:42 deepsource-io id=PRRT_2"
grep -q '^api graphql --paginate' "$FAKE_DIR/calls" || { echo "FAIL: pr-threads paginates with gh api graphql --paginate" >&2; fail=1; }
rm "$FAKE_DIR/threads"
rc=0
out=$("$repo/hack/pr-threads.sh" 7 2>&1) || rc=$?
expect "pr-threads: gh failing exits 2" 2 "pr-threads: cannot read the review threads of PR 7"
: >"$FAKE_DIR/calls"
rc=0
out=$("$repo/hack/pr-threads.sh" 7 2>&1) || rc=$?
[ "$(grep -c '^api graphql' "$FAKE_DIR/calls")" = 4 ] || { echo "FAIL: pr-threads tries 4 times (1 + 3 retries)" >&2; fail=1; }
printf '%s\n' '{"data":{"repository":{"pullRequest":null}}}' >"$FAKE_DIR/threads"
rc=0
out=$("$repo/hack/pr-threads.sh" 7 2>&1) || rc=$?
expect "pr-threads: pullRequest null exits 2" 2 "pr-threads: PR 7 not found (pullRequest is null in the answer)"
rc=0
out=$("$repo/hack/pr-threads.sh" 2>&1) || rc=$?
expect "pr-threads: usage" 64 "usage: hack/pr-threads.sh <PR>"

# 8b. the merge queue (#225): OPEN while queued, merged by the queue build; or removed from it
q1='{"data":{"repository":{"pullRequest":{"isInMergeQueue":true,"mergeQueueEntry":{"position":2,"state":"QUEUED"}}}}}'
q2='{"data":{"repository":{"pullRequest":{"isInMergeQueue":true,"mergeQueueEntry":{"position":1,"state":"AWAITING_CHECKS"}}}}}'
qno='{"data":{"repository":{"pullRequest":{"isInMergeQueue":false,"mergeQueueEntry":null}}}}'
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")" "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")" \
  "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")" "$(pr MERGED CLEAN abc1234def "$lint_ok,$go_ok,$web_ok")"
printf '%s\n%s\n%s\n' "$q1" "$q2" "$q2" >"$FAKE_DIR/queue"
run 7 --interval 0
expect "a queued PR is waited for until the queue build merges it" 0 "in the merge queue (2 QUEUED)
in the merge queue (1 AWAITING_CHECKS)
MERGED abc1234"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")"
printf '%s\n%s\n' "$q1" "$qno" >"$FAKE_DIR/queue"
run 7 --interval 0
expect "a PR that left the queue without merging exits 8" 8 "in the merge queue (2 QUEUED)
DEQUEUED: PR 7 left the merge queue without merging (the queue build failed, it conflicted with an entry ahead, or it was removed); read the queue build's failures on the PR page, fix, and enqueue again with hack/pr-merge.sh"
# The queue clears before the PR reads MERGED: one OPEN-and-not-queued poll is not a dequeue.
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")" "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")" \
  "$(pr MERGED CLEAN abc1234def "$lint_ok,$go_ok,$web_ok")"
printf '%s\n%s\n%s\n' "$q1" "$qno" "$qno" >"$FAKE_DIR/queue"
run 7 --interval 0
expect "a queue that clears just before the PR reads MERGED is a merge, not a dequeue" 0 "in the merge queue (2 QUEUED)
MERGED abc1234"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")"
printf '%s\n' "$q1" >"$FAKE_DIR/queue"
run 7 --interval 0 --timeout 0
expect "a queued PR times out like any wait" 2 "in the merge queue (2 QUEUED)
TIMEOUT"
# An unreadable queue state (API failure) while queued is not a dequeue: keep waiting.
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")" "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")" \
  "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")" "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")" "$(pr MERGED CLEAN abc1234def "$lint_ok,$go_ok,$web_ok")"
printf '%s\n' "$q1" FAIL FAIL "$qno" >"$FAKE_DIR/queue"
rm -f "$FAKE_DIR/qn"
run 7 --interval 0
expect "queued, two API failures, then MERGED: keeps waiting and exits 0" 0 "in the merge queue (2 QUEUED)
merge queue state unreadable; waiting
MERGED abc1234"
# Unreadable answers neither count toward nor reset the two not-queued polls that make a dequeue.
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")"
printf '%s\n' "$q1" FAIL FAIL "$qno" "$qno" >"$FAKE_DIR/queue"
rm -f "$FAKE_DIR/qn"
run 7 --interval 0
expect "queued, two API failures, then two real not-queued polls: DEQUEUED" 8 "in the merge queue (2 QUEUED)
merge queue state unreadable; waiting
DEQUEUED: PR 7 left the merge queue without merging (the queue build failed, it conflicted with an entry ahead, or it was removed); read the queue build's failures on the PR page, fix, and enqueue again with hack/pr-merge.sh"
# --checks judges the head's checks and never asks the queue (pr-merge --admin on an enqueued PR).
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok,$web_ok")"
printf '%s\n' "$q1" >"$FAKE_DIR/queue"
rm -f "$FAKE_DIR/qn"
page '' false >"$FAKE_DIR/threads"
run 7 --checks --interval 0
expect "--checks on a queued PR with green checks exits 0" 0 "checks 3/3 green"
if [ ! -e "$FAKE_DIR/qn" ]; then echo "ok: --checks never calls the queue GraphQL"; else echo "FAIL: --checks called the queue GraphQL"; fail=1; fi

# 8. a transient gh failure is retried
rm -rf "$FAKE_DIR"
mkdir -p "$FAKE_DIR"
cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
n=$(($(cat "$FAKE_DIR/n" 2>/dev/null || echo 0) + 1))
echo "$n" >"$FAKE_DIR/n"
[ "$n" -gt 1 ] || exit 1
echo '{"state":"MERGED","mergeStateStatus":"CLEAN","mergeCommit":{"oid":"1234567890"},"statusCheckRollup":[]}'
SH
run 7 --interval 0
expect "transient gh failure" 0 "MERGED 1234567"

# 9. usage
run
expect "no PR is a usage error" 64 "usage: hack/pr-wait.sh <PR> [--merged] [--checks] [--require CHECK] [--timeout DURATION] [--interval SECONDS]"
run 7 --timeout soon
expect "bad timeout is a usage error" 64 "usage: hack/pr-wait.sh <PR> [--merged] [--checks] [--require CHECK] [--timeout DURATION] [--interval SECONDS]"

exit "$fail"
