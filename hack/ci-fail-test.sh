#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/ci-fail.sh offline: `gh` is a stub on PATH serving canned PR check rollups, run job lists
# and --log-failed logs (with ANSI codes, timestamps and noise to filter).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ci-fail-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
export PATH="$tmp/bin:$PATH" FAKE_CALLS="$tmp/calls"

url=https://github.com/o/r/actions/runs
cat >"$tmp/rollup-fail.json" <<JSON
{"statusCheckRollup":[
 {"name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"$url/100/job/1"},
 {"name":"go","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"$url/200/job/21"},
 {"name":"web","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"$url/200/job/22"},
 {"context":"claim","state":"FAILURE","targetUrl":"https://example.invalid/claim"}]}
JSON
cat >"$tmp/rollup-ok.json" <<JSON
{"statusCheckRollup":[{"name":"lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"$url/100/job/1"}]}
JSON
cat >"$tmp/jobs.json" <<JSON
{"jobs":[{"name":"go","conclusion":"failure","databaseId":21,"url":"$url/200/job/21"},
 {"name":"web","conclusion":"success","databaseId":22,"url":"$url/200/job/22"}]}
JSON

# The go job log: tab separated job/step/timestamp prefix, ANSI colour, noise and Ginkgo/Go output.
esc=$(printf '\033')
t=$(printf '\t')
{
  p="go${t}Test${t}2026-10-31T10:00:0"
  echo "${p}1.1234567Z go: downloading github.com/x/y v1.2.3"
  echo "${p}2.0000000Z ${esc}[31m--- FAIL: TestThing (0.01s)${esc}[0m"
  echo "${p}3.0000000Z irrelevant chatter"
  echo "${p}4.0000000Z [FAIL] Reconciler creates a sandbox [It] sizes it"
  echo "${p}5.0000000Z   Expected"
  echo "${p}5.0000001Z       <int>: 1"
  echo "${p}5.0000002Z   to equal"
  echo "${p}5.0000003Z       <int>: 2"
  echo "${p}6.0000000Z ./internal/foo/foo.go:12:3: undefined: bar"
  echo "${p}7.0000000Z panic: boom"
  echo "${p}8.0000000Z FAIL${t}github.com/stateracloud/koinon/internal/foo${t}0.123s"
  echo "${p}9.0000000Z ##[error]Process completed with exit code 1."
  echo "${p}9.0000001Z ##[error]Process completed with exit code 1."
} >"$tmp/log-go.txt"
{
  for i in $(seq 1 60); do echo "web${t}Test${t}2026-10-31T10:00:00.0000000Z --- FAIL: TestMany$i (0.00s)"; done
} >"$tmp/log-many.txt"
echo "web${t}Lint${t}2026-10-31T10:00:00.0000000Z nothing recognisable here" >"$tmp/log-web.txt"

cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_CALLS"
t=$(dirname "$0")/..
case "$1 $2" in
"pr view") cat "$t/${FAKE_ROLLUP:-rollup-fail}.json" ;;
"run view")
  case "$*" in
  *--json*) cat "$t/jobs.json" ;;
  *"--job 21"*)
    [ -z "${FAKE_GHFAIL:-}" ] || { echo "gh: HTTP 502: Temporary failure resolving api.github.com"; exit 1; }
    cat "$t/log-${FAKE_GOLOG:-go}.txt" ;;
  *"--job 22"*) cat "$t/log-web.txt" ;;
  *) exit 9 ;;
  esac ;;
*) exit 9 ;;
esac
SH
chmod +x "$tmp/bin/gh"

fail=0
run() {
  rc=0
  out=$("$repo/hack/ci-fail.sh" "$@" 2>&1) || rc=$?
}
check() { # <description> <condition command...>
  local d=$1
  shift
  if "$@"; then echo "ok: $d"; else
    echo "FAIL: $d" >&2
    printf '%s\n' "$out" >&2
    fail=1
  fi
}
# shellcheck disable=SC2329 # called through check
has() { printf '%s\n' "$out" | grep -qF -- "$1"; }
# shellcheck disable=SC2329 # called through check
hasnt() { ! has "$1"; }

