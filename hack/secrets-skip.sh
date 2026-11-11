#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Decides whether the `gitleaks` job of secrets.yml skips its scan (#572). Prints skip=true|false and
# reason=<text> (lines for $GITHUB_OUTPUT). The job still runs and reports success, so the required
# `gitleaks` check reports on every pull request, push and merge_group build.
#
# The rule is keyed on the commit, not the tree (gitleaks scans commits, so equal trees can hide other
# commits, for example "add secret" then "revert" pushed by an admin): a push to main is skipped only when
# a successful merge_group run of this workflow (the queue build) has head_sha equal to the pushed SHA.
# The queue fast-forwards main to exactly that merge_group head, and that run scanned its base..head
# range, the commits the push brings. The push event's `before` is not compared with the run's base: the
# runs API does not return the merge_group base_sha, and a commit with the same SHA has the same history,
# so the head_sha match already fixes the range. A pull request, a merge_group build, an admin or
# break-glass merge (no queue run on that commit) and a direct push scan as before. Any doubt (an API
# error, no queue run, an unknown event) is skip=false.
#
# Env: EVENT_NAME, REF, SHA, REPO, GH_TOKEN (gh). Read rights only (actions, contents).
# Needs bash 5, gh, jq (gh --jq).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

emit() { echo "skip=$1"; echo "reason=$2"; }

decide() {
	local head
	if [[ ${EVENT_NAME:-} != push ]]; then
		emit false "event '${EVENT_NAME:-}': scanning"
		return
	fi
	if [[ ${REF:-} != refs/heads/main || -z ${SHA:-} || -z ${REPO:-} ]]; then
		emit false "not a push to main: scanning"
		return
	fi
	if ! head=$(gh api "repos/$REPO/actions/workflows/secrets.yml/runs?event=merge_group&status=success&per_page=30" \
		--jq ".workflow_runs[] | select(.conclusion == \"success\" and .head_sha == \"$SHA\") | .head_sha" 2>/dev/null); then
		emit false "cannot list the merge_group runs of secrets: scanning"
		return
	fi
	head=${head%%$'\n'*}
	if [[ -z $head ]]; then
		emit false "no successful merge_group secrets run on commit $SHA (an admin merge, a direct push, or a run older than the last 30): scanning"
		return
	fi
	emit true "a successful merge_group secrets run scanned commit $SHA"
}

case ${1:-} in
decide) decide ;;
*)
	echo "usage: hack/secrets-skip.sh decide" >&2
	exit 2
	;;
esac
