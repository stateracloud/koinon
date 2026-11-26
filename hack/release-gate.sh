#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): runs in the upstream release workflow; it only reads, so it also runs from a fork.
#
# The release gate (plan item REL1): refuses a release unless the tag is a SemVer release tag, the
# tagged commit is on main, and the required CI checks concluded success on that commit. It does
# not run any tests. The release workflow runs it as its own job (`gate`), so it can later be
# swapped for a "local checks recorded" gate without touching the release job.
#
# Environment: TAG (v1.2.3 or v1.2.3-rc.1), SHA (the tagged commit), GITHUB_REPOSITORY and GH_TOKEN
# (and, optional, DRILL_RESULT_OUT: a path where the green kind drill-result.json of the tag is copied to)
# (needs checks:read, contents:read, actions:read and pull-requests:read; the ruleset is read with the implicit
# metadata permission). The required checks are those of hack/ci-required.txt in the tagged commit (#971; the
# `main` ruleset only when that tree has none). They are judged in this order (ADR 0104-era route, docs/dev/releasing.md): the tagged commit itself;
# where it reports `skipped` (the push skipped its heavy jobs because the merge queue had tested the
# tree) or, for the pull-request-only contexts (pr_only below), the successful merge_group `ci` run on a
# commit with the same tree (the queue build); and only when there is no queue run (an admin merge) the
# head commit of the pull request that merged the tagged commit, for the pull-request-only contexts and
# the dependency-age half of `quick checks` (for a merge commit, the PR whose head is its second parent: a
# merge queue group puts one merge commit per PR on main, #1373). A `go`/`web`/other ci job is never read from a PR head: a
# draft's green is vacuous. The queue build's result is read from the queue head's merge_group runs by
# run id (conclusion_in_run), not as the newest check run on the sha. Needs gh, git and awk, and a clone
# with origin/main.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
: "${TAG:?}" "${SHA:?}" "${GITHUB_REPOSITORY:?}" "${GH_TOKEN:?}"

# The required status checks come from the tagged commit's own hack/ci-required.txt (below), so a check
# added, renamed or dropped needs no edit here.
#
# Required contexts that are not reported by a push to main, read instead from the queue build (the
# merge_group run on the same tree), else from the head commit of the pull request that merged the
# tagged commit (the newest run that started before the merge; an admin merge has no queue build):
#   dco                  its workflow ran on pull_request only; #682 removed it (the sign-off is now in
#                        `quick checks`). Listed for a tag whose own ci-required.txt still requires it.
#   claim                runs on pull_request (hack/claim.sh check-pr, about the PR and its
#                        claim); on a push to main it is skipped. Not required since #682; listed for
#                        a tag whose own ci-required.txt still requires it.
#   e2e result           succeeds on main pushes too, but it is a composite that passes when the
#                        batched e2e was skipped; the real subject is the `quickstart` rule below,
#                        which demands success on the very commit.
#   contributor gate     runs on pull_request and merge_group only (contributor-gate.yml, #848); listed
#                        before it is required, so turning it on needs no edit here.
# This is a list, not "any context with no run on the commit", so a new push-triggered required
# check that is missing or red on the commit fails the gate instead of being excused by a PR run.
# A context not listed here must succeed on the tagged commit itself. When a workflow gains or
# loses a push trigger, move its context between the two cases.
pr_only=(dco claim "e2e result" "contributor gate")
# Contexts that must succeed on the tagged commit AND on the merged pull request's head: `quick
# checks` runs on main too, but its dependency age steps run on pull requests only (old issue 594; they were
# the PR-only job `dependency age`), so main's green says nothing about them.
both=("quick checks")

fail() { echo "release gate: $*" >&2; exit 1; }

[[ $TAG =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$ ]] ||
	fail "tag '$TAG' is not vX.Y.Z or vX.Y.Z-rc.N"

# How many merge queue runs the quickstart fallback reads: the per_page of one call, so 1..100 (no paging).
QUEUE_RUN_LIMIT=${QUEUE_RUN_LIMIT:-30}
[[ $QUEUE_RUN_LIMIT =~ ^([1-9][0-9]?|100)$ ]] ||
	fail "QUEUE_RUN_LIMIT '$QUEUE_RUN_LIMIT' is not an integer from 1 to 100"
