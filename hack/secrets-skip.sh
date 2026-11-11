#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Decides whether the `gitleaks` job of secrets.yml skips its scan (#572). Prints skip=true|false and
# reason=<text> (lines for $GITHUB_OUTPUT). The job still runs and reports success, so the required
# `gitleaks` check reports on every pull request, push and merge_group build.
#
# The rule is the one hack/ci-skip-heavy.sh uses for ci.yml (ADR 0094 amendment): a push to main is
# skipped only when a successful merge_group run of this workflow (the queue build) scanned a commit
# whose tree equals the tree of the pushed commit. The queue run scanned MG_BASE..MG_HEAD, the same
# commits the push brings. A pull request, a merge_group build, an admin or break-glass merge (no queue
# run on that tree) and a direct push scan as before. Any doubt (an API error, no queue run, an unknown
# event) is skip=false.
#
# Env: EVENT_NAME, REF, SHA, REPO, GH_TOKEN (gh). Read rights only (actions, contents).
# Needs bash 5, gh, jq (gh --jq).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

emit() { echo "skip=$1"; echo "reason=$2"; }

decide() {
	local tree head
	if [[ ${EVENT_NAME:-} != push ]]; then
		emit false "event '${EVENT_NAME:-}': scanning"
		return
	fi
	if [[ ${REF:-} != refs/heads/main || -z ${SHA:-} || -z ${REPO:-} ]]; then
		emit false "not a push to main: scanning"
		return
	fi
	if ! tree=$(gh api "repos/$REPO/git/commits/$SHA" --jq .tree.sha 2>/dev/null) || [[ -z $tree ]]; then
		emit false "cannot read the tree of $SHA: scanning"
		return
	fi
	if ! head=$(gh api "repos/$REPO/actions/workflows/secrets.yml/runs?event=merge_group&status=success&per_page=30" \
		--jq ".workflow_runs[] | select(.conclusion == \"success\" and .head_commit.tree_id == \"$tree\") | .head_sha" 2>/dev/null); then
		emit false "cannot list the merge_group runs of secrets: scanning"
		return
	fi
	head=${head%%$'\n'*}
	if [[ -z $head ]]; then
		emit false "no successful merge_group secrets run on the tree of $SHA (an admin merge, a direct push, or a run older than the last 30): scanning"
		return
	fi
	emit true "a successful merge_group secrets run (commit $head) scanned the tree of $SHA"
}

case ${1:-} in
decide) decide ;;
*)
	echo "usage: hack/secrets-skip.sh decide" >&2
	exit 2
	;;
esac
