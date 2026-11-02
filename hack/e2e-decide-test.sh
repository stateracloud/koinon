#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/e2e-decide.sh with a stub rule script and a stub gh.
set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Stub rule: STUB_FORCED / STUB_FULL are the exit codes for --forced-only and the full rule.
cat >"$tmp/needs" <<'SH'
#!/usr/bin/env bash
if [[ $1 == --forced-only ]]; then echo "forced stub"; exit "${STUB_FORCED:-1}"; fi
echo "full stub $*"; exit "${STUB_FULL:-1}"
SH
# Stub gh: STUB_LABELS answers the PR's current labels, STUB_RUNS the other runs on the head (ids, one
# per line), STUB_RAN how many quickstart jobs of such a run ran; anything else STUB_COVERED.
cat >"$tmp/gh" <<'SH'
#!/usr/bin/env bash
case "$*" in
*/issues/*/labels*) echo "${STUB_LABELS:-[]}" ;;
*/actions/workflows/e2e-kind.yml/runs*) [[ -z ${STUB_RUNS:-} ]] || printf '%s\n' $STUB_RUNS ;;
*/actions/runs/*/jobs*) echo "${STUB_RAN:-0}" ;;
*) echo "${STUB_COVERED:-}" ;;
esac
SH
chmod +x "$tmp/needs" "$tmp/gh"
export NEEDS_E2E="$tmp/needs" GH="$tmp/gh" BASE=main

fail=0
# want <true|false> <description> [VAR=value...]: runs the decision with those variables.
want() {
  local expect=$1 desc=$2 got
  shift 2
  export GITHUB_STEP_SUMMARY="$tmp/summary"
  : >"$GITHUB_STEP_SUMMARY"
  got=$(env "$@" hack/e2e-decide.sh | sed -n 's/^needed=//p' | tail -1)
  if [[ $got == "$expect" ]]; then echo "ok: $desc"; else echo "FAIL: $desc (got '$got', want $expect)" >&2; fail=1; fi
}
summary_has() { grep -q -- "$1" "$tmp/summary" || { echo "FAIL: summary lacks '$1'" >&2; fail=1; }; }

want true "PR touching the e2e's files runs" EVENT=pull_request STUB_FORCED=0
want true "PR with the e2e label runs" EVENT=pull_request 'LABELS=["bug","e2e"]'
want false "PR with other labels and nothing relevant does not run" EVENT=pull_request 'LABELS=["bug"]'
want false "PR that is e2e-relevant but unlabelled does not run" EVENT=pull_request STUB_FULL=0
summary_has "hack/e2e-remote.sh"
summary_has "e2e\` label"
want true "PR: a rule failure fails closed" EVENT=pull_request STUB_FORCED=2
want false "PR: nothing relevant says so" EVENT=pull_request
summary_has "nothing in this pull request"
# #468: `gh pr create --label e2e` fires `opened` without the label, then `labeled`.
PRV=(PR=524 GH_REPO=o/r HEAD_SHA=abc RUN_ID=2)
want true "opened: the label added after the event counts" EVENT=pull_request ACTION=opened "${PRV[@]}" 'STUB_LABELS=["e2e"]'
want false "opened: no label now either" EVENT=pull_request ACTION=opened "${PRV[@]}" 'STUB_LABELS=["bug"]'
want false "labeled e2e after a run that ran the e2e on the head: covered" EVENT=pull_request ACTION=labeled "${PRV[@]}" \
  'LABELS=["e2e"]' STUB_RUNS=1 STUB_RAN=1
summary_has "already ran the e2e"
covered=$(env EVENT=pull_request ACTION=labeled "${PRV[@]}" 'LABELS=["e2e"]' STUB_RUNS="7 9" STUB_RAN=1 hack/e2e-decide.sh | sed -n 's/^covered=//p')
[[ $covered == 7 ]] || { echo "FAIL: covered=<the covering run's id> is printed (got '$covered')" >&2; fail=1; }
want true "labeled e2e after a run that skipped the e2e: runs" EVENT=pull_request ACTION=labeled "${PRV[@]}" \
  'LABELS=["e2e"]' STUB_RUNS=1 STUB_RAN=0
want true "labeled e2e with no other run on the head: runs" EVENT=pull_request ACTION=labeled "${PRV[@]}" 'LABELS=["e2e"]'
want true "synchronize is never covered" EVENT=pull_request ACTION=synchronize "${PRV[@]}" 'LABELS=["e2e"]' STUB_RUNS=1 STUB_RAN=1
want true "dispatch always runs" EVENT=workflow_dispatch
want true "push with no green run to compare with runs" EVENT=push
want true "push with e2e-relevant changes since the last green run" EVENT=push STUB_COVERED=abc123 STUB_FULL=0
want false "push without them does not" EVENT=push STUB_COVERED=abc123 STUB_FULL=1
want true "push: a rule failure fails closed" EVENT=push STUB_COVERED=abc123 STUB_FULL=2
want true "nightly always runs (drift detection)" EVENT=schedule STUB_COVERED=abc123 STUB_FULL=1

[[ $fail -eq 0 ]] && echo "e2e-decide tests passed"
exit "$fail"