# How many merge commits a merge queue group may hold before SHA (in_merge_group): bounds the first-parent walk.
MERGE_GROUP_LIMIT=${MERGE_GROUP_LIMIT:-10}
[[ $MERGE_GROUP_LIMIT =~ ^([1-9][0-9]?|100)$ ]] ||
	fail "MERGE_GROUP_LIMIT '$MERGE_GROUP_LIMIT' is not an integer from 1 to 100"

git fetch --no-tags --quiet origin main
git merge-base --is-ancestor "$SHA" origin/main ||
	fail "$SHA is not an ancestor of origin/main; release from a commit that has been merged"

# conclusion_of <sha> <name> [until]: the conclusion of the newest check run of that name (that
# started at or before the ISO time `until`, when given), or empty.
conclusion_of() {
	gh api --paginate "repos/$GITHUB_REPOSITORY/commits/$1/check-runs?per_page=100" \
		--jq '.check_runs[] | [.name, .started_at, (.conclusion // "pending")] | @tsv' |
		awk -F'\t' -v n="$2" -v u="${3:-}" '$1 == n && (u == "" || $2 <= u) && $2 >= t { t = $2; c = $3 } END { print c }'
}

# conclusion_in_run <sha> <name> <run ids, "|"-separated>: the conclusion of the check run of that name on
# <sha> that the Actions app (integration 15368, the same id as in hack/route-guard.sh) reported for one of those workflow runs (the run id is in
# the check run's details_url), or empty. A queue merge fast-forwards main to the queue commit, so one sha carries the
# merge_group run's result and the push run's later `skipped`: the queue build is read by run, never as
# the newest check run on the sha.
conclusion_in_run() {
	gh api --paginate "repos/$GITHUB_REPOSITORY/commits/$1/check-runs?per_page=100" \
		--jq ".check_runs[] | select(.app.id == 15368 and ((.details_url // \"\") | test(\"/actions/runs/($3)/\"))) | [.name, .started_at, (.conclusion // \"pending\")] | @tsv" |
		awk -F'\t' -v n="$2" '$1 == n && $2 >= t { t = $2; c = $3 } END { print c }'
}

# required_contexts: the required status-check contexts of every active rule that applies to the
# default branch, from the rules/branches endpoint (repository and organization rulesets, branch
# patterns resolved by GitHub). Fails closed when it cannot be read.
required_contexts() {
	local branch
	branch=$(gh api "repos/$GITHUB_REPOSITORY" --jq .default_branch) || return 1
	[[ -n $branch ]] || return 1
	gh api --paginate "repos/$GITHUB_REPOSITORY/rules/branches/$branch" \
		--jq '.[] | select(.type == "required_status_checks") | .parameters.required_status_checks[].context' |
		sort -u
}

# The required list of a release is the list of the tagged commit itself (#971): its own hack/ci-required.txt,
# read from the clone. Today's ruleset says nothing about an older tree: after a check is renamed (the ruleset
# names the new one) every earlier rc would fail "no check run found" for a check its tree never had. The ruleset
# is the fallback for a tree with no ci-required.txt, and fails closed.
# shellcheck source-path=SCRIPTDIR source=lib/ci-required.sh
. "$here/lib/ci-required.sh"
required_src="hack/ci-required.txt of $SHA"
if txt=$(git show "$SHA:hack/ci-required.txt" 2>/dev/null) && contexts=$(cir_parse <<<"$txt") && [[ -n $contexts ]]; then
	contexts=$(sort -u <<<"$contexts")
else
	required_src="the ruleset (no usable hack/ci-required.txt in $SHA)"
	contexts=$(required_contexts) ||
		fail "cannot read hack/ci-required.txt of $SHA nor the rules that apply to the default branch (gh api repos/$GITHUB_REPOSITORY/rules/branches/<default> failed); refusing to release without the required list"
	[[ -n $contexts ]] ||
		fail "no required status checks apply to the default branch; refusing to release without the required list"
