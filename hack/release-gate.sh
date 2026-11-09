#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The release gate (plan item REL1): refuses a release unless the tag is a SemVer release tag, the
# tagged commit is on main, and the required CI checks concluded success on that commit. It does
# not run any tests. The release workflow runs it as its own job (`gate`), so it can later be
# swapped for a "local checks recorded" gate without touching the release job.
#
# Environment: TAG (v1.2.3 or v1.2.3-rc.1), SHA (the tagged commit), GITHUB_REPOSITORY and GH_TOKEN
# (needs checks:read, contents:read, statuses:read and pull-requests:read; the ruleset is read with the implicit
# metadata permission). The required checks are read from the `main` ruleset; the pull-request-only
# ones (pr_only below) are read from the head commit of the pull request that merged the tagged
# commit. Needs gh, git and awk, and a clone with origin/main.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
: "${TAG:?}" "${SHA:?}" "${GITHUB_REPOSITORY:?}" "${GH_TOKEN:?}"

# The required status checks come from GitHub at run time (required_contexts below), so a check
# added to or dropped from the ruleset needs no edit here.
#
# Required contexts that are not reported by a push to main, read instead from the head commit of
# the pull request that merged the tagged commit (the newest run that started before the merge):
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

failed=0
for name in "${required[@]}"; do
	if is_pr_only "$name"; then
		got=$(from_pr_heads "$name")
		where="on the merged pull request's head"
	else
		got=$(conclusion_of "$SHA" "$name")
		where="on $SHA"
		# A push to main skips the heavy jobs when the merge tree equals a pull request head whose
		# required checks passed (hack/ci-skip-heavy.sh, old issue 595); they report `skipped` here. Then the
		# merged pull request's head is the evidence, and it must have passed (or skipped) them.
		if [[ $got == skipped ]]; then
			got=$(from_pr_heads "$name")
			[[ $got == skipped ]] && got=success
			where="on the merged pull request's head (skipped on $SHA: the merge tree is that head's tree)"
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
# go-test (old issue 610): the job is skipped when the pull request head had a green external go-test (the commit status
# go-test-lab), and `go` accepts that. A go-test that was skipped is therefore checked once more: on the
# merged pull request's head it must have succeeded, or have been skipped with a green go-test-lab status
# on that head, or the change must touch nothing the go-* jobs run for. A go-test with no run at all is
# the business of `go` above. Needs statuses:read.
got=$(conclusion_of "$SHA" go-test)
if [[ $got == skipped ]]; then
	from_pr_heads go-test >/dev/null # called directly (not in $(...)) so that pr_heads is filled here
	got=
	lab=
	while IFS=$'\t' read -r h merged; do
		[[ -n $h ]] || continue
		got=$(conclusion_of "$h" go-test "$merged")
		if [[ -n $got ]]; then
			[[ $got != skipped ]] || lab=$("$here/go-test-lab.sh" state "$h" 2>/dev/null || true)
			break
		fi
	done <<<"$pr_heads"
	tg=0
	case $got in
	success) echo "ok: go-test (on the merged pull request's head)" ;;
	skipped)
		if [[ $lab == success ]]; then
			echo "ok: go-test (passed in an external run: go-test-lab on the merged pull request's head)"
		else
			"$here/go-test-lab.sh" touches-go "$SHA^1" "$SHA" || tg=$?
			if [[ $tg == 1 ]]; then
				echo "ok: go-test (skipped: the change touches no Go)"
			else
				echo "FAIL: go-test: skipped on the merged pull request's head with no green go-test-lab status (${lab:-none}) and the change may touch Go" >&2
				failed=1
			fi
		fi
		;;
	*)
		echo "FAIL: go-test: ${got:-no check run found} on the merged pull request's head (skipped on $SHA)" >&2
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
	# workflow runs on SHA are read once (one page of 100; a `total_count` above the runs returned fails closed) and a check run whose run is titled like a bisect
	# dispatch (`e2e-kind (<commit>) bisect <id>`, the run-name of e2e-kind.yml) is dropped as well.
	local bisect counts total n
	# The first line is `total_count runs_returned`, then one id per bisect run. One page is read (no
	# paging): if more runs attach to SHA than the page holds, a bisect run could hide beyond it, so stop.
	bisect=$(gh api "repos/$GITHUB_REPOSITORY/actions/workflows/e2e-kind.yml/runs?head_sha=$SHA&per_page=100" \
		--jq '"\(.total_count // 0) \(.workflow_runs | length)", (.workflow_runs[] | select((.display_title // "") | test("^e2e-kind \\(.+\\) bisect ")) | .id)') || return 2
	counts=$(head -n1 <<<"$bisect")
	read -r total n <<<"$counts"
	[[ $total =~ ^[0-9]+$ && $n =~ ^[0-9]+$ ]] || return 2
	if ((total > n)); then
		echo "release gate: more than 100 e2e-kind runs on $SHA: cannot rule out a bisect run" >&2
		return 2
	fi
	bisect=$(tail -n +2 <<<"$bisect")
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
