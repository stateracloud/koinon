#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Decides whether the `changes` job of ci.yml switches the heavy jobs off (#593, #595), and applies
# the decision to the output of hack/ci-changes.sh. The heavy jobs are the ones `changes` gates: go-*
# (through `go`), web-* (through `web`), generated code, chart, chart smoke, docs, escape (admission)
# and flavors. A job switched off by its `if:` reports success, so every required check still reports.
# On a push to main with skip=true, `scripts` and `lint-shell` are skipped too (#594, their own `if:`
# in ci.yml, not an output of this script): the pull request head already passed them, as it must
# for `decide` to say skip. `quick-checks` and `changes` always run.
#
#   hack/ci-skip-heavy.sh decide   prints skip=true|false and reason=<text> (lines for $GITHUB_OUTPUT)
#   hack/ci-skip-heavy.sh apply    reads the name=value lines of hack/ci-changes.sh on stdin and
#                                  prints them back: the heavy names become false when SKIP=true.
#                                  Always adds flavors_publish=<the flavors value before the switch>,
#                                  because flavor-publish (a push to main, it publishes an image)
#                                  must run whatever is skipped.
#
# decide, by event (EVENT_NAME, REPO, GH_TOKEN, and per event):
#   pull_request  PR_NUMBER. skip=true while the pull request is a draft, read live from the API (the
#                 event payload of a re-run is stale: a draft run re-run after "ready" would still say
#                 draft). Marking it ready fires ready_for_review, which runs everything on the same
#                 head, and so does every push to a ready pull request.
#   push          REF, SHA. skip=true only when all of this holds, else everything runs:
#                   - REF is refs/heads/main and SHA is a merge commit with two parents;
#                   - the merge commit's tree equals the tree of its second parent (the pull request
#                     head), i.e. the merge brought nothing the head had not been tested with;
#                   - the first parent is an ancestor of that head (the head was up to date);
#                   - flavors (advisory) succeeded on the head, or nothing the flavors suite builds
#                     from changed between the first parent and the merge (hack/ci-changes.sh);
#                   - every required check that ci.yml produces (hack/ci-required.txt, not the
#                     `external` ones, which other workflows produce) has a newest check run, made by
#                     GitHub Actions on that head, that is completed with conclusion success, skipped
#                     or neutral (skipped is how a job that does not apply reports).
#                 Only read rights are used (contents, checks). Any doubt (an API error, a missing
#                 check, a shallow clone, an unknown event) is skip=false.
# Environment CI_REQUIRED overrides the required list (tests). Needs bash 5, git, gh, jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
required_file=${CI_REQUIRED:-$root/hack/ci-required.txt}

heavy=(go web generate chart chartsmoke docs escape flavors)

emit() { echo "skip=$1"; echo "reason=$2"; }

# required_names: the check names ci.yml produces that the ruleset requires, one per line.
required_names() {
	local line
	while IFS= read -r line; do
		line=${line%%#*}
		line=${line#"${line%%[![:space:]]*}"}
		line=${line%"${line##*[![:space:]]}"}
		[[ -n $line ]] || continue
		case $line in
		external\ * | advisory\ *) continue ;;
		aggregator\ *) line=${line#aggregator } ;;
		esac
		printf '%s\n' "$line"
	done <"$required_file"
}

# decide_pull_request: skip while the pull request is a draft.
decide_pull_request() {
	local draft
	if [[ -z ${PR_NUMBER:-} || -z ${REPO:-} ]]; then
		emit false "no pull request number or repository: running everything"
		return
	fi
	if ! draft=$(gh api "repos/$REPO/pulls/$PR_NUMBER" --jq .draft 2>/dev/null); then
		emit false "could not read the draft state of #$PR_NUMBER: running everything"
		return
	fi
	case $draft in
	true) emit true "draft pull request: the heavy jobs run when it is marked ready for review" ;;
	false) emit false "ready pull request: running everything" ;;
	*) emit false "unexpected draft state '$draft': running everything" ;;
	esac
}