fi
echo "required checks: $required_src"
mapfile -t required <<<"$contexts"

is_both() { local p; for p in "${both[@]}"; do [[ $p == "$1" ]] && return 0; done; return 1; }
is_pr_only() { local p; for p in "${pr_only[@]}"; do [[ $p == "$1" ]] && return 0; done; return 1; }

# commit_parents <sha>: the parents of a commit, one per line.
commit_parents() {
	gh api "repos/$GITHUB_REPOSITORY/git/commits/$1" --jq '.parents[]?.sha'
}
# merged_prs <sha>: "head<TAB>merged_at<TAB>merge_commit_sha" of each merged pull request that
# commits/<sha>/pulls lists.
merged_prs() {
	gh api "repos/$GITHUB_REPOSITORY/commits/$1/pulls" \
		--jq '.[] | select(.merged_at != null) | [.head.sha, .merged_at, (.merge_commit_sha // "")] | @tsv'
}
# brings_new <first parent> <second parent>: true when the merge brings the second parent onto main for the first
# time (it is not yet an ancestor of the first parent), as every queue or merge-button merge does. A re-merge of a
# head that is already on main (an admin push of `git merge --no-ff <old PR head>`) is false: no PR merged it.
# Read from the clone (origin/main is fetched with history); an error fails the gate.
brings_new() {
	local rc=0
	git merge-base --is-ancestor "$2" "$1" || rc=$?
	case $rc in
	0) return 1 ;;
	1) return 0 ;;
	*) fail "cannot tell whether $2 is an ancestor of $1" ;;
	esac
}
# in_merge_group <mcs>: true when SHA is the merge commit <mcs>, or a later merge commit of the same merge queue
# group (#1373). GitHub gives every PR of a group the group's first merge commit as merge_commit_sha. So SHA's
# first-parent walk must reach <mcs> within MERGE_GROUP_LIMIT steps, every commit on the way (<mcs> included)
# must have two parents, and the second parent of each commit between SHA and <mcs> must be the head of a
# merged PR whose merge_commit_sha is also <mcs>, and that commit must bring its second parent onto main for the
# first time (brings_new). Any other shape (a squash commit, a direct push, another
# group) is false.
in_merge_group() {
	local mcs=$1 cur=$SHA i ps rows
	[[ $mcs == "$SHA" ]] && return 0
	for ((i = 0; i <= MERGE_GROUP_LIMIT; i++)); do
		ps=$(commit_parents "$cur") || fail "cannot read the parents of $cur"
		mapfile -t ps <<<"$ps"
		((${#ps[@]} == 2)) || return 1
		if [[ $cur != "$SHA" ]]; then
			[[ $cur == "$mcs" ]] && return 0
			brings_new "${ps[0]}" "${ps[1]}" || return 1
			rows=$(merged_prs "${ps[1]}") || fail "cannot list the pull requests of ${ps[1]}"
			awk -F'\t' -v h="${ps[1]}" -v m="$mcs" '$1 == h && $3 == m { f = 1 } END { exit !f }' <<<"$rows" || return 1
		fi
		cur=${ps[0]}
	done
	return 1
}
pr_heads=
pr_heads_read=0
# load_pr_heads: sets pr_heads to "head<TAB>merged_at" of each pull request that merged SHA, one per line, or
# empty when none did (a direct push: then the PR-only contexts have no evidence). Called directly (not in
# $(...)) so the answer is cached.
#   - SHA is a merge commit (a queue or merge-button merge: first parent main, second parent the PR head): the
#     merged PRs whose head is SHA's second parent, listed from that commit (`commits/<parent 2>/pulls`), and
#     whose merge_commit_sha is SHA or the first merge commit of SHA's queue group (in_merge_group). A queue
#     group of N PRs puts N merge commits on main, but every PR of the group gets the first one as its
#     merge_commit_sha, so `commits/<SHA>/pulls` is empty for the 2nd..Nth commit (#1373). A merge whose second
#     parent was already on main (brings_new) has no PR.
#   - Any other commit (a squash or rebase merge, which has one parent): the merged PRs of
#     `commits/<SHA>/pulls` whose merge_commit_sha is SHA.
load_pr_heads() {
	((pr_heads_read == 0)) || return 0
	pr_heads_read=1
	local parents p2 rows h merged mcs
	parents=$(commit_parents "$SHA") || fail "cannot read the parents of $SHA"
	mapfile -t parents <<<"$parents"
	if ((${#parents[@]} == 2)); then
		p2=${parents[1]}
		[[ $p2 =~ ^[0-9a-f]{7,64}$ ]] || fail "cannot read the parents of $SHA"
		brings_new "${parents[0]}" "$p2" || return 0
		rows=$(merged_prs "$p2") || fail "cannot list the pull requests of $p2 (the second parent of $SHA)"
		while IFS=$'\t' read -r h merged mcs; do
			[[ $h == "$p2" && -n $mcs ]] || continue
			in_merge_group "$mcs" || continue
			pr_heads+=$h$'\t'$merged$'\n'
		done <<<"$rows"
	else
		rows=$(merged_prs "$SHA") || fail "cannot list the pull requests of $SHA"
		while IFS=$'\t' read -r h merged mcs; do
			[[ -n $h && $mcs == "$SHA" ]] || continue
			pr_heads+=$h$'\t'$merged$'\n'
		done <<<"$rows"
	fi
}
# from_pr_heads <name>: the conclusion of the check on the head of the pull request that merged SHA. Call
# load_pr_heads first.
from_pr_heads() {
	local h merged got=
	while IFS=$'\t' read -r h merged; do
		[[ -n $h ]] || continue
		got=$(conclusion_of "$h" "$1" "$merged")
		[[ -n $got ]] && break
	done <<<"$pr_heads"
	echo "$got"
}

# queue_ci_head: the head commit of a successful merge_group `ci` run whose commit has SHA's tree (the
# queue build that tested exactly these files), or empty. Called directly (not in $(...)) so the answer
# is cached. Reads the newest QUEUE_RUN_LIMIT runs' own head_commit.tree_id: one call, no tree lookups.
# qci_run is that run's id; qci_runs is the "|"-separated ids of every merge_group workflow run on that head
# (ci, dco, claims, e2e-kind: each is its own run). A context is read from those runs (conclusion_in_run),
# which also holds when the queue commit is the tagged commit itself (the push run's later `skipped` is
# another run, so it is never read).
qci_head=
qci_run=
qci_runs=
qci_read=0
queue_ci_head() {
	local tree
	if ((qci_read == 0)); then
		qci_read=1
		tree=$(gh api "repos/$GITHUB_REPOSITORY/git/commits/$SHA" --jq .tree.sha) || fail "cannot read the tree of $SHA"
		[[ $tree =~ ^[0-9A-Za-z]+$ ]] || fail "cannot read the tree of $SHA"
		local row
		row=$(gh api "repos/$GITHUB_REPOSITORY/actions/workflows/ci.yml/runs?event=merge_group&status=success&per_page=$QUEUE_RUN_LIMIT" \
			--jq ".workflow_runs[] | select(.conclusion == \"success\" and .head_commit.tree_id == \"$tree\") | [.head_sha, .id] | @tsv" | head -n1) ||
			fail "cannot list the merge_group ci runs"
		IFS=$'\t' read -r qci_head qci_run <<<"$row"
		if [[ -n $qci_head ]]; then
			local ids
			ids=$(gh api --paginate "repos/$GITHUB_REPOSITORY/actions/runs?event=merge_group&head_sha=$qci_head&per_page=100" \
				--jq '.workflow_runs[].id') ||
				fail "cannot list the merge_group runs of $qci_head"
			qci_runs=$(printf '%s\n%s\n' "$qci_run" "$ids" | grep -E '^[0-9]+$' | sort -u | paste -sd'|' -)
		fi
	fi
	return 0
}

failed=0
for name in "${required[@]}"; do
	queue_ci_head
	if is_pr_only "$name"; then
		got=
		[[ -z $qci_head ]] || got=$(conclusion_in_run "$qci_head" "$name" "$qci_runs")
		where="in the merge queue run on this tree ($qci_head)"
		if [[ -z $got ]]; then
			load_pr_heads
			got=$(from_pr_heads "$name")
			where="on the merged pull request's head (no merge queue run reported it: an admin merge)"
		fi
	else
		got=$(conclusion_of "$SHA" "$name")
		where="on $SHA"
		# A push to main skips the heavy jobs when a merge_group ci run already tested the tree
		# (hack/ci-skip-heavy.sh); they report `skipped` here. Then that queue build is the evidence, and
		# it must have passed (or skipped) them. A pull request head is never read for them.
		if [[ $got == skipped ]]; then
			got=
			[[ -z $qci_head ]] || got=$(conclusion_in_run "$qci_head" "$name" "$qci_runs")
			[[ $got == skipped ]] && got=success
			where="in the merge queue run on this tree ($qci_head; skipped on $SHA)"
			[[ -n $qci_head ]] || where="skipped on $SHA and no successful merge_group ci run has this tree"
		fi
	fi
	if [[ $got == success ]] && is_both "$name"; then
		load_pr_heads
		got=$(from_pr_heads "$name")
		where="on the merged pull request's head (it also passed on $SHA)"
	fi
	if [[ $got == success ]]; then
		echo "ok: $name"
	else
		echo "FAIL: $name: ${got:-no check run found} $where" >&2
		failed=1
	fi
done
# go-test: skipped on the tagged commit means the merge queue ran it (a merge_group run skips it only for a
# change with no Go in it). It is checked once more: on the queue build it must have succeeded, or the change
# must touch nothing the go-* jobs run for. A go-test with no run at all is the business of `go` above.
# touches_go <base> <head>: exit 0 when base..head is a change the go-* jobs run for (hack/ci-changes.sh
# go=true), 1 when it is not (documentation, web), 2 when that cannot be told.
touches_go() {
	local changed line
	changed=$(git diff --name-only --no-renames "$1" "$2" 2>/dev/null) || return 2
	[[ -n $changed ]] || return 2
	# Only go= is read: the directories of the harnesses suite (a `go list`) are not needed.
	line=$(CI_CHANGES_HARNESSES_DIRS=${CI_CHANGES_HARNESSES_DIRS:-none} "$here/ci-changes.sh" --stdin <<<"$changed" 2>/dev/null | grep -x 'go=.*') || return 2
	case $line in
	go=true) return 0 ;;
	go=false) return 1 ;;
	*) return 2 ;;
	esac
}
got=$(conclusion_of "$SHA" go-test)
if [[ $got == skipped ]]; then
	queue_ci_head
	got=
	[[ -z $qci_head ]] || got=$(conclusion_in_run "$qci_head" go-test "$qci_runs")
	tg=0
	case $got in
	success) echo "ok: go-test (in the merge queue run $qci_head)" ;;
	skipped | "")
		touches_go "$SHA^1" "$SHA" || tg=$?
		if [[ $tg == 1 ]]; then
			echo "ok: go-test (skipped: the change touches no Go)"
		else
			echo "FAIL: go-test: ${got:-no check run found} in the merge queue run on this tree and the change may touch Go (skipped on $SHA)" >&2
			failed=1
		fi
		;;
	*)
		echo "FAIL: go-test: $got in the merge queue run on this tree (skipped on $SHA)" >&2
		failed=1
		;;
	esac
