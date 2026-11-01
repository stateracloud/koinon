#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/pr-wait.sh offline: `gh` is a stub on PATH that answers `gh pr view` with the next JSON
# state of a scripted sequence (the last one repeats) and records every call.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
export FAKE_DIR="$tmp/seq" PATH="$tmp/bin:$PATH"

cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_DIR/calls"
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
expect "a newer skip after a success is not green" 1 "REQUIRED quickstart skipped
only skipped runs of quickstart on this head; run: gh workflow run e2e-kind.yml --ref <branch>"
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

usage_line="usage: hack/pr-wait.sh <PR> [--merged] [--checks] [--require CHECK] [--timeout DURATION] [--interval SECONDS]"
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
