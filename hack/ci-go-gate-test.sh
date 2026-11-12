#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The required check `go` aggregates the parallel go-* jobs of ci.yml (old issue 217). Fails when a go-*
# job is missing from the gate's needs, or the gate names a job that does not exist.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
wf="$(cd "$(dirname "$0")/.." && pwd)/.github/workflows/ci.yml"

jobs=$(sed -n 's/^  \(go-[a-z0-9-]*\):$/\1/p' "$wf" | sort)
needs=$(sed -n '/^  go:$/,/^  [a-z-]*:$/p' "$wf" | sed -n 's/^    needs: \[\(.*\)\]$/\1/p' | tr ',' '\n' | tr -d ' ' | grep '^go-' | sort || true)

[[ -n $jobs ]] || { echo "FAIL: no go-* jobs in ci.yml"; exit 1; }
if [[ $jobs != "$needs" ]]; then
  echo "FAIL: go-* jobs and the needs of the go gate differ"
  echo "jobs:"; echo "$jobs"; echo "needs:"; echo "$needs"
  exit 1
fi
# shellcheck disable=SC2016 # literal GitHub expression, not a shell expansion
grep -qF '    if: ${{ !cancelled() && !(github.event_name == '"'push'"' && needs.changes.outputs.skip == '"'true'"') }}' <(sed -n '/^  go:$/,/^  [a-z-]*:$/p' "$wf") || { echo "FAIL: go gate must run if: !cancelled() (runs when a part fails, not on a superseded run)"; exit 1; }
# A draft is not tested: the gate must fail on it (old issue 593), or a vacuous green would be the newest run.
# shellcheck disable=SC2016 # literal text of the workflow
# (process substitution, not a pipe: grep -q exits at the first match, and under pipefail sed's SIGPIPE would fail the pipeline)
grep -qF 'SKIP == true && $EVENT == pull_request' <(sed -n '/^  go:$/,/^  [a-z-]*:$/p' "$wf") || { echo "FAIL: go gate must fail on a draft (skip=true on a pull_request)"; exit 1; }
# go-test satisfied by an external run (old issue 610): the job is skipped when `changes` found a green go-test-lab on
# the head, and only then may the gate count that skip. Run the gate's own script with each outcome.
# shellcheck disable=SC2016 # literal text of the workflow
grep -qF "if: needs.changes.outputs.go == 'true' && needs.changes.outputs.go_test_lab != 'true'" <(sed -n '/^  go-test:$/,/^  [a-z-]*:$/p' "$wf") || { echo "FAIL: go-test must be skipped when go_test_lab is true (and only then)"; exit 1; }
grep -q '^      statuses: read$' <(sed -n '/^  changes:$/,/^  [a-z-]*:$/p' "$wf") || { echo "FAIL: the changes job needs statuses: read for go-test-lab"; exit 1; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ci-go-gate-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
# The gate's script: the run block of its only step.
sed -n '/^  go:$/,/^  [a-z-]*:$/p' "$wf" | awk '/^        run: \|$/ { on = 1; next } on && /^  [a-z-]*:$/ { exit } on { sub(/^          /, ""); print }' >"$tmp/gate.sh"
[[ -s $tmp/gate.sh ]] || { echo "FAIL: cannot extract the go gate's script"; exit 1; }
GATE_ENV=() # extra NAME=value pairs for the next gate calls (TIDY, TIDY_RESULT, LINT_RESULT, VULN, VULN_RESULT)
gate() { # gate NAME WANT_RC GO_TEST LAB RESULTS...: runs the extracted script
  local name=$1 want=$2 gt=$3 lab=$4 rc=0
  shift 4
  env CHANGES=success GO=true SKIP=false REASON=x EVENT=pull_request RESULTS="$*" GO_TEST="$gt" LAB="$lab" LAB_REASON=lab \
    TIDY=true TIDY_RESULT=success LINT_RESULT=success VULN=true VULN_RESULT=success "${GATE_ENV[@]}" bash "$tmp/gate.sh" >"$tmp/out" 2>&1 || rc=$?
  [[ $rc == "$want" ]] || { echo "FAIL: $name (rc $rc, want $want)"; cat "$tmp/out"; exit 1; }
}
gate "all green" 0 success false success success success success success success
gate "go-test skipped by an external run is a pass" 0 skipped true success success success skipped success success
gate "go-test skipped without an external run is a failure" 1 skipped false success success success skipped success success
gate "go-test skipped by an external run does not excuse another skipped job" 1 skipped true success skipped success skipped success success
gate "a failed job still fails with an external run" 1 skipped true success failure success skipped success success
gate "lab true but go-test ran and failed" 1 failure true success success success failure success success
# go-tidy and go-lint skipped by the `tidy` output (#572: no Go file, go.mod, go.sum or lint config changed).
GATE_ENV=(TIDY=false TIDY_RESULT=skipped LINT_RESULT=skipped)
gate "tidy and lint skipped because tidy=false is a pass" 0 success false success skipped skipped success success
gate "only go-tidy skipped because tidy=false is a pass" 0 success false success skipped success success success
gate "tidy=false does not excuse a skipped go-test" 1 skipped false success skipped skipped skipped success
gate "tidy=false does not excuse a skipped go-race" 1 success false success skipped skipped success skipped
gate "tidy=false and a failed go-lint fails" 1 success false success skipped failure success success
GATE_ENV=(TIDY=true TIDY_RESULT=skipped LINT_RESULT=success)
gate "go-tidy skipped although tidy=true is a failure" 1 success false success skipped success success success
GATE_ENV=(TIDY=true TIDY_RESULT=success LINT_RESULT=skipped)
gate "go-lint skipped although tidy=true is a failure" 1 success false success success skipped success success
# go-vuln (govulncheck, #579 part 2) skipped by the `vuln` output: go.mod and go.sum did not change.
GATE_ENV=(VULN=false VULN_RESULT=skipped)
gate "go-vuln skipped because vuln=false is a pass" 0 success false success success success success success skipped
gate "vuln=false does not excuse a skipped go-test" 1 skipped false success success success success skipped skipped
gate "vuln=false does not excuse a failed job" 1 success false success success failure success success skipped
GATE_ENV=(VULN=true VULN_RESULT=skipped)
gate "go-vuln skipped although vuln=true is a failure" 1 success false success success success success success skipped
GATE_ENV=(VULN=true VULN_RESULT=failure)
gate "a reachable vulnerability (go-vuln failed) fails the go gate" 1 success false success success success success success failure
GATE_ENV=(VULN=true VULN_RESULT=success)
gate "go-vuln ran and passed" 0 success false success success success success success success
# Only govulncheck's own files changed: go=false, vuln=true. A failed go-vuln must still fail the gate.
GATE_ENV=(GO=false VULN=true VULN_RESULT=failure)
gate "go=false does not hide a failed go-vuln" 1 skipped false success skipped skipped skipped skipped skipped failure
GATE_ENV=(GO=false VULN=true VULN_RESULT=success)
gate "go=false with go-vuln passed is a pass" 0 skipped false success skipped skipped skipped skipped skipped success
# Neither go nor vuln (no Go change, or hack/ci-skip-heavy.sh switched every heavy output off): every go-* job
# is skipped, go-vuln among them, and the gate passes. The skipped go-vuln must stay accepted for this case.
GATE_ENV=(GO=false TIDY=false TIDY_RESULT=skipped LINT_RESULT=skipped VULN=false VULN_RESULT=skipped)
gate "neither go nor vuln is a pass" 0 skipped false success skipped skipped skipped skipped skipped skipped
GATE_ENV=(GO=false TIDY=false TIDY_RESULT=skipped LINT_RESULT=skipped VULN=false VULN_RESULT=failure)
gate "neither go nor vuln still fails a failed go-vuln" 1 skipped false success skipped skipped skipped skipped skipped failure
GATE_ENV=()
grep -qF "if: needs.changes.outputs.vuln == 'true'" <(sed -n '/^  go-vuln:$/,/^  [a-z-]*:$/p' "$wf") || { echo "FAIL: go-vuln must run on the vuln output only"; exit 1; }
grep -qF 'run: hack/govulncheck.sh' <(sed -n '/^  go-vuln:$/,/^  [a-z-]*:$/p' "$wf") || { echo "FAIL: go-vuln must run hack/govulncheck.sh (the pinned version, no retry)"; exit 1; }
grep -qF 'run: hack/govulncheck.sh' "$(dirname "$wf")/govulncheck.yml" || { echo "FAIL: the nightly govulncheck.yml must run the same script"; exit 1; }
! grep -qiE 'retry|attempt|continue-on-error' <(sed -n '/^  go-vuln:$/,/^  [a-z-]*:$/p' "$wf") || { echo "FAIL: go-vuln must not retry or continue on error (rule T1)"; exit 1; }
grep -qF "if: needs.changes.outputs.tidy == 'true'" <(sed -n '/^  go-tidy:$/,/^  [a-z-]*:$/p' "$wf") || { echo "FAIL: go-tidy must run on the tidy output"; exit 1; }
grep -qF "if: needs.changes.outputs.tidy == 'true'" <(sed -n '/^  go-lint:$/,/^  [a-z-]*:$/p' "$wf") || { echo "FAIL: go-lint must run on the tidy output"; exit 1; }
echo "ok: go gate covers $(echo "$jobs" | tr '\n' ' ')"