run 7
check "PR with failures exits 1" test "$rc" = 1
check "lists the failing jobs" has "job go"
check "lists the second failing job" has "job web"
check "run link printed" has "run $url/200/job/21"
check "non-actions status noted" has "no GitHub Actions run: https://example.invalid/claim"
check "passing job is not listed" hasnt "job lint"
check "keeps --- FAIL" has "--- FAIL: TestThing (0.01s)"
check "keeps [FAIL] block" has "[FAIL] Reconciler creates a sandbox [It] sizes it"
check "keeps Expected/to equal" has "to equal"
check "keeps expected values" has "<int>: 2"
check "keeps compiler error" has "./internal/foo/foo.go:12:3: undefined: bar"
check "keeps panic" has "panic: boom"
check "keeps FAIL<TAB>pkg" has "FAIL${t}github.com/stateracloud/koinon/internal/foo${t}0.123s"
check "keeps ##[error] once" test "$(printf '%s\n' "$out" | grep -c '##\[error\]')" = 1
check "drops go: downloading" hasnt "go: downloading"
check "drops chatter" hasnt "irrelevant chatter"
check "strips ANSI" hasnt "$esc"
check "strips timestamps" hasnt "2026-10-31"
check "strips job/step prefix" hasnt "go${t}Test"
check "fallback shows the tail when nothing matches" has "nothing recognisable here"
check "uses --job to fetch one job's log" grep -q -- '--job 21' "$FAKE_CALLS"

run 7 --job web
check "--job filters" test "$rc" = 1
check "--job drops the other job" hasnt "job go"
check "--job keeps the named one" has "job web"

# at most 30 lines per job
cp "$tmp/log-many.txt" "$tmp/log-go.txt"
run 200000000 --job go
check "run id: exit 1" test "$rc" = 1
check "run id: only the failed job" hasnt "job web"
check "at most 30 lines per job" test "$(printf '%s\n' "$out" | wc -l | tr -d ' ')" -le 30
check "run id fetched via --json jobs" grep -q -- 'run view 200000000 --json jobs' "$FAKE_CALLS"

FAKE_ROLLUP=rollup-ok run 7
check "nothing failed: exit 0" test "$rc" = 0
check "nothing failed: no output" test -z "$out"

run
check "no argument is a usage error" test "$rc" = 64

# failure classes: every recorded sample under hack/testdata/ci-fail/ names its expected class
# ("# expect: <category> <retry>" on line 1); the first table row that matches wins, none is unknown.
samples=0
for f in "$repo"/hack/testdata/ci-fail/*.log; do
  samples=$((samples + 1))
  want=$(sed -n '1s/^# expect: //p' "$f")
  sed 1d "$f" >"$tmp/log-go.txt"
  run 7 --job go
  got=$(printf '%s\n' "$out" | sed -n 's/^class=\([a-z]*\) retry=\([a-z]*\)$/\1 \2/p')
  check "class of $(basename "$f") is $want" test "$got" = "$want"
done
check "samples exist" test "$samples" -ge 10

# order matters: the same log gets the other class when the rows swap
cp "$repo/hack/testdata/ci-fail/order-infra-over-test.log" "$tmp/log-go.txt"
{
  printf 'test\tno\t--- FAIL\n'
  printf 'infra\tyes\tErrImagePull\n'
} >"$tmp/swapped.tsv"
CI_FAIL_CLASSES="$tmp/swapped.tsv" run 7 --job go
check "swapped table: first row wins" has "class=test retry=no"
printf '# only a comment\n' >"$tmp/empty.tsv"
CI_FAIL_CLASSES="$tmp/empty.tsv" run 7 --job go
check "empty table falls back to unknown" has "class=unknown retry=unknown"
FAKE_GHFAIL=1 run 7 --job go
check "failing gh: class unknown" has "class=unknown retry=unknown"
check "failing gh: its error text is not classified" hasnt "class=infra"
check "failing gh: still exit 1" test "$rc" = 1
run 7
check "a status without a run is class unknown" has "class=unknown retry=unknown"

# table shape: three tab separated columns, retry yes|no, every regex compiles
badrows=0
while IFS=$'\t' read -r c r re; do
  case $c in '' | '#'*) continue ;; esac
  case $r in yes | no) ;; *) badrows=$((badrows + 1)) ;; esac
  rc=0
  printf '' | grep -Eq -e "$re" || rc=$?
  [ "$rc" -le 1 ] || badrows=$((badrows + 1))
  [ -n "$c" ] && [ -n "$re" ] || badrows=$((badrows + 1))
done <"$repo/hack/ci-fail-classes.tsv"
check "class table rows are well formed" test "$badrows" = 0

exit "$fail"
