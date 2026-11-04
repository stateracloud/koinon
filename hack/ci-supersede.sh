#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Cancel the ci runs of OTHER heads of this pull request (#675). ci.yml groups pull_request runs by
# head SHA with cancel-in-progress off, so two runs on one head (a push, then ready_for_review; a
# re-run) never cancel each other; this script is what still stops the older heads' runs.
#
#   GH_TOKEN=... REPO=o/r PR_NUMBER=N HEAD_SHA=<sha> RUN_ID=<this run> hack/ci-supersede.sh
#
# 1. If the pull request's head is no longer HEAD_SHA, this run is stale (a newer push already
#    exists): cancel this run.
# 2. Otherwise cancel every queued or in-progress ci run of the same pull request whose head is
#    another SHA. A run on this very head is never touched.
#
# Best effort and always exit 0: a failed API call leaves a run going, which costs minutes, not
# correctness. Needs `actions: write` (a fork's read-only token just cannot cancel).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail
export GH_PAGER=cat PAGER=cat
GH=${GH:-gh}
: "${REPO:?}" "${PR_NUMBER:?}" "${HEAD_SHA:?}" "${RUN_ID:?}"

current=$("$GH" api "repos/$REPO/pulls/$PR_NUMBER" --jq .head.sha 2>/dev/null || true)
if [[ -n $current && $current != "$HEAD_SHA" ]]; then
	echo "ci-supersede: PR #$PR_NUMBER head is ${current:0:12}, not ${HEAD_SHA:0:12}: cancelling this stale run"
	"$GH" run cancel "$RUN_ID" -R "$REPO" >/dev/null 2>&1 || echo "ci-supersede: could not cancel run $RUN_ID" >&2
	exit 0
fi

n=0
for status in queued in_progress; do
	ids=$("$GH" api "repos/$REPO/actions/workflows/ci.yml/runs?event=pull_request&status=$status&per_page=100" \
		--jq ".workflow_runs[] | select(any(.pull_requests[]?; .number == $PR_NUMBER) and .head_sha != \"$HEAD_SHA\") | .id" 2>/dev/null || true)
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