fi
# The kind e2e is not a required check (it is batched on main, and `e2e result` passes when it is
# skipped), so a release asks for the `quickstart` job itself to have concluded success on this very
# commit. Two things widen that, both fail-closed (#444, ADR 0102):
#   - A `skipped` quickstart check run is no evidence either way: a queue merge fast-forwards main to the
#     queue commit, whose merge_group run passed, and the push run then adds a skipped quickstart
#     (hack/e2e-decide.sh, the push skip). The newest run that is not skipped decides; a failure or a
#     cancellation there fails the gate.
#   - A dispatch with `inputs.commit` (hack/e2e-bisect.sh) tests another commit than the one its check run
#     attaches to; e2e-kind.yml names that job `quickstart (commit)`, so it never matches `quickstart` here (#453).
#     Older bisect dispatches still carry the name `quickstart`: their workflow run is titled
#     `e2e-kind (<commit>) bisect <id>` and quickstart_conclusion drops check runs of such runs.
#   - With no such run on the commit, a green merge_group `quickstart` job whose head commit has the same
#     tree as the tagged commit is the evidence (the tree is what the e2e tested).
# If neither exists, dispatch e2e-kind on the commit first.
quickstart_conclusion() { # the newest check run named quickstart on SHA that is not skipped, or empty
	# Check runs from before #453 (bisect dispatches on older heads) are still named `quickstart`, so the
	# workflow runs on SHA are read (all pages) and a check run whose run is titled like a bisect
	# dispatch (`e2e-kind (<commit>) bisect <id>`, the run-name of e2e-kind.yml) is dropped as well.
	# GitHub returns at most 1,000 results for a filtered workflow-run list: at that size a bisect run
	# could hide beyond the cap, so the gate fails closed.
	local bisect total n
	# Per page, a first line `C total_count`, then `R id` per run and `B id` per bisect run.
	bisect=$(gh api --paginate "repos/$GITHUB_REPOSITORY/actions/workflows/e2e-kind.yml/runs?head_sha=$SHA&per_page=100" \
		--jq '"C \(.total_count // 0)", (.workflow_runs[] | "R \(.id)", (select((.display_title // "") | test("^e2e-kind \\(.+\\) bisect ")) | "B \(.id)"))') || return 2
	# n counts distinct run ids: a run that moved between pages while paging is read twice, not two runs.
	read -r total n < <(awk '$1 == "C" { if ($2 > t) t = $2 } $1 == "R" && !($2 in seen) { seen[$2] = 1; c++ } END { print t + 0, c + 0 }' <<<"$bisect")
	if ((total >= 1000 || n >= 1000)); then
		echo "release gate: $SHA has 1000 or more e2e-kind runs (GitHub's cap for a filtered run list): cannot rule out a bisect run" >&2
		return 2
	fi
	if ((total > n)); then
		echo "release gate: read $n of $total e2e-kind runs on $SHA: cannot rule out a bisect run" >&2
		return 2
	fi
	bisect=$(awk '$1 == "B" { print $2 }' <<<"$bisect")
	gh api --paginate "repos/$GITHUB_REPOSITORY/commits/$SHA/check-runs?per_page=100" \
		--jq '.check_runs[] | [.name, .started_at, (.conclusion // "pending"), ((.details_url // "") | capture("/runs/(?<r>[0-9]+)") | .r) // ""] | @tsv' |
		awk -F'\t' -v ids="$(tr '\n' ' ' <<<"$bisect")" 'BEGIN { n = split(ids, a, " "); for (i = 1; i <= n; i++) skip[a[i]] = 1 }
			$1 == "quickstart" && $3 != "skipped" && !($4 in skip) && $2 >= t { t = $2; c = $3 } END { print c }'
}
# queue_quickstart_run: prints the id of a green merge_group e2e run whose quickstart job succeeded on a
# commit with SHA's tree. Exit 0 found, 1 none, 2 an API call failed. It looks at the newest
# QUEUE_RUN_LIMIT (default 30, an integer 1..100: it is the per_page of one call, with no paging) green
# merge_group runs only, one git/commits call each at most, so a queue commit older than that is not
# found (dispatch e2e-kind on the commit instead).
queue_quickstart_run() {
	local tree runs id head htree ok n=0
	tree=$(gh api "repos/$GITHUB_REPOSITORY/git/commits/$SHA" --jq .tree.sha) || return 2
	[[ -n $tree ]] || return 2
	runs=$(gh api "repos/$GITHUB_REPOSITORY/actions/workflows/e2e-kind.yml/runs?event=merge_group&status=success&per_page=$QUEUE_RUN_LIMIT" \
		--jq '.workflow_runs[] | [.id, .head_sha] | @tsv') || return 2
	while IFS=$'\t' read -r id head; do
		[[ -n ${id:-} ]] || continue
		n=$((n + 1))
		((n <= QUEUE_RUN_LIMIT)) || break
		# the tagged commit itself is a queue commit (fast-forward merge): no tree lookup needed
		if [[ $head != "$SHA" ]]; then
			htree=$(gh api "repos/$GITHUB_REPOSITORY/git/commits/$head" --jq .tree.sha) || return 2
			[[ $htree == "$tree" ]] || continue
		fi
		ok=$(gh api "repos/$GITHUB_REPOSITORY/actions/runs/$id/jobs?per_page=100" \
			--jq '.jobs[] | select(.name == "quickstart" and .conclusion == "success") | .id') || return 2
		if [[ -n $ok ]]; then
			echo "$id"
			return 0
		fi
	done <<<"$runs"
	return 1
}
got=$(quickstart_conclusion) || fail "the e2e-kind workflow runs on $SHA could not be read"
if [[ $got == success ]]; then
	echo "ok: kind e2e (quickstart)"
