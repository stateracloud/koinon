#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/pr-wait.sh offline: `gh` is a stub on PATH that answers `gh pr view` with the next JSON
# state of a scripted sequence (the last one repeats) and records every call. `gh run list` answers
# with the next line of $FAKE_DIR/runs (the live e2e-kind runs on the head; the last line repeats,
# none when the file is absent).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/pr-wait-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
export FAKE_DIR="$tmp/seq" PATH="$tmp/bin:$PATH"

cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_DIR/calls"
if [ "$1 $2" = "run list" ] && [[ $* == *--workflow\ ci.yml* ]]; then # the ci runs on the head: $FAKE_DIR/ciruns (JSON), else gh "fails"
  [ -f "$FAKE_DIR/ciruns" ] || exit 9
  cat "$FAKE_DIR/ciruns"
  exit 0
fi
if [ "$1 $2" = "run list" ]; then
  [ -f "$FAKE_DIR/runs" ] || exit 0
  m=$(($(cat "$FAKE_DIR/rn" 2>/dev/null || echo 0) + 1))
  echo "$m" >"$FAKE_DIR/rn"
  sed -n "${m}p" "$FAKE_DIR/runs" | grep . || tail -n 1 "$FAKE_DIR/runs"
  exit 0
fi
if [ "$1" = api ] && [[ $2 == */rules/branches/* ]]; then # the ruleset's required contexts, one per line
  [ -f "$FAKE_DIR/rules" ] || exit 9
  cat "$FAKE_DIR/rules"
  exit 0
fi
if [ "$1" = api ] && [[ $2 == */issues/*/events ]]; then # the last ready_for_review time, if $FAKE_DIR/ready exists
  [ -f "$FAKE_DIR/ready" ] || exit 9
  cat "$FAKE_DIR/ready"
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
if grep -q -v '^pr view 7 --json' "$FAKE_DIR/calls"; then
  echo "FAIL: only gh pr view is called" >&2
  fail=1
else echo "ok: only gh pr view is called"; fi

# 2. --checks exits 0 when all finished, before the merge
seq_set "$(pr OPEN BLOCKED "" "$lint_ok,$go_p")" "$(pr OPEN BLOCKED "" "$lint_ok,$go_ok")"
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
expect "conflict" 4 "needs-update"

# 7. timeout (still pending)
seq_set "$(pr OPEN BLOCKED "" "$go_p")"
run 7 --timeout 2s --interval 1
expect "timeout" 2 "checks 0/1 pending: go
TIMEOUT"

# 10. --require: missing, skipped, failed, green
# 10a. (#392) two runs of one required check on the head: only the newest counts
q_skip=$(cr_at quickstart SKIPPED 21 2026-11-01T10:00:00Z)
q_ok=$(cr_at quickstart SUCCESS 22 2026-11-01T10:05:00Z)
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$q_skip,$q_ok")"
run 7 --checks --require quickstart --interval 0
expect "later success supersedes an earlier skip" 0 "checks 2/2 green"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$q_ok,$q_skip")"
run 7 --checks --require quickstart --interval 0
expect "newest run wins whatever the rollup order" 0 "checks 2/2 green"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$q_ok,$(cr_at quickstart SKIPPED 23 2026-11-01T10:10:00Z)")"
run 7 --checks --require quickstart --interval 0
expect "a success on the head wins over a newer skip (#547: any run that passed counts)" 0 "checks 2/2 green"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$(cr_at quickstart CANCELLED 21 2026-11-01T10:00:00Z),$q_ok")"
run 7 --checks --require quickstart --interval 0
expect "later success supersedes an earlier cancel" 0 "checks 2/2 green"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$(cr_at quickstart CANCELLED 21 2026-11-01T10:00:00Z)")"
run 7 --checks --require quickstart --interval 0
expect "only a cancelled run: failed, with the dispatch hint" 1 "FAILED quickstart (run 21)
REQUIRED quickstart failed
only cancelled runs of quickstart on this head; run: gh workflow run e2e-kind.yml --ref <branch>"

# 10b. (#431) cancelled and in progress with the same startedAt: keep waiting for the live run
t0=2026-11-01T10:00:00Z
q_cancel=$(cr_at quickstart CANCELLED 41261362803 $t0)
q_live=$(printf '{"name":"quickstart","status":"IN_PROGRESS","conclusion":"","startedAt":"%s","detailsUrl":"https://github.com/o/r/actions/runs/41261363000/job/9"}' $t0)
q_done=$(cr_at quickstart SUCCESS 41261363000 $t0)
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$q_cancel,$q_live")" "$(pr OPEN CLEAN "" "$lint_ok,$q_live,$q_cancel")" \
  "$(pr OPEN CLEAN "" "$lint_ok,$q_cancel,$q_done")"
run 7 --checks --require quickstart --interval 0
expect "cancelled plus in-progress at the same startedAt: waits, then passes" 0 "checks 1/2 pending: quickstart
checks 2/2 green"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$q_cancel,$q_live")" "$(pr OPEN CLEAN "" "$lint_ok,$q_cancel,$(cr_at quickstart FAILURE 41261363000 $t0)")"
run 7 --checks --require quickstart --interval 0
expect "cancelled plus live that then fails: the failure is reported" 1 "checks 1/2 pending: quickstart
FAILED quickstart (run 41261363000)
REQUIRED quickstart failed"

# 10c. (#468) the only quickstart on the head is cancelled while a second e2e-kind run has not created
# its jobs yet: wait for that run, then judge its result; with no live run, fail as before.
prh() { # <rollup>: an open PR whose head is abc1234…
  printf '{"state":"OPEN","mergeStateStatus":"CLEAN","mergeCommit":null,"headRefOid":"abc1234def","statusCheckRollup":[%s]}' "$1"
}
q_first=$(cr_at quickstart CANCELLED 41261362803 2026-11-01T10:00:00Z)
q_second=$(cr_at quickstart SUCCESS 41261363000 2026-11-01T10:01:00Z)
seq_set "$(prh "$lint_ok,$q_first")" "$(prh "$lint_ok,$q_first")" "$(prh "$lint_ok,$q_first,$q_second")"
printf '41261363000\n41261363000\n\n' >"$FAKE_DIR/runs"
run 7 --checks --require quickstart --interval 0
expect "cancelled while another e2e-kind run is queued: waits, then passes" 0 "waiting for e2e-kind run 41261363000 on abc1234
checks 2/2 green"
grep -q -- '^run list --commit abc1234def --workflow e2e-kind.yml' "$FAKE_DIR/calls" ||
  { echo "FAIL: lists the e2e-kind runs of the head commit" >&2; fail=1; }
seq_set "$(prh "$lint_ok")" "$(prh "$lint_ok,$q_second")"
printf '41261363000\n\n' >"$FAKE_DIR/runs"
run 7 --checks --require quickstart --interval 0
expect "required check missing while an e2e-kind run is queued: waits" 0 "waiting for e2e-kind run 41261363000 on abc1234
checks 2/2 green"
seq_set "$(prh "$lint_ok,$q_first")"
printf '\n' >"$FAKE_DIR/runs"
run 7 --checks --require quickstart --interval 0
expect "cancelled with no live e2e-kind run: failed, with the dispatch hint" 1 "FAILED quickstart (run 41261362803)
REQUIRED quickstart failed
only cancelled runs of quickstart on this head; run: gh workflow run e2e-kind.yml --ref <branch>"
seq_set "$(prh "$lint_ok,$(cr go COMPLETED FAILURE 11),$q_first")"
printf '41261363000\n' >"$FAKE_DIR/runs"
run 7 --checks --require quickstart --interval 0
expect "another check failed: no waiting for the live run" 1 "FAILED go (run 11)
FAILED quickstart (run 41261362803)
REQUIRED quickstart failed
only cancelled runs of quickstart on this head; run: gh workflow run e2e-kind.yml --ref <branch>"

# A cancelled `decide` (not required) and a red `e2e result` of the first e2e-kind run, while the
# second run on the head is still live: wait for it; its green checks supersede the first run's.
crw() { # <name> <conclusion> <run id> <startedAt>: a check of the e2e-kind workflow
  printf '{"name":"%s","workflowName":"e2e-kind","status":"COMPLETED","conclusion":"%s","startedAt":"%s","detailsUrl":"https://github.com/o/r/actions/runs/%s/job/9"}' "$1" "$2" "$4" "$3"
}
d1=$(crw decide CANCELLED 41261362803 2026-11-01T10:00:00Z)
r1=$(crw "e2e result" FAILURE 41261362803 2026-11-01T10:00:30Z)
d2=$(crw decide SUCCESS 41261363000 2026-11-01T10:01:00Z)
r2=$(crw "e2e result" SUCCESS 41261363000 2026-11-01T10:30:00Z)
seq_set "$(prh "$lint_ok,$d1,$r1")" "$(prh "$lint_ok,$d1,$r1,$d2")" "$(prh "$lint_ok,$d1,$r1,$d2,$r2")"
printf '41261363000\n41261363000\n\n' >"$FAKE_DIR/runs"
run 7 --checks --require "e2e result" --interval 0
expect "cancelled decide and red e2e result of an earlier run, another run live: waits, then passes" 0 "waiting for e2e-kind run 41261363000 on abc1234
checks 3/3 green"
seq_set "$(prh "$lint_ok,$d1,$r1")"
printf '\n' >"$FAKE_DIR/runs"
run 7 --checks --require "e2e result" --interval 0
expect "cancelled decide and red e2e result with no live run: fails" 1 "FAILED decide (run 41261362803)
FAILED e2e result (run 41261362803)
REQUIRED e2e result failed"

# All green, but the `e2e` label dispatched an e2e-kind run on the head that has not created its
# checks yet: not final until it has.
er1=$(crw "e2e result" SUCCESS 41261362803 2026-11-01T10:00:30Z)
er2=$(crw "e2e result" SUCCESS 41265167595 2026-11-01T10:40:00Z)
seq_set "$(prh "$lint_ok,$er1")" "$(prh "$lint_ok,$er1,$er2")"
printf '41265167595\n\n' >"$FAKE_DIR/runs"
run 7 --checks --require "e2e result" --interval 0
expect "all green while a dispatched e2e-kind run is queued: waits" 0 "waiting for e2e-kind run 41265167595 on abc1234
checks 2/2 green"

# 11. (#547) --require looks at every check run of the head (gh api), not only the PR rollup, and
# `--require e2e` passes on a green e2e-lab status or a green quickstart run.
row() { # <name> <status> <conclusion> <run id> <startedAt>: one row of head_runs' output
  printf '{"name":"%s","status":"%s","conclusion":"%s","startedAt":"%s","completedAt":"%s","detailsUrl":"https://github.com/o/r/actions/runs/%s/job/9","workflowName":""}\n' "$1" "$2" "$3" "$5" "$5" "$4"
}
st() { # <context> <state>: a commit status in the rollup
  printf '{"context":"%s","state":"%s","targetUrl":"https://argo.example.invalid/workflows/x/y"}' "$1" "$2"
}
q_skip_new=$(cr_at quickstart SKIPPED 51 2026-11-01T11:00:00Z)
seq_set "$(prh "$lint_ok,$q_skip_new")"
row quickstart COMPLETED SUCCESS 50 2026-11-01T10:00:00Z >"$FAKE_DIR/rows"
printf '\n' >"$FAKE_DIR/runs"
run 7 --checks --require quickstart --interval 0
expect "a dispatched success on the head counts next to the rollup's skipped run" 0 "checks 2/2 green"
seq_set "$(prh "$lint_ok,$q_skip_new")"
run 7 --checks --require quickstart --interval 0
expect "... but without the success in the check runs it is still skipped" 1 "REQUIRED quickstart skipped
only skipped runs of quickstart on this head; run: gh workflow run e2e-kind.yml --ref <branch>"
grep -q -- '^api repos/{owner}/{repo}/commits/abc1234def/check-runs' "$FAKE_DIR/calls" || { echo "FAIL: reads the head's check runs" >&2; fail=1; }

seq_set "$(prh "$lint_ok,$(st e2e-lab SUCCESS)")"
printf '\n' >"$FAKE_DIR/runs"
run 7 --checks --require e2e --interval 0
expect "--require e2e: e2e-lab success passes" 0 "checks 2/2 green"
seq_set "$(prh "$lint_ok,$q_skip_new,$(st e2e-lab SUCCESS)")"
printf '41265167595\n' >"$FAKE_DIR/runs"
run 7 --checks --require e2e --interval 0
expect "--require e2e: e2e-lab success beats a skipped quickstart and a live e2e-kind run" 0 "checks 2/2 green"
seq_set "$(prh "$lint_ok,$q_skip_new")"
row quickstart COMPLETED SUCCESS 50 2026-11-01T10:00:00Z >"$FAKE_DIR/rows"
printf '\n' >"$FAKE_DIR/runs"
run 7 --checks --require e2e --interval 0
expect "--require e2e: a dispatched quickstart success passes" 0 "checks 2/2 green"
seq_set "$(prh "$lint_ok,$(st e2e-lab FAILURE),$(cr_at quickstart SUCCESS 52 2026-11-01T10:00:00Z)")"
printf '\n' >"$FAKE_DIR/runs"
run 7 --checks --require e2e --interval 0
expect "--require e2e: quickstart success passes although e2e-lab is red" 0 "checks 2/2 green"
seq_set "$(prh "$lint_ok,$(st e2e-lab FAILURE)")"
run 7 --checks --require e2e --interval 0
expect "--require e2e: a red e2e-lab alone fails" 1 "FAILED e2e-lab"
seq_set "$(prh "$lint_ok,$(st e2e-lab PENDING)")"
run 7 --checks --require e2e --timeout 1s --interval 1
expect "--require e2e: a pending e2e-lab is waited for" 2 "checks 1/2 pending: e2e-lab
TIMEOUT"
seq_set "$(prh "$lint_ok,$q_skip_new")"
run 7 --checks --require e2e --interval 0
expect "--require e2e: only a skipped quickstart says how to get a green e2e" 1 "REQUIRED e2e skipped
no green e2e on this head (skipped quickstart, no e2e-lab success); run: hack/e2e-remote.sh --argo (posts e2e-lab), or gh workflow run e2e-kind.yml --ref <branch>"
seq_set "$(prh "$lint_ok")"
run 7 --checks --require e2e --interval 0
expect "--require e2e: nothing at all is missing" 1 "REQUIRED e2e missing
no green e2e on this head (missing quickstart, no e2e-lab success); run: hack/e2e-remote.sh --argo (posts e2e-lab), or gh workflow run e2e-kind.yml --ref <branch>"
seq_set "$(prh "$lint_ok,$(st e2e-lab SUCCESS),$go_ok")"
printf '\n' >"$FAKE_DIR/runs"
run 7 --checks --require e2e --require go --interval 0
expect "--require e2e together with another required check" 0 "checks 3/3 green"

# success then a newer FAILURE stays red; success then a newer skip is green (#392 intent for failures)
seq_set "$(prh "$lint_ok,$q_ok,$(cr_at quickstart FAILURE 24 2026-11-01T10:20:00Z)")"
printf '\n' >"$FAKE_DIR/runs"
run 7 --checks --require quickstart --interval 0
expect "a newer failure after a success stays failed" 1 "FAILED quickstart (run 24)
REQUIRED quickstart failed"
seq_set "$(prh "$lint_ok,$q_ok,$(cr_at quickstart SKIPPED 25 2026-11-01T10:20:00Z)")"
run 7 --checks --require quickstart --interval 0
expect "a newer skip after a success is green" 0 "checks 2/2 green"
# success, failure, then a newer skip: the failure is not hidden by the skip
seq_set "$(prh "$lint_ok,$q_ok,$(cr_at quickstart FAILURE 24 2026-11-01T10:20:00Z),$(cr_at quickstart SKIPPED 26 2026-11-01T10:30:00Z)")"
run 7 --checks --require quickstart --interval 0
expect "success, failure, then a newer skip stays failed" 1 "FAILED quickstart (run 24)
REQUIRED quickstart failed"
# --require e2e judges quickstart with the same rule
seq_set "$(prh "$lint_ok,$q_ok,$(cr_at quickstart FAILURE 24 2026-11-01T10:20:00Z)")"
run 7 --checks --require e2e --interval 0
expect "--require e2e: a quickstart success followed by a newer failure is red" 1 "FAILED quickstart (run 24)
REQUIRED e2e failed"
seq_set "$(prh "$lint_ok,$q_ok,$(cr_at quickstart SKIPPED 25 2026-11-01T10:20:00Z)")"
run 7 --checks --require e2e --interval 0
expect "--require e2e: a quickstart success followed by a newer skip is green" 0 "checks 2/2 green"
# an explicit --require quickstart stays required next to --require e2e
seq_set "$(prh "$lint_ok,$(st e2e-lab SUCCESS),$q_skip_new")"
run 7 --checks --require e2e --require quickstart --interval 0
expect "--require e2e --require quickstart: e2e-lab green does not excuse a skipped quickstart" 1 "REQUIRED quickstart skipped
only skipped runs of quickstart on this head; run: gh workflow run e2e-kind.yml --ref <branch>"

usage_line="usage: hack/pr-wait.sh <PR> [--merged] [--checks] [--require CHECK|e2e] [--timeout DURATION] [--interval SECONDS]"
e2e_ok=$(cr "e2e result" COMPLETED SUCCESS 14)
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok")"
run 7 --checks --require "e2e result" --interval 0
expect "required check missing" 1 "REQUIRED e2e result missing"
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$(cr "e2e result" COMPLETED SKIPPED 14)")"
run 7 --checks --require "e2e result" --interval 0
expect "required check skipped" 1 "REQUIRED e2e result skipped
only skipped runs of e2e result on this head; run: gh workflow run e2e-kind.yml --ref <branch>"
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

# 7b. #562: the ruleset's required contexts must exist on the head. A queued workflow run has no
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
seq_set "$(pr OPEN CLEAN "" "$lint_ok,$go_ok")"
run 7 --interval 0 --timeout 0
expect "without --checks the ruleset is not consulted" 2 "checks 2/2 green
TIMEOUT"

# 7c. (#647) a draft is never green; after ready_for_review the draft's runs are ignored
seq_set '{"state":"OPEN","isDraft":true,"mergeStateStatus":"BLOCKED","mergeCommit":null,"statusCheckRollup":[{"name":"quick checks","status":"COMPLETED","conclusion":"SUCCESS","startedAt":"2026-11-01T10:00:00Z"}]}'
run 7 --checks --interval 0
expect "--checks on a draft: DRAFT, exit 5" 5 "DRAFT: not tested, mark ready for review (hack/ship.sh --ready, gh pr ready)"
rd() { # <rollup>: a ready PR
  printf '{"state":"OPEN","isDraft":false,"mergeStateStatus":"CLEAN","mergeCommit":null,"statusCheckRollup":[%s]}' "$1"
}
cr_ci() { cr_at "$@" | sed 's/^{/{"workflowName":"ci",/'; }
old_go=$(cr_ci go SUCCESS 61 2026-11-01T10:00:00Z)
new_go=$(cr_ci go SUCCESS 62 2026-11-01T11:05:00Z)
seq_set "$(rd "$old_go")" "$(rd "$old_go")" "$(rd "$old_go,$new_go")"
printf '2026-11-01T11:00:00Z\n' >"$FAKE_DIR/ready"
printf 'go\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0
expect "after ready: the draft's green run is ignored until the ready run exists" 0 "waiting for the ready run
checks 1/1 green"
# dco has no ready run: its draft-era verdict stands
dco_old=$(cr_at dco SUCCESS 63 2026-11-01T10:00:00Z | sed 's/^{/{"workflowName":"dco",/')
seq_set "$(rd "$old_go,$new_go,$dco_old")"
printf 'go\ndco\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0
expect "after ready: a draft-era dco run still counts" 0 "checks 3/3 green"
# a draft-era e2e-lab status still satisfies --require e2e
lab_old='{"context":"e2e-lab","state":"SUCCESS","startedAt":"2026-11-01T10:00:00Z","targetUrl":"https://argo.example.invalid/workflows/x/y"}'
seq_set "$(rd "$old_go,$new_go,$lab_old")"
printf 'go\n' >"$FAKE_DIR/rules"
run 7 --checks --require e2e --interval 0
expect "after ready: a draft-era e2e-lab status satisfies --require e2e" 0 "checks 3/3 green"
# per workflow run: a draft-era FAILURE (#628) and a draft run cancelled after the ready event (#649)
rdh() { # <rollup>: a ready PR with a head commit
  printf '{"state":"OPEN","isDraft":false,"mergeStateStatus":"CLEAN","mergeCommit":null,"headRefOid":"abc1234","statusCheckRollup":[%s]}' "$1"
}
printf '[{"databaseId":61,"createdAt":"2026-11-01T10:00:00Z"},{"databaseId":62,"createdAt":"2026-11-01T11:00:05Z"}]' >"$FAKE_DIR/ciruns"
bad_go=$(cr_at go FAILURE 61 2026-11-01T10:30:00Z | sed 's/^{/{"workflowName":"ci",/')
seq_set "$(rdh "$bad_go")" "$(rdh "$bad_go,$new_go")"
printf '2026-11-01T11:00:00Z\n' >"$FAKE_DIR/ready"
printf 'go\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0
expect "after ready: a draft-era red go is superseded by the ready run" 0 "waiting for the ready run
checks 1/1 green"
cx_go=$(cr_at go CANCELLED 61 2026-11-01T11:00:09Z | sed 's/^{/{"workflowName":"ci",/')
cx_web=$(cr_at web CANCELLED 61 2026-11-01T11:00:10Z | sed 's/^{/{"workflowName":"ci",/')
new_web=$(cr_ci web SUCCESS 62 2026-11-01T11:05:00Z)
seq_set "$(rdh "$cx_go,$cx_web,$new_go,$new_web")"
printf '2026-11-01T11:00:00Z\n' >"$FAKE_DIR/ready"
printf '[{"databaseId":61,"createdAt":"2026-11-01T10:00:00Z"},{"databaseId":62,"createdAt":"2026-11-01T11:00:05Z"}]' >"$FAKE_DIR/ciruns"
printf 'go\nweb\n' >"$FAKE_DIR/rules"
run 7 --checks --interval 0
expect "after ready: a draft run cancelled after the ready event is superseded" 0 "checks 2/2 green"
# (#676) a draft-era run re-run by hand after the ready event replays draft=true (red); it is ignored
rr_go=$(cr_at go FAILURE 61 2026-11-01T11:10:00Z | sed 's/^{/{"workflowName":"ci",/')
seq_set "$(rdh "$rr_go,$new_go")"
printf '2026-11-01T11:00:00Z\n' >"$FAKE_DIR/ready"
printf 'go\n' >"$FAKE_DIR/rules"
printf '[{"databaseId":61,"createdAt":"2026-11-01T10:00:00Z","startedAt":"2026-11-01T11:10:00Z"},{"databaseId":62,"createdAt":"2026-11-01T11:00:05Z","startedAt":"2026-11-01T11:00:05Z"}]' >"$FAKE_DIR/ciruns"
run 7 --checks --interval 0
expect "after ready: a re-run draft-era run is ignored, not reported FAILED" 0 "checks 1/1 green"
# clock skew: a ready run created 20 s before the ready event is kept (30 s slack)
sk_go=$(cr_at go SUCCESS 62 2026-11-01T10:59:45Z | sed 's/^{/{"workflowName":"ci",/')
seq_set "$(rdh "$sk_go")"
printf '[{"databaseId":62,"createdAt":"2026-11-01T10:59:40Z","startedAt":"2026-11-01T10:59:40Z"}]' >"$FAKE_DIR/ciruns"
run 7 --checks --interval 0
expect "after ready: a run 20 s before the ready event is kept (clock skew)" 0 "checks 1/1 green"
rm -f "$FAKE_DIR/ready" "$FAKE_DIR/rules" "$FAKE_DIR/ciruns"

# 7d. (#661) a stale PR head: GitHub's headRefOid is not origin's branch tip
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
expect "no PR is a usage error" 64 "usage: hack/pr-wait.sh <PR> [--merged] [--checks] [--require CHECK|e2e] [--timeout DURATION] [--interval SECONDS]"
run 7 --timeout soon
expect "bad timeout is a usage error" 64 "usage: hack/pr-wait.sh <PR> [--merged] [--checks] [--require CHECK|e2e] [--timeout DURATION] [--interval SECONDS]"

exit "$fail"
