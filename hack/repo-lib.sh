# shellcheck shell=bash
# SPDX-License-Identifier: Apache-2.0
#
# The one place the hack/ scripts learn which GitHub repository and project board they work on (#846).
# No other script names the org: a fork runs the same scripts against itself. Source it, do not run it.
#
#   koinon_upstream   owner/name of the upstream repository, read from the `module github.com/<owner>/<name>`
#                     line of this checkout's go.mod (the module path of internal/project, AGENTS.md).
#                     KOINON_UPSTREAM overrides it.
#   koinon_repo       the repository the scripts act on: KOINON_REPO, else GH_REPO (gh's own variable), else
#                     the remote gh itself would pick in the current directory's checkout (the one
#                     `gh repo set-default` chose, else `upstream`, `github`, `origin`), else koinon_upstream. So
#                     the scripts and their plain `gh pr` calls agree: a fork cloned with an `upstream`
#                     remote acts on the upstream, a fork cloned alone acts on the fork.
#   koinon_board      prints "<owner> <number>" of the project board, or nothing when there is none.
#                     KOINON_BOARD is the number, or `none`; its default is 1 on the upstream repository and
#                     none elsewhere (a fork has no board). KOINON_BOARD_OWNER defaults to koinon_repo's owner.
#   koinon_is_upstream  exit 0 when koinon_repo is koinon_upstream.
#   koinon_use_repo   exports GH_REPO=$KOINON_REPO when KOINON_REPO is set, so the script's plain `gh pr`
#                     and `gh api` calls act on the same repository; without KOINON_REPO it changes nothing.
#
# Every function prints to stdout and needs only git and the shell; none of them calls GitHub.

# The go.mod next to this file's checkout (hack/repo-lib.sh -> go.mod), not the current directory's: a
# script run from a test checkout still knows its own upstream.
_koinon_repo_lib_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

koinon_upstream() {
	if [[ -n ${KOINON_UPSTREAM:-} ]]; then
		printf '%s\n' "$KOINON_UPSTREAM"
		return
	fi
	local mod
	mod=$(sed -n 's|^module github\.com/\([^/[:space:]]*/[^/[:space:]]*\).*|\1|p' "$_koinon_repo_lib_root/go.mod" 2>/dev/null | head -n 1)
	[[ -n $mod ]] || {
		echo "repo-lib: cannot read the upstream repository from $_koinon_repo_lib_root/go.mod (set KOINON_UPSTREAM)" >&2
		return 1
	}
	printf '%s\n' "$mod"
}

# koinon_github_slug URL: owner/name of a GitHub remote URL (https, ssh or scp form), or nothing.
koinon_github_slug() {
	local u=${1%/}
	u=${u%.git}
	[[ $u =~ ^(https?://|ssh://)?([^@/]+@)?github\.com[:/]([^/]+)/([^/]+)$ ]] || return 0
	printf '%s/%s\n' "${BASH_REMATCH[3]}" "${BASH_REMATCH[4]}"
}

koinon_repo() {
	local r=${KOINON_REPO:-${GH_REPO:-}}
	local remote
	if [[ -z $r ]]; then
		# `gh repo set-default` stores `base` on the chosen remote, or the chosen owner/name when that is
		# not the remote's own repository.
		local resolved
		resolved=$(git config --get-regexp '^remote\..*\.gh-resolved$' 2>/dev/null | awk '$2 != "" {print $2; exit}')
		if [[ $resolved == */* ]]; then
			r=$resolved
		else
			for remote in "$(git config --get-regexp '^remote\..*\.gh-resolved$' 2>/dev/null | awk '$2 == "base" {sub(/^remote\./, "", $1); sub(/\.gh-resolved$/, "", $1); print $1; exit}')" upstream github origin; do
				[[ -n $remote ]] || continue
				r=$(koinon_github_slug "$(git remote get-url "$remote" 2>/dev/null || true)")
				[[ -z $r ]] || break
			done
		fi
	fi
	[[ -n $r ]] || r=$(koinon_upstream) || return 1
	r=${r#https://github.com/}
	r=${r#github.com/}
	printf '%s\n' "$r"
}

koinon_use_repo() {
	[[ -z ${KOINON_REPO:-} ]] || export GH_REPO=$KOINON_REPO
}

koinon_is_upstream() {
	local r u
	r=$(koinon_repo) && u=$(koinon_upstream) || return 1
	[[ ${r,,} == "${u,,}" ]]
}

koinon_board() {
	local n=${KOINON_BOARD:-} owner=${KOINON_BOARD_OWNER:-} r
	if [[ -z $n ]]; then
		if koinon_is_upstream; then n=1; else n=none; fi
	fi
	[[ $n != none ]] || return 0
	[[ $n =~ ^[0-9]+$ ]] || {
		echo "repo-lib: KOINON_BOARD must be a project number or none, not $n" >&2
		return 1
	}
	if [[ -z $owner ]]; then
		r=$(koinon_repo) || return 1
		owner=${r%%/*}
	fi
	printf '%s %s\n' "$owner" "$n"
}
