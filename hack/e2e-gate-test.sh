#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/e2e-gate.sh (the `e2e result` decision) and hack/e2e-rerun-result.sh with a stub gh.
# The stub answers from JSON files in $tmp (runs, jobs-<id>, lab, failed) through the caller's --jq
# filter, so the filters are exercised. Calls are recorded in $tmp/calls.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d "${TMPDIR:-/tmp}/e2e-gate-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/gh" <<'SH'
#!/usr/bin/env bash
# Answers from JSON files in $T, applying the caller's --jq filter like gh does.
echo "$*" >>"$T/calls"
jqf=""
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do [[ ${args[i]} == --jq ]] && jqf=${args[i + 1]}; done
ans() { [[ -f $1 ]] || exit 0; if [[ -n $jqf ]]; then jq -r "$jqf" "$1"; else cat "$1"; fi; }
case "$*" in
"run list"*"--event pull_request"*) ans "$T/failed.json" ;;
"run list"*) ans "$T/runs.json" ;;
"api repos/o/r/actions/runs/"*"/jobs"*)
  id=${2#repos/o/r/actions/runs/}; id=${id%%/*}
  ans "$T/jobs-$id.json" ;;
"api repos/o/r/commits/"*"/statuses"*)
  [[ ! -f $T/lab.err ]] || { cat "$T/lab.err" >&2; exit 1; }
  ans "$T/lab.json" ;;
"run rerun"*) [[ -z ${T_RERUN_FAILS:-} ]] ;;
*) exit 9 ;;
esac
SH
chmod +x "$tmp/gh"
export T="$tmp" GH="$tmp/gh" GH_REPO=o/r SHA=0123456789abcdef0123456789abcdef01234567 BRANCH=feat/x GITHUB_RUN_ID=100
unset GITHUB_STEP_SUMMARY