elif [[ -z $got ]]; then
	qrc=0
	qrun=$(queue_quickstart_run) || qrc=$?
	case $qrc in
	0) echo "ok: kind e2e (quickstart passed in merge queue run $qrun on a commit with this commit's tree)" ;;
	2)
		echo "FAIL: kind e2e (quickstart job): no run on this commit, and the merge queue runs could not be read (an API error or a rate limit; only the newest $QUEUE_RUN_LIMIT are looked at)" >&2
		failed=1
		;;
	*)
		echo "FAIL: kind e2e (quickstart job): no run on this commit and no green merge queue run on its tree among the newest $QUEUE_RUN_LIMIT; run the e2e-kind workflow on $SHA" >&2
		failed=1
		;;
	esac
else
	echo "FAIL: kind e2e (quickstart job): $got; run the e2e-kind workflow on $SHA" >&2
	failed=1
fi
# The restore drill (ADR 0109 decision 2, #1039): every tag, rc or final, needs a green drill on kind for its
# own commit. The `drill` job of e2e-kind.yml (nightly, on a tag push, or a dispatch) uploads the artifact
# `drill-logs` with drill-result.json in it; the newest artifact for this commit whose result says commit ==
# SHA, profile kind, verdict pass, every required check passing and the browser sign-in on is the evidence.
# The artifact must come from a run of the tag itself (the tag push, or a dispatch on the tag ref: head_branch
# is the tag). Only that CI artifact is read, never a release asset or a result someone uploaded. Known gaps (ADR 0109
# decision 3) do not turn the verdict of an rc red; they are printed. A final tag allows none. Everything that
# cannot be read fails closed.
DRILL_ARTIFACT_LIMIT=${DRILL_ARTIFACT_LIMIT:-30}
[[ $DRILL_ARTIFACT_LIMIT =~ ^([1-9][0-9]?|100)$ ]] ||
	fail "DRILL_ARTIFACT_LIMIT '$DRILL_ARTIFACT_LIMIT' is not an integer from 1 to 100"
