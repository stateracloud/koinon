#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The route guard (ADR 0105): did the commit that reached main come out of a merge queue build? Run by
# .github/workflows/route-guard.yml on every push to main, as a fast in-repo signal. The real alarm is the overnight
# report that reads the rule-suites API (which records every bypass whatever tool made it).
#
# A queue merge puts on main a tree that a `merge_group` run of `ci` built, and the queue merged it because every
# REQUIRED job (hack/ci-required.txt) passed: the run itself can still be in progress or red (a `flavors`
# job still running), so this judges the jobs, not the run's conclusion, and polls an in-progress run for up to POLL_MAX seconds
# (default 600). When no such run exists for the pushed tree it opens an issue keyed by the sha, or comments on the
# open one, labelled `break-glass`, unless the commit is the merge of a pull request that changes a path under
# `.github/` and whose required checks were green on its head: that is the expected `--admin` route
# (ADR 0105) and gets no issue. A push with no pull request at all is a real hit. When it cannot tell (an API read
# failed), it opens the issue anyway and says "could not verify". The comparison is by TREE, as
# hack/ci-skip-heavy.sh and hack/release-gate.sh do; whether the landed sha equals merge_group.head_sha is ADR
# 0105's open probe P3.
#
# It never fails: exit 0 always, because a red job on main is noise for every later reader and cannot undo the
# merge. Environment: SHA (the pushed commit), REPO (owner/name), GH (the gh command, default gh), REQUIRED (the
# ci-required.txt to read), POLL_MAX and POLL_SLEEP (seconds).
# Needs gh and jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail
export GH_PAGER=cat PAGER=cat

GH=${GH:-gh}
REQUIRED=${REQUIRED:-$(cd "$(dirname "$0")" && pwd)/ci-required.txt}
POLL_MAX=${POLL_MAX:-600}
POLL_SLEEP=${POLL_SLEEP:-20}
if [[ -z ${SHA:-} || -z ${REPO:-} ]]; then
	echo "route-guard: SHA and REPO are required; nothing checked" >&2
	exit 0
fi

# Required job names of the ci workflow (aggregators and plain jobs), and all required check names of a pull request
# head (those plus the external ones).
ci_jobs=$(sed -e 's/#.*//' -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' "$REQUIRED" 2>/dev/null |
	awk 'NF && $1 != "advisory" && $1 != "external" { sub(/^aggregator /, ""); print }')
pr_checks=$(sed -e 's/#.*//' -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' "$REQUIRED" 2>/dev/null |
	awk 'NF && $1 != "advisory" { sub(/^(aggregator|external) /, ""); print }' | sort -u)

# run_state <run id>: ok | bad | pending | unknown, from the jobs of one merge_group run.
run_state() {
	local jobs name row st
	if ! jobs=$("$GH" api "repos/$REPO/actions/runs/$1/jobs?per_page=100" --paginate \
		--jq '.jobs[] | [.name, .status, (.conclusion // "none")] | @tsv' 2>/dev/null); then
		echo unknown
		return
	fi
	[[ -n $ci_jobs && -n $jobs ]] || { echo unknown; return; }
	st=ok
	while IFS= read -r name; do
		row=$(awk -F'\t' -v n="$name" '$1 == n { r = $0 } END { print r }' <<<"$jobs")
		if [[ -z $row ]]; then st=pending; continue; fi
		IFS=$'\t' read -r _ jstatus jconc <<<"$row"
		if [[ $jstatus != completed ]]; then
			[[ $st == bad ]] || st=pending
		elif [[ $jconc != success && $jconc != skipped ]]; then
			st=bad
		fi
	done <<<"$ci_jobs"
	echo "$st"
}

# queue_state <tree>: queue (some run's required jobs all passed) | pending | none | unknown; polls pending runs.
queue_state() {
	local tree=$1 waited=0 runs id best st seen_unknown
	while :; do
		if ! runs=$("$GH" api "repos/$REPO/actions/workflows/ci.yml/runs?event=merge_group&per_page=30" \
			--jq ".workflow_runs[] | select(.head_commit.tree_id == \"$tree\") | .id" 2>/dev/null); then
			echo unknown
			return
		fi
		best=none seen_unknown=0
		for id in $runs; do
			st=$(run_state "$id")
			case $st in
			ok) best=queue; break ;;
			pending) best=pending ;;
			unknown) seen_unknown=1 ;;
			esac
		done
		case $best in
		queue) echo queue; return ;;
		pending)
			if ((waited >= POLL_MAX)); then echo none; return; fi
			sleep "$POLL_SLEEP"
			waited=$((waited + POLL_SLEEP))
			;;
		*) if ((seen_unknown)); then echo unknown; else echo none; fi; return ;;
		esac
	done
}

