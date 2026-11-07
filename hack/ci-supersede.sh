#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Cancel the ci runs of OTHER heads of this pull request (old issue 675). ci.yml groups pull_request runs by
# head SHA with cancel-in-progress off, so two runs on one head (a push, then ready_for_review; a
# re-run) never cancel each other; this script is what still stops the older heads' runs.
#
#   GH_TOKEN=... REPO=o/r PR_NUMBER=N HEAD_SHA=<sha> RUN_ID=<this run> \
#   HEAD_REF=<head branch> hack/ci-supersede.sh
#
# 1. If the pull request's head is no longer HEAD_SHA AND a ci run of that newer head exists (larger
#    run id), this run is stale: cancel this run.
# 2. Cancel every queued or in-progress ci run with a smaller run id and another head SHA that
#    belongs to the pull request: by number (pull_requests, empty for fork PRs) or by head branch
#    (HEAD_REF). A run on this very head is never touched.
#
# Best effort and always exit 0: a failed API call leaves a run going, which costs minutes, not
# correctness. Needs `actions: write` (a fork's read-only token just cannot cancel).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail
export GH_PAGER=cat PAGER=cat
GH=${GH:-gh}
: "${REPO:?}" "${PR_NUMBER:?}" "${HEAD_SHA:?}" "${RUN_ID:?}" "${HEAD_REF:?}"

runs_json() { "$GH" api "repos/$REPO/actions/workflows/ci.yml/runs?event=pull_request&$1&per_page=100" 2>/dev/null || true; }

# A read of the PR head can lag a push (REST is eventually consistent). So a run never trusts it alone:
# it cancels itself only when a ci run of the newer head really exists with a larger run id, and it
# cancels only runs with a smaller run id than its own.
current=$("$GH" api "repos/$REPO/pulls/$PR_NUMBER" --jq .head.sha 2>/dev/null || true)
if [[ -n $current && $current != "$HEAD_SHA" ]]; then
	newer=$(runs_json "head_sha=$current" | jq -r --argjson me "$RUN_ID" --arg cur "$current" \
		'[.workflow_runs[]? | select(.head_sha == $cur and .id > $me)] | length' 2>/dev/null || true)
	if [[ ${newer:-0} -gt 0 ]]; then
		echo "ci-supersede: PR #$PR_NUMBER head is ${current:0:12}, not ${HEAD_SHA:0:12}, and it has a newer ci run: cancelling this stale run"
		"$GH" run cancel "$RUN_ID" -R "$REPO" >/dev/null 2>&1 || echo "ci-supersede: could not cancel run $RUN_ID" >&2
		exit 0
	fi
	echo "ci-supersede: PR #$PR_NUMBER head reads ${current:0:12} but no newer ci run exists (a stale read?): keeping this run"
fi

n=0
for status in queued in_progress; do
	ids=$(runs_json "status=$status" | jq -r --argjson pr "$PR_NUMBER" --arg sha "$HEAD_SHA" --arg ref "$HEAD_REF" --argjson me "$RUN_ID" \
		'.workflow_runs[]? | select((.head_branch == $ref or any(.pull_requests[]?; .number == $pr)) and .head_sha != $sha and .id < $me) | .id' 2>/dev/null || true)
	for id in $ids; do
		[[ $id != "$RUN_ID" ]] || continue
		if "$GH" run cancel "$id" -R "$REPO" >/dev/null 2>&1; then
			echo "ci-supersede: cancelled run $id (an older head of PR #$PR_NUMBER)"
			n=$((n + 1))
		else
			echo "ci-supersede: could not cancel run $id" >&2
		fi
	done
done
echo "ci-supersede: $n older run(s) cancelled"
exit 0