# decide_push: skip when the merge commit is the tested pull request head, tree for tree.
decide_push() {
	local parents head changed flavors_line tree_m tree_h required name runs checks latest conclusion
	if [[ ${REF:-} != refs/heads/main || -z ${SHA:-} || -z ${REPO:-} ]]; then
		emit false "not a push to main: running everything"
		return
	fi
	if ! parents=$(git rev-list --parents -n1 "$SHA" 2>/dev/null); then
		emit false "cannot read $SHA (shallow clone?): running everything"
		return
	fi
	read -ra p <<<"$parents"
	if ((${#p[@]} != 3)); then
		emit false "$SHA is not a merge commit with two parents: running everything"
		return
	fi
	head=${p[2]}
	if ! tree_m=$(git rev-parse "$SHA^{tree}" 2>/dev/null) || ! tree_h=$(git rev-parse "$head^{tree}" 2>/dev/null); then
		emit false "cannot read the trees of $SHA and $head: running everything"
		return
	fi
	if [[ $tree_m != "$tree_h" ]]; then
		emit false "the merge tree differs from the pull request head $head (merged out of date, or an admin merge): running everything"
		return
	fi
	# The head must contain the first parent: a merge of an out-of-date branch, or of a branch whose
	# tree only happens to match, was not tested on top of what main had.
	if ! git merge-base --is-ancestor "${p[1]}" "$head" 2>/dev/null; then
		emit false "the first parent ${p[1]} is not an ancestor of the pull request head $head: running everything"
		return
	fi
	if ! required=$(required_names) || [[ -z $required ]]; then
		emit false "cannot read the required checks: running everything"
		return
	fi
	if ! runs=$(gh api --paginate "repos/$REPO/commits/$head/check-runs?per_page=100" \
		--jq '.check_runs[] | select(.app.slug == "github-actions") | [.name, .id, .status, (.conclusion // "none")] | @tsv' 2>/dev/null); then
		emit false "cannot read the check runs of $head: running everything"
		return
	fi
	# The newest run (highest id) of each name decides: a re-run or a later run of the same job
	# (a draft run, then the ready run) replaces the earlier one.
	latest=$(sort -t$'\t' -k1,1 -k2,2n <<<"$runs" | awk -F'\t' '{ last[$1] = $0 } END { for (n in last) print last[n] }')
	while IFS= read -r name; do
		checks=$(awk -F'\t' -v n="$name" '$1 == n { print }' <<<"$latest")
		if [[ -z $checks ]]; then
			emit false "required check '$name' has no run on $head: running everything"
			return
		fi
		conclusion=$(awk -F'\t' '$3 == "completed" { print $4 }' <<<"$checks")
		case $conclusion in
		success | skipped | neutral) ;;
		*)
			emit false "required check '$name' on $head is not green (${conclusion:-not completed}): running everything"
			return
			;;
		esac
	done <<<"$required"
	# flavors is advisory, so it is not in the required list, but flavor-publish ships what it tested:
	# an image is published untested unless flavors passed on the head or nothing it builds from changed.
	conclusion=$(awk -F'\t' '$1 == "flavors" && $3 == "completed" { print $4 }' <<<"$latest")
	if [[ $conclusion != success ]]; then
		changed=$(git diff --name-only "${p[1]}" "$SHA" 2>/dev/null) || changed=
		if [[ -z $changed ]] || ! flavors_line=$("$root/hack/ci-changes.sh" --stdin <<<"$changed" 2>/dev/null | grep -x 'flavors=.*') || [[ $flavors_line != flavors=false ]]; then
			emit false "flavors did not pass on $head (${conclusion:-no completed run}) and files it builds from may have changed: running everything"
			return
		fi
	fi
	emit true "the merge tree equals the tree of $head, whose required checks passed"
}

decide() {
	case ${EVENT_NAME:-} in
	pull_request) decide_pull_request ;;
	push) decide_push ;;
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
*)
	echo "usage: hack/ci-skip-heavy.sh decide|apply" >&2
	exit 2
	;;
esac
