#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Decides whether the `changes` job of ci.yml switches the heavy jobs off, and applies
# the decision to the output of hack/ci-changes.sh. The heavy jobs are the ones `changes` gates: go-*
# (through `go`), web-* (through `web`), generated code, chart, chart smoke, escape (admission),
# flavors and opencode plugin. A job switched off by its `if:` reports success, so every required check still reports.
# On a push to main with skip=true, `scripts` and `lint-shell` are skipped too (their own `if:`
# in ci.yml, not an output of this script): the merge queue already ran them on this tree.
# `quick-checks` and `changes` always run.
#
#   hack/ci-skip-heavy.sh decide   prints skip=true|false and reason=<text> (lines for $GITHUB_OUTPUT)
#   hack/ci-skip-heavy.sh lab      prints go_test_lab=true|false and go_test_lab_reason=<text>: whether the
#                                  job go-test is satisfied by an external run (old issue 610), see below
#   hack/ci-skip-heavy.sh apply    reads the name=value lines of hack/ci-changes.sh on stdin and
#                                  prints them back: the heavy names become false when SKIP=true.
#                                  Always adds flavors_publish=<the flavors value before the switch>,
#                                  because flavor-publish (a push to main, it publishes an image)
#                                  must run whatever is skipped.
#
# decide, by event (EVENT_NAME, REPO, GH_TOKEN, and per event):
#   pull_request  PR_DRAFT (github.event.pull_request.draft, true|false). skip=true while the event
#                 says draft: a draft runs nothing heavy and its `go` and `web` report success with
#                 "draft: not run" (ci.yml). The payload flag, not the live state. A re-run replays the
#                 original event, so a re-run of a draft run is a draft run again (re-run the
#                 ready_for_review run instead). Marking it ready fires ready_for_review, which runs
#                 everything on the same head, and so does every push to a ready pull request. A draft
#                 cannot be queued or merged, and no gate reads a draft's green. No flag: skip=false.
#   merge_group   (the merge queue, #225) skip=false always: the queue commit is main plus the
#                 queued pull requests, a tree no check has seen, so skip-heavy never skips on it.
#                 ci.yml passes merge_group.base_sha to hack/ci-changes.sh, which path-filters against
#                 it like a ready pull request. A draft cannot be queued.
#   push          REF, SHA. skip=true only when all of this holds, else everything runs:
#                   - REF is refs/heads/main and SHA has a parent;
#                   - a `ci` run of event merge_group finished with conclusion success for a commit
#                     (the queue build) whose tree equals SHA's tree: the merge queue already ran the
#                     whole suite on exactly these files. A pull request head's green never counts (a
#                     draft run is green by skipping), and an admin merge has no queue run, so its push
#                     runs everything (ADR 0094, amendment);
#                   - flavors (required in the queue) succeeded on that queue commit, or nothing the flavors suite
#                     builds from changed between the first parent and SHA (hack/ci-changes.sh).
#                 Only read rights are used (contents, checks, actions). Any doubt (an API error, no
#                 queue run, a shallow clone, an unknown event) is skip=false.
# lab (old issue 610): go_test_lab=true only for a pull_request event, not a draft (SKIP != true), whose head
#   (HEAD_SHA: github.event.pull_request.head.sha, not the merge ref github.sha) has the commit status
#   `go-test-lab` = success (hack/go-test-lab.sh; an external go-test run that passed on exactly this head). ci.yml then skips the go-test job and the `go` gate accepts that skip.
#   Anything else, an API error included, is false: go-test runs on the hosted runner as before. There is no
#   waiting for a status that arrives later; a status posted before the pull request is marked ready counts.
# Needs bash 5, git, gh, jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)

heavy=(go web generate chart chartsmoke escape flavors opencodeplugin tidy vuln shell scripts)

emit() { echo "skip=$1"; echo "reason=$2"; }

# decide_pull_request: skip while the event says the pull request is a draft. The flag is the one in the
# event payload (PR_DRAFT: github.event.pull_request.draft). No API call: a re-run replays the original
# event, so a re-run of a draft run is a draft run again.
decide_pull_request() {
	case ${PR_DRAFT:-} in
	true) emit true "draft pull request: the heavy jobs run when it is marked ready for review" ;;
	false) emit false "ready pull request: running everything" ;;
	*) emit false "no draft flag in the event ('${PR_DRAFT:-}'): running everything" ;;
	esac
}

# queue_head: print the head commit of a successful merge_group `ci` run whose tree is $1 (the newest first),
# read from each run's own head_commit.tree_id (one call, no per-commit lookups).
# Return 1 when there is none, 2 when the runs cannot be listed.
queue_head() {
	local want=$1 sha
	if ! sha=$(gh api "repos/$REPO/actions/workflows/ci.yml/runs?event=merge_group&status=success&per_page=30" \
		--jq ".workflow_runs[] | select(.conclusion == \"success\" and .head_commit.tree_id == \"$want\") | .head_sha" 2>/dev/null); then
		return 2
	fi
	sha=${sha%%$'\n'*}
	[[ -n $sha ]] || return 1
	printf '%s\n' "$sha"
}

