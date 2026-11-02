#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/e2e-label.sh (the e2e-label workflow's dispatcher) with a stub gh.
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d "${TMPDIR:-/tmp}/e2e-label-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# Stub gh. The runs listing applies the script's own --jq filter to $FAKE/runs.N.json for the Nth
# listing (the last one repeats). A run's quickstart verdict: the next line of $FAKE/q.<id> per call
# (the last one repeats; none when the file is absent). The branch head: $FAKE/branch (default the
# head). Every call is logged to $FAKE/calls. After `workflow run`, listings add dispatch run 999
# unless $FAKE/nodispatch exists.
cat >"$tmp/gh" <<'SH'
#!/usr/bin/env bash
echo "$*" >>"$FAKE/calls"
case "$*" in
*/actions/workflows/e2e-kind.yml/runs*)
  n=$(($(cat "$FAKE/rn" 2>/dev/null || echo 0) + 1)); echo "$n" >"$FAKE/rn"
  while [[ $n -gt 1 && ! -e $FAKE/runs.$n.json ]]; do n=$((n - 1)); done
  if [[ -e $FAKE/dispatched && ! -e $FAKE/nodispatch ]]; then # the dispatched run shows up
    jq '.workflow_runs += [{"id":999,"event":"workflow_dispatch","display_title":"e2e-kind","status":"queued"}]' "$FAKE/runs.$n.json" | jq -r "$4"
  else
    jq -r "$4" "$FAKE/runs.$n.json"
  fi ;;
*/actions/runs/*/jobs*)
  id=$2; id=${id#*actions/runs/}; id=${id%%/*}
  [[ -e $FAKE/q.$id ]] || { echo none; exit; }
  c=$(($(cat "$FAKE/c.$id" 2>/dev/null || echo 0) + 1)); echo "$c" >"$FAKE/c.$id"
  total=$(wc -l <"$FAKE/q.$id"); [[ $c -le $total ]] || c=$total
  sed -n "${c}p" "$FAKE/q.$id" ;;
*/branches/*) cat "$FAKE/branch" 2>/dev/null || echo abc1234def ;;
"workflow run"*) touch "$FAKE/dispatched" ;;
*) echo "unexpected gh $*" >&2; exit 1 ;;
esac
SH
chmod +x "$tmp/gh"
export GH="$tmp/gh" GH_REPO=o/r HEAD_SHA=abc1234def BRANCH=fix/1-x POLL=0 WAIT_MAX=5 LIST_POLL=0 LIST_MAX=3

fail=0
fresh() { FAKE=$(mktemp -d "$tmp/w.XXXXXX"); export FAKE; }
# run_json <N> <runs...>: each run is id:event:title:status.
run_json() {
  local n=$1 r id ev title st out=""
  shift
  for r in "$@"; do
    IFS=: read -r id ev title st <<<"$r"
    out+=${out:+,}$(printf '{"id":%s,"event":"%s","display_title":"%s","status":"%s"}' "$id" "$ev" "$title" "$st")
  done
  printf '{"workflow_runs":[%s]}' "$out" >"$FAKE/runs.$n.json"
}
# want <dispatch|none> <description> <output substring>
want() {
  local expect=$1 desc=$2 sub=$3 out rc=0 got=none
  out=$(hack/e2e-label.sh 2>&1) || rc=$?
  grep -q '^workflow run e2e-kind.yml --ref fix/1-x$' "$FAKE/calls" 2>/dev/null && got=dispatch
  if [[ $rc -eq 0 && $got == "$expect" && $out == *"$sub"* ]]; then
    echo "ok: $desc"
  else
    echo "FAIL: $desc (exit $rc, $got, want $expect and '$sub')" >&2
    printf '%s\n' "$out" | sed 's/^/  | /' >&2
    fail=1
  fi
}

fresh
run_json 1
want dispatch "no run on the head: dispatches, and waits until the run is listed" "dispatched run 999"
fresh
run_json 1 "41:pull_request:e2e-kind:completed"
echo skipped >"$FAKE/q.41"
want dispatch "the head's run skipped the e2e: dispatches" "dispatching"
fresh
run_json 1 "41:pull_request:e2e-kind:completed"
echo failure >"$FAKE/q.41"
want dispatch "the head's e2e failed: dispatches (re-labelling retries)" "dispatching"
fresh
run_json 1 "41:pull_request:e2e-kind:completed"
echo success >"$FAKE/q.41"
want none "the head already passed the e2e: nothing" "already passed"
fresh
run_json 1 "41:pull_request:e2e-kind:in_progress"
echo pending >"$FAKE/q.41"
want none "a run on the head runs the e2e: nothing" "runs the e2e"
fresh
run_json 1 "41:pull_request:e2e-kind:queued"
run_json 2 "41:pull_request:e2e-kind:in_progress"
printf 'none\npending\n' >"$FAKE/q.41"
want none "opened with the label: waits for the run to decide, which runs the e2e" "waiting for run 41 to decide"
fresh
run_json 1 "41:pull_request:e2e-kind:in_progress"
run_json 2 "41:pull_request:e2e-kind:completed"
printf 'none\nskipped\n' >"$FAKE/q.41"
want dispatch "the run decided before the label landed (skipped): dispatches" "dispatching"
fresh
run_json 1 "41:pull_request:e2e-kind:queued"
want dispatch "a run that never decides: dispatches after WAIT_MAX" "dispatching anyway"
fresh
run_json 1 "60:workflow_dispatch:e2e-kind (0123abc) bisect 7:completed"
echo success >"$FAKE/q.60"
want dispatch "a bisect dispatch that tested another commit does not count" "dispatching"
fresh
run_json 1 "61:workflow_dispatch:e2e-kind:completed"
echo success >"$FAKE/q.61"
want none "a dispatch of the head that passed counts" "already passed"
fresh
run_json 1
echo f00ba7 >"$FAKE/branch"
want none "the branch moved since the label: its own run takes it" "its own run takes the label"

fresh
run_json 1 "61:workflow_dispatch:e2e-kind:completed"
echo failure >"$FAKE/q.61"
want dispatch "an earlier red dispatch is not taken for the new one" "dispatched run 999"
fresh
run_json 1
touch "$FAKE/nodispatch"
out=$(hack/e2e-label.sh 2>&1) && { echo "FAIL: a dispatch run that never appears must fail" >&2; fail=1; }
if [[ $out == *"no e2e-kind dispatch run"* ]]; then
  echo "ok: a dispatch run that never appears fails loudly"
else
  echo "FAIL: missing dispatch message: $out" >&2
  fail=1
fi

[[ $fail -eq 0 ]] && echo "e2e-label tests passed"
exit "$fail"