fail=0
reset() { rm -f "$tmp"/*.json "$tmp"/calls; }
# runs <id> <event> <displayTitle>...: the runs the head has (one per triple).
runs() { local j="[" sep=""; while (($#)); do j+="$sep{\"databaseId\":$1,\"event\":\"$2\",\"displayTitle\":\"$3\"}"; sep=","; shift 3; done; echo "$j]" >"$tmp/runs.json"; }
# job <run id> <quickstart conclusion> <go-suite marker step conclusion>
job() { echo "{\"jobs\":[{\"id\":7,\"name\":\"quickstart\",\"conclusion\":\"$2\",\"steps\":[{\"name\":\"Go suite (marker for the e2e gate)\",\"conclusion\":\"$3\"}]},{\"id\":8,\"name\":\"e2e result\",\"conclusion\":\"failure\",\"steps\":[]}]}" >"$tmp/jobs-$1.json"; }
# gate <want rc> <description> [VAR=value...]
gate() {
  local want=$1 desc=$2 rc=0 out
  shift 2
  out=$(env "$@" hack/e2e-gate.sh 2>&1) || rc=$?
  if [[ $rc -eq $want ]]; then echo "ok: $desc"; else echo "FAIL: $desc (rc $rc, want $want)" >&2; echo "$out" >&2; fail=1; fi
  gate_out=$out
}
has() { grep -q -- "$1" <<<"$gate_out" || { echo "FAIL: output lacks '$1'" >&2; fail=1; }; }
G=(DECIDE=success QUICKSTART=skipped REQUIRED=true)

reset
gate 0 "not required, quickstart skipped" DECIDE=success QUICKSTART=skipped REQUIRED=false
gate 0 "draft: decide skipped, nothing required" DECIDE=skipped QUICKSTART=skipped REQUIRED=
gate 1 "not required, quickstart failed" DECIDE=success QUICKSTART=failure REQUIRED=false
gate 1 "decide cancelled" DECIDE=cancelled QUICKSTART=skipped REQUIRED=false
gate 0 "required, this run's quickstart passed" DECIDE=success QUICKSTART=success REQUIRED=true
gate 1 "required, nothing on the head" "${G[@]}"
has "post the commit status e2e-lab"
has "gh workflow run e2e-kind.yml --ref feat/x"
has "Re-run failed jobs"

runs 101 workflow_dispatch e2e-kind
job 101 success success
gate 0 "required, a dispatched go-suite run's quickstart job passed (the run may still be in progress)" "${G[@]}"
job 101 failure success
gate 1 "required, the dispatched quickstart failed" "${G[@]}"
job 101 skipped skipped
gate 1 "required, a run whose quickstart was skipped is not evidence" "${G[@]}"
job 101 success skipped
gate 1 "required, a suite without the go marker (web, or a future one) is not evidence" "${G[@]}"
runs 101 workflow_dispatch "e2e-kind (0123456789abcdef0123456789abcdef01234567) bisect 9"
job 101 success success
gate 1 "required, a bisect dispatch of another commit is not evidence" "${G[@]}"
runs 101 push e2e-kind
gate 1 "required, a push run is not evidence on a PR head" "${G[@]}"
runs 101 pull_request e2e-kind
gate 0 "required, another pull_request run with a green quickstart (label) is evidence" "${G[@]}"
runs 100 workflow_dispatch e2e-kind
job 100 success success
gate 1 "required, this run itself is not looked up as another run" "${G[@]}"

reset
echo '[{"context":"e2e-lab","state":"success"}]' >"$tmp/lab.json"
gate 0 "required, e2e-lab is success" "${G[@]}"
echo '[{"context":"e2e-lab","state":"failure"},{"context":"e2e-lab","state":"success"}]' >"$tmp/lab.json"
gate 1 "required, the newest e2e-lab is failure" "${G[@]}"
has "e2e-lab is failure"
echo '[{"context":"e2e-lab","state":"pending"}]' >"$tmp/lab.json"
gate 1 "required, e2e-lab is pending" "${G[@]}"
echo '[{"context":"e2e-lab","state":"success"}]' >"$tmp/lab.json"
gate 0 "required, this run quickstart failed but e2e-lab is success (the external run is an accelerator)" DECIDE=success QUICKSTART=failure REQUIRED=true
gate 0 "required, this run quickstart cancelled but e2e-lab is success" DECIDE=success QUICKSTART=cancelled REQUIRED=true
gate 1 "required, no head sha is read as no evidence" "${G[@]}" SHA=
rm -f "$tmp/lab.json"
gate 1 "required, quickstart failed and no other evidence" DECIDE=success QUICKSTART=failure REQUIRED=true
echo '[{"context":"e2e-lab","state":"success"}]' >"$tmp/lab.json"

rm -f "$tmp/lab.json"
echo "gh: Resource not accessible by integration (HTTP 403)" >"$tmp/lab.err"
gate 1 "required, an API error reading e2e-lab fails closed" "${G[@]}"
has "could not read e2e-lab: gh: Resource not accessible by integration (HTTP 403)"
has "statuses: read"
rm -f "$tmp/lab.err"
echo 'not json' >"$tmp/lab.json"
gate 1 "required, an invalid e2e-lab response fails closed" "${G[@]}"
has "could not read e2e-lab: invalid JSON"
echo '[{"context":"e2e-lab","state":"success"}]' >"$tmp/lab.json"

wf=.github/workflows/e2e-kind.yml
# The result job can read commit statuses (without it e2e-lab is a 403, koinon-private#588).
awk '/^  result:/{f=1;next} f&&/^  [a-z-]+:/{f=0} f' "$wf" | grep -qxF '      statuses: read # hack/e2e-gate.sh reads the e2e-lab commit status; without it the API answers 403 (koinon-private#588)' || { echo "FAIL: the result job lacks statuses: read" >&2; fail=1; }
echo ok: result job statuses pinned

# The YAML and the gate agree on the marker step: its exact name, and the `if` that makes the default
# (no inputs, as on pull_request) count as the go suite.
grep -qxF '      - name: Go suite (marker for the e2e gate)' "$wf" || { echo "FAIL: $wf lacks the marker step name" >&2; fail=1; }
grep -qF '"Go suite (marker for the e2e gate)"' hack/e2e-gate.sh || { echo "FAIL: e2e-gate.sh does not read the marker step name" >&2; fail=1; }
grep -A1 -xF '      - name: Go suite (marker for the e2e gate)' "$wf" | grep -qxF "        if: (inputs.suite || 'go') == 'go'" || { echo "FAIL: the marker step's if is not the go-suite default" >&2; fail=1; }
echo ok: marker step pinned

# hack/e2e-rerun-result.sh
reset
rc=0
hack/e2e-rerun-result.sh "$SHA" >/dev/null || rc=$?
if [[ $rc -eq 0 ]] && ! grep -q '^run rerun' "$tmp/calls"; then echo "ok: rerun: nothing failed, nothing re-run"; else echo "FAIL: rerun without a failed run" >&2; fail=1; fi
echo '[{"databaseId":55,"status":"completed","conclusion":"failure","headBranch":"feat/x"}]' >"$tmp/failed.json"
job 55 failure success
hack/e2e-rerun-result.sh "$SHA" >/dev/null
if grep -q '^run rerun 55 --job 8$' "$tmp/calls" && ! grep -q -- '--failed' "$tmp/calls"; then echo "ok: rerun: only the e2e result job is re-run"; else echo "FAIL: rerun did not re-run job 8 of run 55 alone" >&2; fail=1; fi
echo '[{"databaseId":55,"status":"in_progress","conclusion":null}]' >"$tmp/failed.json"
: >"$tmp/calls"
hack/e2e-rerun-result.sh "$SHA" >/dev/null
if ! grep -q '^run rerun' "$tmp/calls"; then echo "ok: rerun: a run still in progress is left alone"; else echo "FAIL: rerun touched an in-progress run" >&2; fail=1; fi
echo '[{"databaseId":55,"status":"completed","conclusion":"failure","headBranch":"feat/x"}]' >"$tmp/failed.json"
rc=0
T_RERUN_FAILS=1 hack/e2e-rerun-result.sh "$SHA" >/dev/null 2>&1 || rc=$?
if [[ $rc -eq 0 ]]; then echo "ok: rerun: a gh failure is not an error"; else echo "FAIL: rerun failed hard (rc $rc)" >&2; fail=1; fi
# no head branch: the live-run check cannot run, so nothing is re-run and no branchless run list is made
echo '[{"databaseId":55,"status":"completed","conclusion":"failure","headBranch":""}]' >"$tmp/failed.json"
: >"$tmp/calls"
rc=0
out=$(hack/e2e-rerun-result.sh "$SHA" 2>&1) || rc=$?
if [[ $rc -eq 0 ]] && ! grep -q '^run rerun' "$tmp/calls" && ! grep -q -- '--branch' "$tmp/calls" && grep -q 'no head branch' <<<"$out"; then echo "ok: rerun: an empty head branch skips the re-run"; else echo "FAIL: rerun with an empty head branch" >&2; fail=1; fi
rc=0
hack/e2e-rerun-result.sh not-a-sha >/dev/null 2>&1 || rc=$?
if [[ $rc -eq 64 ]]; then echo "ok: rerun: usage"; else echo "FAIL: rerun usage rc $rc" >&2; fail=1; fi

[[ $fail -eq 0 ]] && echo "e2e-gate tests passed"
exit "$fail"
