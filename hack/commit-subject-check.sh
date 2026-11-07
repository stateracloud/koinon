#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Conventional Commits subject check over a pull request's commits (CONTRIBUTING, AGENTS.md
# "Conventions"). Usage: hack/commit-subject-check.sh <base> <head>
#
# Checks every non-merge commit of <base>..<head> (same loop and fix-it message as the dco workflow).
# Exempt: merge commits (--no-merges), commits authored by `dependabot[bot]` with a
# `@users.noreply.github.com` email, and the subject `git revert` writes (`Revert "..."`).
# Exit 0 when every subject conforms, 1 when one does not, 2 on misuse or when the commits cannot be
# listed (fails closed).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

if [[ $# -ne 2 ]]; then
	echo "usage: $0 <base> <head>" >&2
	exit 2
fi
base=$1 head=$2

re='^(feat|fix|docs|chore|ci|test|refactor|build|perf|style|revert)(\([^)]*\))?!?: .'

# List the commits first, apart from the loop: a failing rev-list (bad range, missing object) must
# fail the check, not leave an empty list that passes. Its stderr goes to the log as it is, never into
# the list (a warning such as "refname is ambiguous" must not be read as a commit).
if ! commits=$(git rev-list --no-merges "$base..$head"); then
	echo "::error::git rev-list --no-merges $base..$head failed (see its error above), cannot check commit subjects"
	exit 2
fi

fail=0
while read -r c; do
	[[ -z $c ]] && continue
	name=$(git show -s --format='%an' "$c")
	email=$(git show -s --format='%ae' "$c")
	if [[ $name == "dependabot[bot]" && $email == *@users.noreply.github.com ]]; then
		continue
	fi
	subject=$(git show -s --format='%s' "$c")
	if [[ $subject == 'Revert "'* ]]; then
		continue
	fi
	if ! [[ $subject =~ $re ]]; then
		echo "::error::commit $(git show -s --format='%h' "$c") subject is not a Conventional Commit: '$subject'"
		fail=1
	fi
done <<<"$commits"

if [[ $fail -ne 0 ]]; then
	echo "Expected 'type(scope): summary' with type one of feat, fix, docs, chore, ci, test, refactor, build, perf, style, revert (scope optional, '!' marks a breaking change)."
	echo "Fix with: git commit --amend for an unpushed last commit; a pushed commit needs a history rewrite and a force-push: ask your human. Merge commits are exempt."
fi
exit $fail
