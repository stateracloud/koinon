#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The release gate (plan item REL1): refuses a release unless the tag is a SemVer release tag, the
# tagged commit is on main, and the required CI checks concluded success on that commit. It does
# not run any tests. The release workflow runs it as its own job (`gate`), so it can later be
# swapped for a "local checks recorded" gate without touching the release job.
#
# Environment: TAG (v1.2.3 or v1.2.3-rc.1), SHA (the tagged commit), GITHUB_REPOSITORY and GH_TOKEN
# (needs checks:read and contents:read). Needs gh, git and awk, and a clone with origin/main.
set -euo pipefail

: "${TAG:?}" "${SHA:?}" "${GITHUB_REPOSITORY:?}" "${GH_TOKEN:?}"

# The check names of the `main` ruleset's required status checks. Keep in step with the ruleset.
required=(dco go web proto migrations "generated code" gitleaks chart "escape (admission)")
# Checks whose workflow runs on pull requests only: they are looked up on the head commit of the
# pull request that merged the tagged commit when the tagged commit has none.
pr_only=(dco)

fail() { echo "release gate: $*" >&2; exit 1; }

[[ $TAG =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$ ]] ||
	fail "tag '$TAG' is not vX.Y.Z or vX.Y.Z-rc.N"

git fetch --no-tags --quiet origin main
git merge-base --is-ancestor "$SHA" origin/main ||
	fail "$SHA is not an ancestor of origin/main; release from a commit that has been merged"

# conclusion_of <sha> <name>: the conclusion of the newest check run of that name, or empty.
conclusion_of() {
	gh api --paginate "repos/$GITHUB_REPOSITORY/commits/$1/check-runs?per_page=100" \
		--jq '.check_runs[] | [.name, .started_at, (.conclusion // "pending")] | @tsv' |
		awk -F'\t' -v n="$2" '$1 == n && $2 >= t { t = $2; c = $3 } END { print c }'
}

pr_heads=
failed=0
for name in "${required[@]}"; do
	got=$(conclusion_of "$SHA" "$name")
	if [[ -z $got ]]; then
		for p in "${pr_only[@]}"; do
			[[ $p == "$name" ]] || continue
			if [[ -z $pr_heads ]]; then
				pr_heads=$(gh api "repos/$GITHUB_REPOSITORY/commits/$SHA/pulls" \
					--jq '.[] | select(.merged_at != null) | .head.sha')
			fi
			for h in $pr_heads; do
				got=$(conclusion_of "$h" "$name")
				[[ -n $got ]] && break
			done
		done
	fi
	if [[ $got == success ]]; then
		echo "ok: $name"
	else
		echo "FAIL: $name: ${got:-no check run found}" >&2
		failed=1
	fi
done
((failed == 0)) || fail "required checks have not all succeeded on $SHA"
echo "release gate passed for $TAG at $SHA"
