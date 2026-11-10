#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The release gate (plan item REL1): refuses a release unless the tag is a SemVer release tag, the
# tagged commit is on main, and the required CI checks concluded success on that commit. It does
# not run any tests. The release workflow runs it as its own job (`gate`), so it can later be
# swapped for a "local checks recorded" gate without touching the release job.
#
# Environment: TAG (v1.2.3 or v1.2.3-rc.1), SHA (the tagged commit), GITHUB_REPOSITORY and GH_TOKEN
# (needs checks:read, contents:read, actions:read and pull-requests:read; the ruleset is read with the implicit
# metadata permission). The required checks are read from
# the `main` ruleset, in this order (ADR 0104-era route, docs/dev/releasing.md): the tagged commit itself;
# where it reports `skipped` (the push skipped its heavy jobs because the merge queue had tested the
# tree) or, for the pull-request-only contexts (pr_only below), the successful merge_group `ci` run on a
# commit with the same tree (the queue build); and only when there is no queue run (an admin merge) the
# head commit of the pull request that merged the tagged commit, for the pull-request-only contexts and
# the dependency-age half of `quick checks`. A `go`/`web`/other ci job is never read from a PR head: a
# draft's green is vacuous. Needs gh, git and awk, and a clone with origin/main.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
: "${TAG:?}" "${SHA:?}" "${GITHUB_REPOSITORY:?}" "${GH_TOKEN:?}"

# The required status checks come from GitHub at run time (required_contexts below), so a check
# added to or dropped from the ruleset needs no edit here.
#
# Required contexts that are not reported by a push to main, read instead from the queue build (the
# merge_group run on the same tree), else from the head commit of the pull request that merged the
# tagged commit (the newest run that started before the merge; an admin merge has no queue build):
#   dco                  its workflow runs on pull_request only.
#   claim                runs on pull_request (hack/claim.sh check-pr, about the PR and its
#                        claim); on a push to main it is skipped.
#   e2e result           succeeds on main pushes too, but it is a composite that passes when the
#                        batched e2e was skipped; the real subject is the `quickstart` rule below,
#                        which demands success on the very commit.
# This is a list, not "any context with no run on the commit", so a new push-triggered required
# check that is missing or red on the commit fails the gate instead of being excused by a PR run.
# A context not listed here must succeed on the tagged commit itself. When a workflow gains or
# loses a push trigger, move its context between the two cases.
pr_only=(dco claim "e2e result")
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
# <sha> that the Actions app (integration 15368) reported for one of those workflow runs (the run id is in
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

contexts=$(required_contexts) ||
	fail "cannot read the rules that apply to the default branch (gh api repos/$GITHUB_REPOSITORY/rules/branches/<default> failed); refusing to release without the required list"
[[ -n $contexts ]] ||
	fail "no required status checks apply to the default branch; refusing to release without the required list"
mapfile -t required <<<"$contexts"

is_both() { local p; for p in "${both[@]}"; do [[ $p == "$1" ]] && return 0; done; return 1; }
is_pr_only() { local p; for p in "${pr_only[@]}"; do [[ $p == "$1" ]] && return 0; done; return 1; }

pr_heads=
pr_heads_read=0
# from_pr_heads <name>: the conclusion of the check on the head of the pull request that merged SHA.
from_pr_heads() {
	local h merged got=
	if ((pr_heads_read == 0)); then
		pr_heads=$(gh api "repos/$GITHUB_REPOSITORY/commits/$SHA/pulls" \
			--jq '.[] | select(.merged_at != null) | [.head.sha, .merged_at] | @tsv') ||
			fail "cannot list the pull requests of $SHA"
		pr_heads_read=1
	fi
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
# go-test: skipped on the tagged commit means the merge queue ran it (a merge_group run never skips it for a
# lab status, only for a change with no Go in it). It is checked once more: on the queue build it must have
# succeeded, or the change must touch nothing the go-* jobs run for. A go-test with no run at all is the
# business of `go` above.
got=$(conclusion_of "$SHA" go-test)
if [[ $got == skipped ]]; then
	queue_ci_head
	got=
	[[ -z $qci_head ]] || got=$(conclusion_in_run "$qci_head" go-test "$qci_runs")
	tg=0
	case $got in
	success) echo "ok: go-test (in the merge queue run $qci_head)" ;;
	skipped | "")
		"$here/go-test-lab.sh" touches-go "$SHA^1" "$SHA" || tg=$?
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
((failed == 0)) || fail "required checks have not all succeeded on $SHA"
echo "release gate passed for $TAG at $SHA"