# What does the pushed tree look like, and did a queue build test it?
verdict="" # unqueued | unknown
why=""
if ! tree=$("$GH" api "repos/$REPO/git/commits/$SHA" --jq .tree.sha 2>/dev/null) || [[ -z $tree ]]; then
	verdict=unknown why="could not read the tree of $SHA"
else
	case $(queue_state "$tree") in
	queue)
		echo "route-guard: $SHA came out of the merge queue (every required job passed on a merge_group build of its tree); nothing to report"
		exit 0
		;;
	unknown) verdict=unknown why="could not read the merge_group runs of ci or their jobs" ;;
	*) verdict=unqueued why="no merge_group ci run on the tree of $SHA whose required jobs all passed (an admin merge, a break-glass merge or a direct push, or a run older than the last 30)" ;;
	esac
fi

# Which pull request, and is this the expected --admin route (a .github/ PR whose required checks were green)?
label=break-glass
pr="" nopr=0
if ! pr=$("$GH" api "repos/$REPO/commits/$SHA/pulls" --jq '.[0].number // empty' 2>/dev/null); then
	pr=""
	verdict=unknown why="$why; could not look up the merged pull request"
elif [[ -z $pr ]]; then
	nopr=1
	[[ $verdict == unknown ]] || why="$why; no pull request is associated with this commit: a direct push"
else
	if files=$("$GH" api "repos/$REPO/pulls/$pr/files" --paginate --jq '.[].filename' 2>/dev/null) && [[ -n $files ]]; then
		if grep -q '^\.github/' <<<"$files"; then
			head=$("$GH" api "repos/$REPO/pulls/$pr" --jq .head.sha 2>/dev/null) || head=""
			checks=""
			# 15368 is the GitHub Actions app id; hack/release-gate.sh (conclusion_in_run) uses the same id.
			[[ -z $head ]] || checks=$("$GH" api "repos/$REPO/commits/$head/check-runs?per_page=100" --paginate \
				--jq '.check_runs[] | select(.app.id == 15368) | [.id, .name, .status, (.conclusion // "none")] | @tsv' 2>/dev/null) || checks=""
			if [[ -z $checks ]]; then
				verdict=unknown why="$why; could not read the checks of the head of PR #$pr"
			else
				allgreen=1
				while IFS= read -r name; do
					row=$(sort -t$'\t' -k1,1n <<<"$checks" | awk -F'\t' -v n="$name" '$2 == n { r = $0 } END { print r }')
					IFS=$'\t' read -r _ _ cst ccon <<<"$row"
					if [[ -z $row || $cst != completed || ($ccon != success && $ccon != skipped) ]]; then
						allgreen=0
						why="$why; check '$name' was not green on the head of PR #$pr (${ccon:-missing})"
					fi
				done <<<"$pr_checks"
				if ((allgreen)); then
					echo "route-guard: $SHA is the --admin merge of .github PR #$pr with every required check green on its head; expected route, nothing to report"
					exit 0
				fi
			fi
		fi
	else
		verdict=unknown why="$why; could not read the files of PR #$pr"
	fi
fi

short=${SHA:0:12}
key="route-guard: $short"
if [[ $verdict == unknown ]]; then
	title="$key could not verify the route to main"
elif ((nopr)); then
	title="$key reached main by a direct push"
else
	title="$key reached main without a merge queue build"
fi
if ((nopr)) && [[ $verdict != unknown ]]; then
	note="A direct push to main (no pull request): a real hit, not a false alarm."
elif [[ $verdict == unknown ]]; then
	note="Could not verify: an API read failed, so this may be a false alarm."
else
	note="The merge queue did not build this tree."
fi
body="Commit \`$SHA\` is on main.

- Check: $why
- Pull request: ${pr:+#$pr}${pr:-none found}
- $note
- Label: \`$label\`

Why this exists: ADR 0105. The queue is the route to main; the expected exception is \`--admin\` for a \`.github/**\` PR with green checks, which gets no issue. The overnight report reads GitHub's rule-suites record of every bypass. Reason and approver of a break-glass merge are in the \`break-glass\` issue \`hack/pr-merge.sh --break-glass\` opens. Close this issue with a line saying what happened."

existing=$("$GH" issue list --state open --search "\"$key\" in:title" --json number --jq '.[0].number // empty' 2>/dev/null) || existing=""
if [[ -n $existing ]]; then
	"$GH" issue comment "$existing" --body "Seen again for the same sha. $why" >/dev/null 2>&1 ||
		echo "route-guard: could not comment on issue #$existing" >&2
	echo "route-guard: commented on issue #$existing"
	exit 0
fi
"$GH" issue create --title "$title" --body "$body" --label "$label" >/dev/null 2>&1 ||
	"$GH" issue create --title "$title" --body "$body" >/dev/null 2>&1 ||
	echo "route-guard: WARNING: could not open the issue for $SHA ($title)" >&2
echo "route-guard: $title ($label)"
exit 0
