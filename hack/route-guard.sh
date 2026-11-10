#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The route guard (ADR 0105): did the commit that reached main come out of a merge queue build? Run by
# .github/workflows/route-guard.yml on every push to main, as a fast in-repo signal. The real alarm is the overnight
# report that reads the rule-suites API (which records every bypass whatever tool made it).
#
# A queue merge puts on main a tree that a successful `merge_group` run of `ci` already built. When no such run exists
# for the pushed tree (an admin merge, a break-glass merge, a direct push), this opens an issue keyed by the sha, or
# comments on the open one, labelled `admin:github` when the merged pull request changed a path under `.github/`
# (the expected `--admin` route) and `break-glass` otherwise. When it cannot tell (an API read failed), it opens the
# issue anyway and says "could not verify". The comparison is by TREE, as hack/ci-skip-heavy.sh and
# hack/release-gate.sh do; whether the landed sha equals merge_group.head_sha is ADR 0105's open probe P3.
#
# It never fails: exit 0 always, because a red job on main is noise for every later reader and cannot undo the
# merge. Environment: SHA (the pushed commit), REPO (owner/name), GH (the gh command, default gh).
# Needs gh and jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail
export GH_PAGER=cat PAGER=cat

GH=${GH:-gh}
if [[ -z ${SHA:-} || -z ${REPO:-} ]]; then
	echo "route-guard: SHA and REPO are required; nothing checked" >&2
	exit 0
fi

# What does the pushed tree look like, and did a queue build test it?
verdict="" # queue | unqueued | unknown
why=""
if ! tree=$("$GH" api "repos/$REPO/git/commits/$SHA" --jq .tree.sha 2>/dev/null) || [[ -z $tree ]]; then
	verdict=unknown why="could not read the tree of $SHA"
elif ! qhead=$("$GH" api "repos/$REPO/actions/workflows/ci.yml/runs?event=merge_group&status=success&per_page=30" \
	--jq ".workflow_runs[] | select(.conclusion == \"success\" and .head_commit.tree_id == \"$tree\") | .head_sha" 2>/dev/null); then
	verdict=unknown why="could not list the merge_group runs of ci"
elif [[ -n ${qhead%%$'\n'*} ]]; then
	echo "route-guard: $SHA came out of the merge queue (build ${qhead%%$'\n'*}); nothing to report"
	exit 0
else
	verdict=unqueued why="no successful merge_group ci run on the tree of $SHA (an admin merge, a break-glass merge or a direct push, or a run older than the last 30)"
fi

# Which class? The merged pull request's changed paths decide: `.github/` is the expected --admin route.
label=break-glass
pr=""
if pr=$("$GH" api "repos/$REPO/commits/$SHA/pulls" --jq '.[0].number // empty' 2>/dev/null) && [[ -n $pr ]]; then
	if files=$("$GH" api "repos/$REPO/pulls/$pr/files" --paginate --jq '.[].filename' 2>/dev/null) && [[ -n $files ]]; then
		if grep -q '^\.github/' <<<"$files"; then label=admin:github; fi
	else
		verdict=unknown why="$why; could not read the files of PR #$pr"
	fi
else
	pr=""
	[[ $verdict == unknown ]] || { verdict=unknown why="$why; could not find the merged pull request"; }
fi

short=${SHA:0:12}
key="route-guard: $short"
if [[ $verdict == unknown ]]; then
	title="$key could not verify the route to main"
else
	title="$key reached main without a merge queue build"
fi
body="Commit \`$SHA\` is on main.

- Check: $why
- Pull request: ${pr:+#$pr}${pr:-not found}
- Class: \`$label\` ($([[ $label == admin:github ]] && echo 'a .github/** pull request, the expected --admin route' || echo 'not a .github/** change, or not known: explain it'))
- $([[ $verdict == unknown ]] && echo 'could not verify: an API read failed, so this may be a false alarm' || echo 'The merge queue did not build this tree.')

Why this exists: ADR 0105. The queue is the route to main; the overnight report reads GitHub's rule-suites record of every bypass. Reason and approver of a break-glass merge are in the \`break-glass\` issue \`hack/pr-merge.sh --break-glass\` opens. Close this issue with a line saying what happened."

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