# decide_push: skip when a successful merge_group run (the queue build) tested this very tree.
decide_push() {
	local parents changed tree_m qhead rc=0 flavors_line conclusion runs
	if [[ ${REF:-} != refs/heads/main || -z ${SHA:-} || -z ${REPO:-} ]]; then
		emit false "not a push to main: running everything"
		return
	fi
	if ! parents=$(git rev-list --parents -n1 "$SHA" 2>/dev/null); then
		emit false "cannot read $SHA (shallow clone?): running everything"
		return
	fi
	read -ra p <<<"$parents"
	if ((${#p[@]} < 2)); then
		emit false "$SHA has no parent: running everything"
		return
	fi
	if ! tree_m=$(git rev-parse "$SHA^{tree}" 2>/dev/null); then
		emit false "cannot read the tree of $SHA: running everything"
		return
	fi
	qhead=$(queue_head "$tree_m") || rc=$?
	case $rc in
	0) ;;
	2)
		emit false "cannot list the merge_group runs of ci: running everything"
		return
		;;
	*)
		emit false "no successful merge_group ci run on the tree of $SHA (an admin merge, or a run older than the last 30): running everything"
		return
		;;
	esac
	# flavors is skipped on a pull request and may be skipped on the queue commit when nothing it builds from changed, so a green queue run does not always prove it, and flavor-publish ships what it tested:
	# an image is published untested unless flavors passed on the queue commit or nothing it builds from changed.
	if ! runs=$(gh api --paginate "repos/$REPO/commits/$qhead/check-runs?per_page=100" \
		--jq '.check_runs[] | select(.app.slug == "github-actions") | [.name, .id, .status, (.conclusion // "none")] | @tsv' 2>/dev/null); then
		emit false "cannot read the check runs of the queue commit $qhead: running everything"
		return
	fi
	conclusion=$(sort -t$'\t' -k1,1 -k2,2n <<<"$runs" | awk -F'\t' '$1 == "flavors" { last = $0 } END { print last }' | awk -F'\t' '$3 == "completed" { print $4 }')
	if [[ $conclusion != success ]]; then
		changed=$(git diff --name-only "${p[1]}" "$SHA" 2>/dev/null) || changed=
		if [[ -z $changed ]] || ! flavors_line=$("$root/hack/ci-changes.sh" --stdin <<<"$changed" 2>/dev/null | grep -x 'flavors=.*') || [[ $flavors_line != flavors=false ]]; then
			emit false "flavors did not pass on the queue commit $qhead (${conclusion:-no completed run}) and files it builds from may have changed: running everything"
			return
		fi
	fi
	emit true "a successful merge_group ci run (commit $qhead) tested the tree of $SHA"
}

# lab: is go-test satisfied by a green go-test-lab status on the pull request head (old issue 610)?
lab() {
	local state
	if [[ ${EVENT_NAME:-} != pull_request ]]; then
		echo "go_test_lab=false"; echo "go_test_lab_reason=not a pull request: go-test runs"
	elif [[ ${SKIP:-} == true ]]; then
		echo "go_test_lab=false"; echo "go_test_lab_reason=the heavy jobs are off: nothing to satisfy"
	elif [[ -z ${HEAD_SHA:-} ]]; then
		echo "go_test_lab=false"; echo "go_test_lab_reason=no head commit given: go-test runs"
	elif ! state=$("$root/hack/go-test-lab.sh" state "$HEAD_SHA" 2>/dev/null); then
		echo "go_test_lab=false"; echo "go_test_lab_reason=cannot read the statuses of ${HEAD_SHA:0:12}: go-test runs"
	elif [[ $state == success ]]; then
		echo "go_test_lab=true"; echo "go_test_lab_reason=go-test-lab is success on ${HEAD_SHA:0:12}: go-test passed in an external run"
	else
		echo "go_test_lab=false"; echo "go_test_lab_reason=no green go-test-lab on ${HEAD_SHA:0:12} (${state:-none}): go-test runs"
	fi
}

decide() {
	case ${EVENT_NAME:-} in
	pull_request) decide_pull_request ;;
	push) decide_push ;;
	merge_group) emit false "merge group build: the merge queue tests main plus the queued pull requests, so the heavy jobs are never skipped here (ci-changes.sh still path-filters against merge_group.base_sha)" ;;
	*) emit false "event '${EVENT_NAME:-}': running everything" ;;
	esac
}

apply() {
	local line name value flavors=true h skip=false
	[[ ${SKIP:-} == true ]] && skip=true
	while IFS= read -r line; do
		[[ -n $line ]] || continue
		name=${line%%=*}
		value=${line#*=}
		[[ $name != flavors ]] || flavors=$value
		if [[ $skip == true ]]; then
			for h in "${heavy[@]}"; do
				[[ $name == "$h" ]] && value=false
			done
		fi
		echo "$name=$value"
	done
	echo "flavors_publish=$flavors"
}

case ${1:-} in
decide) decide ;;
apply) apply ;;
lab) lab ;;
*)
	echo "usage: hack/ci-skip-heavy.sh decide|apply|lab" >&2
	exit 2
	;;
esac
