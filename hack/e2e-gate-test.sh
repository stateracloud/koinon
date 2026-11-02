#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/e2e-gate.sh (the `e2e result` decision) and hack/e2e-rerun-result.sh with a stub gh.
# Stub answers (files in $tmp): runs = ids of successful e2e-kind runs on the head, one per line;
# jobs-<id> = the `quickstart` job ids of that run that succeeded; lab = the newest e2e-lab state;
# failed = id of the failed pull request run. Calls are recorded in $tmp/calls.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d "${TMPDIR:-/tmp}/e2e-gate-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/gh" <<'SH'
#!/usr/bin/env bash
echo "$*" >>"$T/calls"
case "$*" in
"run list"*"--status success"*) cat "$T/runs" 2>/dev/null || true ;;
"run list"*"--event pull_request"*) cat "$T/failed" 2>/dev/null || true ;;
"api repos/o/r/actions/runs/"*"/jobs"*)
  id=${2#repos/o/r/actions/runs/}; id=${id%%/*}
  cat "$T/jobs-$id" 2>/dev/null || true ;;
"api repos/o/r/commits/"*"/statuses"*) cat "$T/lab" 2>/dev/null || true ;;
"run rerun"*) [[ -z ${T_RERUN_FAILS:-} ]] ;;
*) exit 9 ;;
esac
SH
chmod +x "$tmp/gh"
export T="$tmp" GH="$tmp/gh" GH_REPO=o/r SHA=0123456789abcdef0123456789abcdef01234567 BRANCH=feat/x GITHUB_RUN_ID=100
unset GITHUB_STEP_SUMMARY

fail=0
reset() { rm -f "$tmp"/runs "$tmp"/jobs-* "$tmp"/lab "$tmp"/failed "$tmp"/calls; }
# gate <want rc> <description> [VAR=value...]
gate() {
  local want=$1 desc=$2 rc=0 out
  shift 2
  out=$(env "$@" hack/e2e-gate.sh 2>&1) || rc=$?
  if [[ $rc -eq $want ]]; then echo "ok: $desc"; else echo "FAIL: $desc (rc $rc, want $want)" >&2; echo "$out" >&2; fail=1; fi
  gate_out=$out
}
has() { grep -q -- "$1" <<<"$gate_out" || { echo "FAIL: output lacks '$1'" >&2; fail=1; }; }

reset
gate 0 "not required, quickstart skipped" DECIDE=success QUICKSTART=skipped REQUIRED=false
gate 0 "draft: decide skipped, nothing required" DECIDE=skipped QUICKSTART=skipped REQUIRED=
gate 1 "not required, quickstart failed" DECIDE=success QUICKSTART=failure REQUIRED=false
gate 1 "decide cancelled" DECIDE=cancelled QUICKSTART=skipped REQUIRED=false
gate 0 "required, this run's quickstart passed" DECIDE=success QUICKSTART=success REQUIRED=true
gate 1 "required, nothing on the head" DECIDE=success QUICKSTART=skipped REQUIRED=true
has "hack/e2e-remote.sh --argo"
has "gh workflow run e2e-kind.yml --ref feat/x"
has "Re-run failed jobs"

echo 101 >"$tmp/runs"
echo 7 >"$tmp/jobs-101"
gate 0 "required, a dispatched run's quickstart passed" DECIDE=success QUICKSTART=skipped REQUIRED=true
rm "$tmp/jobs-101"
gate 1 "required, a green run whose quickstart was skipped is not evidence" DECIDE=success QUICKSTART=skipped REQUIRED=true
echo 7 >"$tmp/jobs-100"
printf '100\n' >"$tmp/runs"
gate 1 "required, this run itself is not looked up as another run" DECIDE=success QUICKSTART=skipped REQUIRED=true

reset
echo success >"$tmp/lab"
gate 0 "required, e2e-lab is success" DECIDE=success QUICKSTART=skipped REQUIRED=true
echo failure >"$tmp/lab"
gate 1 "required, e2e-lab is failure" DECIDE=success QUICKSTART=skipped REQUIRED=true
has "e2e-lab is failure"
echo pending >"$tmp/lab"
gate 1 "required, e2e-lab is pending" DECIDE=success QUICKSTART=skipped REQUIRED=true
gate 1 "required and quickstart failed, a lab success does not hide it" DECIDE=success QUICKSTART=failure REQUIRED=true
gate 1 "required, no head sha is read as no evidence" DECIDE=success QUICKSTART=skipped REQUIRED=true SHA=

# hack/e2e-rerun-result.sh
reset
rc=0
hack/e2e-rerun-result.sh "$SHA" >/dev/null || rc=$?
if [[ $rc -eq 0 ]] && ! grep -q '^run rerun' "$tmp/calls"; then echo "ok: rerun: nothing failed, nothing re-run"; else echo "FAIL: rerun without a failed run" >&2; fail=1; fi
echo 55 >"$tmp/failed"
hack/e2e-rerun-result.sh "$SHA" >/dev/null
if grep -q '^run rerun 55 --failed' "$tmp/calls"; then echo "ok: rerun: the failed pull request run is re-run"; else echo "FAIL: rerun did not re-run run 55" >&2; fail=1; fi
rc=0
T_RERUN_FAILS=1 hack/e2e-rerun-result.sh "$SHA" >/dev/null 2>&1 || rc=$?
if [[ $rc -eq 0 ]]; then echo "ok: rerun: a gh failure is not an error"; else echo "FAIL: rerun failed hard (rc $rc)" >&2; fail=1; fi
rc=0
hack/e2e-rerun-result.sh not-a-sha >/dev/null 2>&1 || rc=$?
if [[ $rc -eq 64 ]]; then echo "ok: rerun: usage"; else echo "FAIL: rerun usage rc $rc" >&2; fail=1; fi

[[ $fail -eq 0 ]] && echo "e2e-gate tests passed"
exit "$fail"
