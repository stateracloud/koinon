#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline test of hack/ci-supersede.sh (old issue 675). gh is a stub: the run list and the PR head come from
# env vars, every cancel is logged.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -uo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ci-supersede-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
export FAKE_LOG=$tmp/log

cat >"$tmp/gh" <<'SH'
#!/usr/bin/env bash
echo "gh $*" >>"$FAKE_LOG"
case "$2" in
repos/o/r/pulls/7) echo "$FAKE_PR_HEAD" ;;
repos/o/r/actions/workflows/ci.yml/runs*)
	if [[ $2 == *head_sha=* ]]; then
		sha=${2#*head_sha=}; sha=${sha%%&*}
		jq -c --arg sha "$sha" '{workflow_runs: ([.[]] | add | map(select(.head_sha == $sha)))}' <<<"$FAKE_RUNS"
	else
		status=${2#*status=}; status=${status%%&*}
		jq -c "{workflow_runs: .$status}" <<<"$FAKE_RUNS"
	fi
	;;
esac
exit 0
SH
chmod +x "$tmp/gh"

pass=0 fail=0
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then echo "ok: $d"; pass=$((pass + 1)); else echo "FAIL: $d" >&2; fail=$((fail + 1)); fi; }
# shellcheck disable=SC2329 # called through check
has() { grep -q -- "$1" "$2"; }
# shellcheck disable=SC2329
hasnt() { ! grep -q -- "$1" "$2"; }

# shellcheck disable=SC2329
eq() { [[ $1 == "$2" ]]; }
# The stub serves FAKE_RUNS keyed by status: {queued: [...], in_progress: [...]}
run() { GH=$tmp/gh REPO=o/r PR_NUMBER=7 HEAD_SHA=new HEAD_REF=feat/x RUN_ID=${RUN_ID:-900} GH_TOKEN=x "$repo/hack/ci-supersede.sh"; }
export FAKE_PR_HEAD=new
export FAKE_RUNS='{"queued":[{"id":1,"head_sha":"old","pull_requests":[{"number":7}]},{"id":2,"head_sha":"new","pull_requests":[{"number":7}]}],"in_progress":[{"id":3,"head_sha":"old","pull_requests":[{"number":7}]},{"id":4,"head_sha":"old","pull_requests":[{"number":8}]},{"id":900,"head_sha":"new","pull_requests":[{"number":7}]}]}'
: >"$FAKE_LOG"
run >"$tmp/o1" 2>&1
check "older head queued run cancelled" has 'gh run cancel 1 ' "$FAKE_LOG"
check "older head running run cancelled" has 'gh run cancel 3 ' "$FAKE_LOG"
check "a run on the same head is never cancelled" hasnt 'gh run cancel 2 ' "$FAKE_LOG"
check "another PR's run is never cancelled" hasnt 'gh run cancel 4 ' "$FAKE_LOG"
check "this run is not cancelled" hasnt 'gh run cancel 900' "$FAKE_LOG"

# the PR moved on: this run is stale and cancels itself, nobody else
: >"$FAKE_LOG"
FAKE_PR_HEAD=newer FAKE_RUNS='{"queued":[{"id":1,"head_sha":"old","pull_requests":[{"number":7}]},{"id":950,"head_sha":"newer","pull_requests":[{"number":7}]}],"in_progress":[]}' run >"$tmp/o2" 2>&1
check "stale run cancels itself" has 'gh run cancel 900 ' "$FAKE_LOG"
check "stale run cancels nobody else" eq "$(grep -c 'gh run cancel' "$FAKE_LOG")" 1

# a stale read of the PR head (an older head) must not make the newest run cancel itself: no ci run
# of that head with a larger id exists
: >"$FAKE_LOG"
FAKE_PR_HEAD=old run >"$tmp/o2b" 2>&1
check "stale head read: the newest run does not cancel itself" hasnt 'gh run cancel 900' "$FAKE_LOG"
check "stale head read: older runs are still cancelled" has 'gh run cancel 1 ' "$FAKE_LOG"

# the newer head exists but its run is not newer than this one (a read of an older head): keep
: >"$FAKE_LOG"
FAKE_PR_HEAD=newer FAKE_RUNS='{"queued":[],"in_progress":[{"id":800,"head_sha":"newer","pull_requests":[{"number":7}]}]}' run >"$tmp/o2c" 2>&1
check "newer head with a smaller run id: no self-cancel" hasnt 'gh run cancel 900' "$FAKE_LOG"

# the newer head has a run with a larger id: this run is stale
: >"$FAKE_LOG"
FAKE_PR_HEAD=newer FAKE_RUNS='{"queued":[{"id":950,"head_sha":"newer","pull_requests":[{"number":7}]}],"in_progress":[]}' run >"$tmp/o2d" 2>&1
check "newer head with a larger run id: self-cancel" has 'gh run cancel 900 ' "$FAKE_LOG"

# a run never cancels a run with a larger id, even on another head of the PR
: >"$FAKE_LOG"
FAKE_RUNS='{"queued":[{"id":950,"head_sha":"newer","pull_requests":[{"number":7}]}],"in_progress":[]}' run >"$tmp/o2e" 2>&1
check "a larger run id is never cancelled" hasnt 'gh run cancel 950' "$FAKE_LOG"

# fork PR: pull_requests is empty on the old head's run; matched by head branch
: >"$FAKE_LOG"
FAKE_RUNS='{"queued":[{"id":5,"head_sha":"old","head_branch":"feat/x","pull_requests":[]},{"id":6,"head_sha":"old","head_branch":"other","pull_requests":[]}],"in_progress":[]}' run >"$tmp/o2f" 2>&1
check "empty pull_requests: matched by head branch" has 'gh run cancel 5 ' "$FAKE_LOG"
check "empty pull_requests: another branch untouched" hasnt 'gh run cancel 6 ' "$FAKE_LOG"

# an API failure for the PR head leaves everything running and exits 0
: >"$FAKE_LOG"
FAKE_PR_HEAD="" FAKE_RUNS='{"queued":[],"in_progress":[]}' run >"$tmp/o3" 2>&1
rc=$?
check "nothing to cancel: exit 0" eq "$rc" 0

echo "$pass passed, $fail failed"
exit $((fail > 0))
