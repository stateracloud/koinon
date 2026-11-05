#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/e2e-decide.sh with a stub rule script and a stub gh.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d "${TMPDIR:-/tmp}/e2e-decide-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# Stub rule: STUB_FORCED / STUB_FULL are the exit codes for --forced-only and the full rule.
cat >"$tmp/needs" <<'SH'
#!/usr/bin/env bash
if [[ $1 == --forced-only ]]; then echo "forced stub"; exit "${STUB_FORCED:-1}"; fi
echo "full stub $*"; exit "${STUB_FULL:-1}"
SH
# Stub gh: STUB_LABELS answers the PR's current labels; anything else STUB_COVERED.
cat >"$tmp/gh" <<'SH'
#!/usr/bin/env bash
case "$*" in
*/issues/*/labels*) echo "${STUB_LABELS:-[]}" ;;
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
# req <true|false> <description> [VAR=value...]: the `required=` line (what e2e result enforces, koinon-private#562).
req() {
  local expect=$1 desc=$2 got
  shift 2
  export GITHUB_STEP_SUMMARY="$tmp/summary"
  : >"$GITHUB_STEP_SUMMARY"
  got=$(env "$@" hack/e2e-decide.sh | sed -n 's/^required=//p' | tail -1)
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
# koinon-private#468: `gh pr create --label e2e` fires `opened` without the label, then `labeled`.
PRV=(PR=524 GH_REPO=o/r)
want true "opened: the label added after the event counts" EVENT=pull_request ACTION=opened "${PRV[@]}" 'STUB_LABELS=["e2e"]'
want false "opened: no label now either" EVENT=pull_request ACTION=opened "${PRV[@]}" 'STUB_LABELS=["bug"]'
want true "dispatch always runs" EVENT=workflow_dispatch
want true "push with no green run to compare with runs" EVENT=push
want true "push with e2e-relevant changes since the last green run" EVENT=push STUB_COVERED=abc123 STUB_FULL=0
want false "push without them does not" EVENT=push STUB_COVERED=abc123 STUB_FULL=1
want true "push: a rule failure fails closed" EVENT=push STUB_COVERED=abc123 STUB_FULL=2
want true "nightly always runs (drift detection)" EVENT=schedule STUB_COVERED=abc123 STUB_FULL=1

# required: the pull request needs an e2e, whether or not it runs here.
req true "PR touching the e2e's files requires one" EVENT=pull_request STUB_FORCED=0
req true "PR with the label requires one" EVENT=pull_request 'LABELS=["e2e"]'
req true "e2e-relevant PR without the label requires one (not run here)" EVENT=pull_request STUB_FULL=0
req true "PR: a rule failure requires one" EVENT=pull_request STUB_FORCED=2
req true "PR: a full-rule failure requires one" EVENT=pull_request STUB_FULL=2
req false "PR with nothing relevant requires none" EVENT=pull_request
req false "dispatch requires nothing of itself" EVENT=workflow_dispatch
req false "push requires nothing" EVENT=push STUB_COVERED=abc123 STUB_FULL=0
req false "nightly requires nothing" EVENT=schedule
want false "relevant unlabelled PR still does not run here" EVENT=pull_request STUB_FULL=0
summary_has "e2e result"
summary_has "gh workflow run e2e-kind.yml"

[[ $fail -eq 0 ]] && echo "e2e-decide tests passed"
exit "$fail"
