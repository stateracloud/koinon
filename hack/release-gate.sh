#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The release gate (plan item REL1): refuses a release unless the tag is a SemVer release tag, the
# tagged commit is on main, and the required CI checks concluded success on that commit. It does
# not run any tests. The release workflow runs it as its own job (`gate`), so it can later be
# swapped for a "local checks recorded" gate without touching the release job.
#
# Environment: TAG (v1.2.3 or v1.2.3-rc.1), SHA (the tagged commit), GITHUB_REPOSITORY and GH_TOKEN
# (needs checks:read, contents:read and pull-requests:read; the ruleset is read with the implicit
# metadata permission). The required checks are read from the `main` ruleset; the pull-request-only
# ones (pr_only below) are read from the head commit of the pull request that merged the tagged
# commit. Needs gh, git and awk, and a clone with origin/main.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

: "${TAG:?}" "${SHA:?}" "${GITHUB_REPOSITORY:?}" "${GH_TOKEN:?}"

# The required status checks come from GitHub at run time (required_contexts below), so a check
# added to or dropped from the ruleset needs no edit here.
#
# Required contexts that are not reported by a push to main, read instead from the head commit of
# the pull request that merged the tagged commit (the newest run that started before the merge):
#   dco, dependency age  their workflows or jobs run on pull_request only (ci.yml gives
#                        `dependency age` `if: github.event_name == 'pull_request'`, so on main it is
#                        a skipped run). `dependency age` becomes required with #561; the gate
#                        expects it.
#   claim                runs on pull_request_target (hack/claim.sh check-pr, about the PR and its
#                        claim); on a push to main it is skipped.
#   e2e result           succeeds on main pushes too, but it is a composite that passes when the
#                        batched e2e was skipped; the real subject is the `quickstart` rule below,
#                        which demands success on the very commit.
# This is a list, not "any context with no run on the commit", so a new push-triggered required
# check that is missing or red on the commit fails the gate instead of being excused by a PR run.
# A context not listed here must succeed on the tagged commit itself. When a workflow gains or
# loses a push trigger, move its context between the two cases.
pr_only=(dco "dependency age" claim "e2e result")

fail() { echo "release gate: $*" >&2; exit 1; }

[[ $TAG =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$ ]] ||
	fail "tag '$TAG' is not vX.Y.Z or vX.Y.Z-rc.N"

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

is_pr_only() { local p; for p in "${pr_only[@]}"; do [[ $p == "$1" ]] && return 0; done; return 1; }

pr_heads=
pr_heads_read=0
failed=0
for name in "${required[@]}"; do
	if is_pr_only "$name"; then
		if ((pr_heads_read == 0)); then
			pr_heads=$(gh api "repos/$GITHUB_REPOSITORY/commits/$SHA/pulls" \
				--jq '.[] | select(.merged_at != null) | [.head.sha, .merged_at] | @tsv') ||
				fail "cannot list the pull requests of $SHA"
			pr_heads_read=1
		fi
		got=
		while IFS=$'\t' read -r h merged; do
			[[ -n $h ]] || continue
			got=$(conclusion_of "$h" "$name" "$merged")
			[[ -n $got ]] && break
		done <<<"$pr_heads"
		where="on the merged pull request's head"
	else
		got=$(conclusion_of "$SHA" "$name")
		where="on $SHA"
	fi
	if [[ $got == success ]]; then
		echo "ok: $name"
	else
		echo "FAIL: $name: ${got:-no check run found} $where" >&2
		failed=1
	fi
done
# The kind e2e is not a required check (it is batched on main, and `e2e result` passes when it is
# skipped), so a release asks for the `quickstart` job itself to have concluded success on this very
# commit. If main's batched run skipped or never reached it, dispatch e2e-kind on the commit first.
got=$(conclusion_of "$SHA" quickstart)
if [[ $got == success ]]; then
	echo "ok: kind e2e (quickstart)"
else
	echo "FAIL: kind e2e (quickstart job): ${got:-no run on this commit}; run the e2e-kind workflow on $SHA" >&2
	failed=1
fi
((failed == 0)) || fail "required checks have not all succeeded on $SHA"
echo "release gate passed for $TAG at $SHA"
