#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Git identity guard (koinon#320): a commit, a merge commit included, carries an author and a
# committer that appear in the allowed identity list, never a personal name picked up from a global
# git config.
#
#   hack/git-identity-check.sh commit           the identity git would use for the next commit
#                                               (what the pre-commit and pre-merge-commit hooks run)
#   hack/git-identity-check.sh range <base> <head>
#                                               author and committer of every commit in base..head,
#                                               merge commits included (what CI runs on a pull request)
#   hack/git-identity-check.sh install          make this worktree run hack/githooks (hack/wt.sh new does it)
#
# The allowed list is hack/git-identities.allowed: one `Name <email>` glob per line (`#` comments).
# KOINON_ALLOWED_IDENTITIES (newline separated, same syntax) replaces the file in commit mode (not in
# range mode). In range mode the list is read from the base commit, so a pull request cannot widen its
# own list; a person commits under their GitHub noreply address, which the list already allows; only a maintainer
# adds a line, and it counts from the next pull request on (KOINON_ALLOWED_IDENTITIES sets a list locally for the hook). Exit 0 when every
# identity is allowed, 1 when one is not, 2 on misuse or when the commits cannot be listed (fails closed).
# KOINON_SKIP_IDENTITY_CHECK=1 turns the hook off for one command (CI and hack/ship.sh ignore it).
# The hooks chain to a personal hook of the same name in the common .git/hooks (core.hooksPath replaces it).
# They do not run on --no-verify, rebase or cherry-pick; CI checks every commit of a pull request.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

usage() {
	sed -n '4,13p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
	exit 2
}

# patterns: prints the allowed `Name <email>` globs, comments and blank lines removed.
patterns() {
	local src line
	if [[ $mode == range ]]; then
		src=$range_list
	elif [[ -n ${KOINON_ALLOWED_IDENTITIES:-} ]]; then
		src=$KOINON_ALLOWED_IDENTITIES
	else
		[[ -r $here/git-identities.allowed ]] || {
			echo "git-identity-check: $here/git-identities.allowed is missing" >&2
			return 1
		}
		src=$(<"$here/git-identities.allowed")
	fi
	while IFS= read -r line; do
		line=${line%%#*}
		line=${line#"${line%%[![:space:]]*}"}
		line=${line%"${line##*[![:space:]]}"}
		[[ -n $line ]] && printf '%s\n' "$line"
	done <<<"$src"
	return 0
}

# allowed <"Name <email>">: succeeds when the identity matches a pattern.
allowed() {
	local ident=$1 p
	while IFS= read -r p; do
		# shellcheck disable=SC2053 # the right side is a glob on purpose
		[[ $ident == $p ]] && return 0
	done <<<"$pats"
	return 1
}

mode=${1:-}
case $mode in
commit | range | install) ;;
*) usage ;;
esac

if [[ $mode == install ]]; then
	[[ $# -eq 1 ]] || usage
	top=$(git rev-parse --show-toplevel) || exit 2
	[[ -d $top/hack/githooks ]] || { echo "git-identity-check: $top/hack/githooks not found" >&2; exit 2; }
	# Per worktree, so a linked worktree does not change the main checkout's hooks. Relative paths
	# resolve against the worktree top, which every worktree of this branch layout shares.
	if ! git -C "$top" config --worktree core.hooksPath hack/githooks 2>/dev/null; then
		git -C "$top" config extensions.worktreeConfig true || exit 2
		git -C "$top" config --worktree core.hooksPath hack/githooks || exit 2
	fi
	echo "git-identity-check: hooks installed in $top (core.hooksPath=hack/githooks)"
	exit 0
fi

if [[ $mode == range ]]; then
	# CI: the list comes from the pull request's BASE commit, so a pull request cannot widen the list it
	# is checked against (a new contributor's line counts from the next pull request on). The one
	# exception: when the base has no list at all and the head adds the file, the head's list is used
	# (the pull request that introduces the list). That fallback is only for a base that does not have the file
	# (e.g. a release/* cut before #320); a base that has the file always wins.
	# KOINON_ALLOWED_IDENTITIES is not honoured here.
	[[ $# -eq 3 ]] || usage
	base=$2 head=$3
	if ! commits=$(git rev-list "$base..$head"); then
		echo "::error::git rev-list $base..$head failed (see its error above), cannot check commit identities"
		exit 2
	fi
	if range_list=$(git show "$base:hack/git-identities.allowed" 2>/dev/null); then
		:
	elif ! git cat-file -e "$base:hack/git-identities.allowed" 2>/dev/null &&
		range_list=$(git show "$head:hack/git-identities.allowed" 2>/dev/null); then
		echo "git-identity-check: the base has no hack/git-identities.allowed; using the head's (newly added) list"
	else
		echo "::error::hack/git-identities.allowed is missing at both $base and $head, cannot check commit identities"
		exit 2
	fi
fi

pats=$(patterns) || exit 2
[[ -n $pats ]] || { echo "git-identity-check: the allowed identity list is empty" >&2; exit 2; }

if [[ $mode == commit ]]; then
	[[ $# -eq 1 ]] || usage
	if [[ ${KOINON_SKIP_IDENTITY_CHECK:-} == 1 && -z ${CI:-} ]]; then
		exit 0
	fi
	fail=0
	for role in AUTHOR COMMITTER; do
		ident=$(git var "GIT_${role}_IDENT") || {
			echo "git-identity-check: git has no ${role,,} identity (set user.name and user.email)" >&2
			exit 1
		}
		ident=${ident% * *} # drop the timestamp and the zone
		if ! allowed "$ident"; then
			echo "git-identity-check: the ${role,,} identity '$ident' is not in the allowed list." >&2
			fail=1
		fi
	done
	if [[ $fail -ne 0 ]]; then
		echo "A person: set your GitHub noreply address in this repository (the ID and username are on https://github.com/settings/emails):" >&2
		echo "  git config user.name \"<your name>\"; git config user.email \"<id>+<username>@users.noreply.github.com\"" >&2
		echo "An agent: use the project identity, or the noreply address of the human it works for. Only a maintainer adds lines to" >&2
		echo "hack/git-identities.allowed (see CONTRIBUTING.md, \"Commit identity\"). Allowed:" >&2
		printf '  %s\n' "${pats//$'\n'/$'\n'  }" >&2
	fi
	exit $fail
fi

# range
fail=0
while read -r c; do
	[[ -z $c ]] && continue
	for role in a c; do
		ident=$(git show -s --format="%${role}n <%${role}e>" "$c")
		if ! allowed "$ident"; then
			label=author
			[[ $role == c ]] && label=committer
			echo "::error::commit $(git show -s --format='%h' "$c") has $label '$ident', which is not in hack/git-identities.allowed"
			fail=1
		fi
	done
done <<<"$commits"
if [[ $fail -ne 0 ]]; then
	echo "Allowed identities (hack/git-identities.allowed):"
	printf '  %s\n' "${pats//$'\n'/$'\n'  }"
	echo "Fix with: set user.name to your name and user.email to your GitHub noreply address (<id>+<username>@users.noreply.github.com), then git commit --amend --reset-author for an unpushed last commit; a pushed commit needs a history rewrite and a force-push: ask your human. Only a maintainer adds lines to hack/git-identities.allowed."
fi
exit $fail
