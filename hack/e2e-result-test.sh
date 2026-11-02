#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/e2e-result.sh (the e2e-kind `e2e result` job) with a stub gh.
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Stub gh. The runs on the head: the Nth listing answers $FAKE/runs.N (the last one repeats); each
# line is `<id> <status> <conclusion>`. A run's quickstart verdict: the next line of $FAKE/q.<id>
# per call (the last one repeats), as the real jq prints it.
cat >"$tmp/gh" <<'SH'
#!/usr/bin/env bash
case "$*" in
*/actions/workflows/e2e-kind.yml/runs*)
  n=$(($(cat "$FAKE/rn" 2>/dev/null || echo 0) + 1)); echo "$n" >"$FAKE/rn"
  while [[ $n -gt 1 && ! -e $FAKE/runs.$n ]]; do n=$((n - 1)); done
  cat "$FAKE/runs.$n" 2>/dev/null || true ;;
*/actions/runs/*/jobs*)
  id=$2; id=${id#*actions/runs/}; id=${id%%/*}
  c=$(($(cat "$FAKE/c.$id" 2>/dev/null || echo 0) + 1)); echo "$c" >"$FAKE/c.$id"
  total=$(wc -l <"$FAKE/q.$id"); [[ $c -le $total ]] || c=$total
  sed -n "${c}p" "$FAKE/q.$id" ;;
*) echo "unexpected gh $*" >&2; exit 1 ;;
esac
SH
chmod +x "$tmp/gh"
export GH="$tmp/gh" GH_REPO=o/r HEAD_SHA=abc1234def RUN_ID=99 POLL=0 WAIT_MAX=100

fail=0
# fresh: a new fake world. Then write runs.N and q.<id> files into $FAKE.
fresh() { FAKE=$(mktemp -d "$tmp/w.XXXX"); export FAKE; }
# want <exit> <description> <expected output substring> [VAR=value...]
want() {
  local code=$1 desc=$2 sub=$3 out rc=0
  shift 3
  out=$(env "$@" hack/e2e-result.sh 2>&1) || rc=$?
  if [[ $rc -eq $code && $out == *"$sub"* ]]; then
    echo "ok: $desc"
  else
    echo "FAIL: $desc (exit $rc, want $code; want output containing '$sub')" >&2
    printf '%s\n' "$out" | sed 's/^/  | /' >&2
    fail=1
  fi
}

fresh
want 0 "plain run: e2e passed" "" DECIDE=success QUICKSTART=success
want 0 "plain run: e2e not needed" "" DECIDE=success QUICKSTART=skipped
want 1 "plain run: e2e failed" "did not pass (failure)" DECIDE=success QUICKSTART=failure
want 1 "plain run: decide cancelled" "did not pass (cancelled)" DECIDE=cancelled QUICKSTART=skipped

# 1. covered: the covering run's own quickstart decides, not the newest finished run on the head.
fresh
echo failure >"$FAKE/q.41"
printf '41 completed failure\n42 completed success\n' >"$FAKE/runs.1"
want 1 "covered: the covering run's e2e failed, a later green label run does not count" "run 41 ran the e2e on this commit: failure" \
  DECIDE=success QUICKSTART=skipped COVERED=41
fresh
echo success >"$FAKE/q.41"
want 0 "covered: the covering run's e2e passed" "success" DECIDE=success QUICKSTART=skipped COVERED=41
fresh
printf 'pending\npending\nsuccess\n' >"$FAKE/q.41"
want 0 "covered: waits for the covering run's e2e" "waiting for the e2e of run 41" DECIDE=success QUICKSTART=skipped COVERED=41
fresh
echo skipped >"$FAKE/q.41"
want 1 "covered: a covering run that skipped the e2e fails closed" "skipped" DECIDE=success QUICKSTART=skipped COVERED=41
fresh
echo pending >"$FAKE/q.41"
want 1 "covered: gives up after WAIT_MAX" "gave up" DECIDE=success QUICKSTART=skipped COVERED=41 WAIT_MAX=2 POLL=1

# 2. mirror (another label): never green while the e2e on the head still runs.
fresh
printf '41 in_progress -\n' >"$FAKE/runs.1"
printf '41 in_progress -\n' >"$FAKE/runs.2"
printf '41 completed failure\n' >"$FAKE/runs.3"
echo failure >"$FAKE/q.41"
want 1 "mirror: waits for the live e2e, then mirrors its failure" "waiting for e2e-kind run 41" DECIDE=skipped QUICKSTART=skipped MIRROR=true
[[ $(cat "$FAKE/rn") -eq 3 ]] || { echo "FAIL: mirror listed the runs $(cat "$FAKE/rn") times, want 3" >&2; fail=1; }
fresh
printf '41 in_progress -\n' >"$FAKE/runs.1"
printf '41 completed success\n' >"$FAKE/runs.2"
echo success >"$FAKE/q.41"
want 0 "mirror: waits for the live e2e, then mirrors its success" "newest run that ran the e2e on this commit: 41 (success)" \
  DECIDE=skipped QUICKSTART=skipped MIRROR=true
fresh
printf '41 completed failure\n42 completed success\n43 completed success\n' >"$FAKE/runs.1"
echo failure >"$FAKE/q.41"
echo skipped >"$FAKE/q.42"
echo skipped >"$FAKE/q.43"
want 1 "mirror: a green label run after a red e2e does not hide it (the #468 scenario)" "run 41: failure" \
  DECIDE=skipped QUICKSTART=skipped MIRROR=true
fresh
printf '41 completed failure\n44 completed success\n' >"$FAKE/runs.1"
echo failure >"$FAKE/q.41"
echo success >"$FAKE/q.44"
want 0 "mirror: a later green e2e (re-run or dispatch) counts" "44 (success)" DECIDE=skipped QUICKSTART=skipped MIRROR=true
fresh
printf '41 completed success\n' >"$FAKE/runs.1"
echo skipped >"$FAKE/q.41"
want 0 "mirror: no run ran the e2e, the last one was green" "newest finished run: success" DECIDE=skipped QUICKSTART=skipped MIRROR=true
fresh
printf '41 completed failure\n' >"$FAKE/runs.1"
echo none >"$FAKE/q.41"
want 1 "mirror: no run ran the e2e, the last one failed" "concluded failure" DECIDE=skipped QUICKSTART=skipped MIRROR=true
fresh
want 0 "mirror: no other run on the head" "newest finished run: none" DECIDE=skipped QUICKSTART=skipped MIRROR=true

[[ $fail -eq 0 ]] && echo "e2e-result tests passed"
exit "$fail"
