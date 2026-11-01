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
cat >"$tmp/gh" <<'SH'
#!/usr/bin/env bash
echo "${STUB_COVERED:-}"
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
want true "dispatch always runs" EVENT=workflow_dispatch
want true "push with no green run to compare with runs" EVENT=push
want true "push with e2e-relevant changes since the last green run" EVENT=push STUB_COVERED=abc123 STUB_FULL=0
want false "push without them does not" EVENT=push STUB_COVERED=abc123 STUB_FULL=1
want true "push: a rule failure fails closed" EVENT=push STUB_COVERED=abc123 STUB_FULL=2
want true "nightly always runs (drift detection)" EVENT=schedule STUB_COVERED=abc123 STUB_FULL=1

[[ $fail -eq 0 ]] && echo "e2e-decide tests passed"
exit "$fail"