# The checks a result must carry as `pass`, by name (ADR 0085 decision 15: sign in, a provider key decrypts, a
# schedule fires, a pending workflow recovers, an archived agent restores; ADR 0109: the browser sign-in), so that
# a removed or renamed spec cannot pass silently. A check missing, repeated (whatever the verdicts of the copies) or not `pass` fails the gate.
DRILL_REQUIRED_CHECKS='["signin","provider-key","schedule","workflow-dead-executor","archive","browser-signin"]'

# drill_verdict <file> <commit> <final: 0|1>: prints why the result is not green (and returns 1), or prints
# the known gaps (possibly none) and returns 0. A result with no check, or with a check that is neither
# pass nor known-gap, is never green, whatever its verdict field says. For a final tag (1) a known gap is red.
drill_verdict() {
	jq -rs --arg c "$2" --argjson final "$3" --argjson req "$DRILL_REQUIRED_CHECKS" '
		def gaps: [(.checks // [])[] | select(.verdict == "known-gap")];
		def missing: . as $r | [$req[] | . as $n | select(([$r.checks[]? | select(.name == $n)] | length) != 1 or ([$r.checks[]? | select(.name == $n and .verdict == "pass")] | length) != 1)];
		def verdict: if type != "object" then "not a drill-result.json object"
		elif .commit != $c then "it is for commit \(.commit // "none"), not \($c)"
		elif .topology.profile != "kind" then "its profile is \(.topology.profile // "none"), not kind"
		elif .verdict != "pass" then "its verdict is \(.verdict // "none")"
		elif ((.checks // []) | length) == 0 then "it has no check"
		elif ((.checks // []) | map(select(.verdict != "pass" and .verdict != "known-gap")) | length) > 0 then "a check is neither pass nor known-gap"
		elif (missing | length) > 0 then "required checks not passing exactly once: " + (missing | join(", "))
		elif .topology.browserSignin != true then "its topology has no browserSignin"
		elif $final == 1 and (gaps | length) > 0 then "a final tag allows no known gap: " + ([gaps[] | (.issue // "?") + " " + .name] | join(", "))
		else "OK known gaps: " + (([.knownGaps[]? | (.issue | tostring) + " " + .name] | join(", ")) | if . == "" then "none" else . end) end;
		if length != 1 then "not exactly one result in the file" else (.[0] | verdict) end' "$1" 2>/dev/null |
		awk 'BEGIN { rc = 1 } NR == 1 && /^OK / { sub(/^OK /, ""); rc = 0 } { print; n++ } END { if (n == 0) { print "unreadable drill-result.json"; rc = 1 } exit (n == 1 ? rc : 1) }'
}

# kind_drill: ok when a green kind drill result for SHA is among the drill-logs artifacts (a final tag also
# allows no known gap).
kind_drill() {
	local rows id why='' seen=0 dir final=0
	[[ $TAG == *-rc.* ]] || final=1
	dir=$(mktemp -d "${TMPDIR:-/tmp}/release-gate-drill.XXXXXX") || return 2
	rows=$(gh api "repos/$GITHUB_REPOSITORY/actions/artifacts?name=drill-logs&per_page=$DRILL_ARTIFACT_LIMIT" \
		--jq ".artifacts[] | select(.expired | not) | select(.workflow_run.head_sha == \"$SHA\" and .workflow_run.head_branch == \"$TAG\" and .workflow_run.head_repository_id == .workflow_run.repository_id) | .id") || { rm -rf "$dir"; return 2; }
	for id in $rows; do
		seen=1
		gh api "repos/$GITHUB_REPOSITORY/actions/artifacts/$id/zip" >"$dir/a.zip" 2>/dev/null || { why="artifact $id could not be downloaded"; continue; }
		unzip -p "$dir/a.zip" '*drill-result.json' >"$dir/r.json" 2>/dev/null || { why="artifact $id has no drill-result.json (the suite did not finish)"; continue; }
		if why=$(drill_verdict "$dir/r.json" "$SHA" "$final"); then
			echo "ok: kind restore drill (artifact $id; $why)"
			# The release workflow attaches the evidence it just judged to the release (#928).
			if [[ -n ${DRILL_RESULT_OUT:-} ]]; then cp "$dir/r.json" "$DRILL_RESULT_OUT" || return 2; fi
			rm -rf "$dir"
			return 0
		fi
		why="artifact $id: $why"
	done
	rm -rf "$dir"
	if ((seen == 0)); then
		echo "FAIL: restore drill on kind: no drill-logs artifact for $SHA among the newest $DRILL_ARTIFACT_LIMIT; run the e2e-kind workflow with suite=drill on the ref $TAG (the drill of a tag push takes about half an hour: re-run the release gate job once it is green, or dispatch the drill on the tag ref; a dispatch on a branch or with -f commit is not found)" >&2
	else
		echo "FAIL: restore drill on kind: no green result for $SHA (last: $why)" >&2
	fi
	return 1
}

drc=0
kind_drill || drc=$?
case $drc in
0) ;;
1) failed=1 ;;
*)
	echo "FAIL: restore drill: an API call failed" >&2
	failed=1
	;;
esac
((failed == 0)) || fail "required checks have not all succeeded on $SHA"
echo "release gate passed for $TAG at $SHA"
