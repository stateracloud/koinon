#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Short-lived git worktrees under one root, so agents do not carry long paths or worktree plumbing.
# The root is ${KOINON_WT_ROOT:-${TMPDIR:-/tmp}/kw}. Run it from any checkout of the repository; it
# never changes the checkout you run it from.
#
# Usage:
#   wt.sh new <name> [--from REF] [--branch BRANCH]
#       fetch origin, add a worktree at <root>/<name> from REF (default origin/main). With --branch it
#       creates that branch, otherwise HEAD is detached. Prints only the absolute path. It also
#       installs web/node_modules (pnpm install --frozen-lockfile --offline from the shared store),
#       because make generate needs it; when that fails it says how to fix it and still succeeds.
#       KOINON_WT_NO_INSTALL=1 skips the install.
#   wt.sh rm <name> [--branch] [--force]
#       remove the worktree (<name> is found through `git worktree list`, wherever $TMPDIR points:
#       rm, ls, gc and path manage every linked worktree that lives in a directory named kw, also one
#       created under another $TMPDIR, as a sandboxed and an unsandboxed shell resolve it differently).
#       Refuses, naming each check that failed, when: a live claim (claim/<N> on origin, N from a
#       branch named <type>/<N>-<slug>) is held on its branch; a process has its working directory in
#       it; it has uncommitted changes; or it has commits on no remote branch. --force overrides all
#       of them and prints what it overrides. --branch also deletes its local branch, never one with
#       an open PR. Remove worktrees only through this script, never `git worktree remove`.
#   wt.sh gc [--dry-run]
#       remove every worktree under the root with proof it is finished (its branch's PR is merged, or
#       its upstream is gone) that has no open PR and passes all the rm checks; a fresh or detached
#       worktree is kept. Prints one keep/remove line each; a removal that fails is reported, the
#       next worktree still runs, and the exit status is 1. Never forces.
#   wt.sh ls
#       one line per worktree: name branch clean|dirty +ahead/-behind (vs origin/main) #PR|-
#   wt.sh path <name>
#       print the worktree path (exit 1 when it does not exist)
#   KOINON_WT_NO_FETCH=1 skips the fetch of claim refs that rm and gc make before the claim check.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat

tmp=${TMPDIR:-/tmp}
root=${KOINON_WT_ROOT:-${tmp%/}/kw}
root=${root%/}

die() {
  echo "wt: $*" >&2
  exit 1
}
usage() {
  sed -n '/^# Usage:/,/^set /p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2
  exit 64
}

check_name() {
  case $1 in '' | */* | .* | *[!A-Za-z0-9._-]*) die "bad name: $1" ;; esac
}

open_pr() { # <branch>: prints the open PR number, if any
  gh pr list --head "$1" --state open --json number 2>/dev/null | jq -r '.[0].number // empty' 2>/dev/null || true
}

install_web() { # <dir>: web dependencies for make generate; never fails the worktree creation
	local dir=$1
	[ -z "${KOINON_WT_NO_INSTALL:-}" ] || return 0
	[ -f "$dir/web/pnpm-lock.yaml" ] || return 0
	if ! command -v pnpm >/dev/null 2>&1; then
		echo "wt: pnpm not installed; web/node_modules missing, so make generate will fail (install pnpm, then: cd $dir/web && pnpm install --frozen-lockfile)" >&2
		return 0
	fi
	if (cd "$dir/web" && pnpm install --frozen-lockfile --offline) >&2; then
		return 0
	fi
	echo "wt: offline pnpm install failed (store lacks a package); retrying online" >&2
	if (cd "$dir/web" && pnpm install --frozen-lockfile) >&2; then
		return 0
	fi
	echo "wt: web install failed; make generate will fail until: cd $dir/web && pnpm install --frozen-lockfile" >&2
}

cmd_new() {
  local name="" from=origin/main branch=""
  while [ $# -gt 0 ]; do
    case $1 in
    --from)
      [ $# -ge 2 ] || usage
      from=$2
      shift
      ;;
    --branch)
      [ $# -ge 2 ] || usage
      branch=$2
      shift
      ;;
    -*) usage ;;
    *)
      [ -z "$name" ] || usage
      name=$1
      ;;
    esac
    shift
  done
  check_name "$name"
  local dir=$root/$name
  [ ! -e "$dir" ] || die "$dir already exists"
  git fetch -q origin >&2 || die "git fetch origin failed"
  mkdir -p "$root"
  if [ -n "$branch" ]; then
    git worktree add -q -b "$branch" "$dir" "$from" >&2
  else
    git worktree add -q --detach "$dir" "$from" >&2
  fi
  # Commits and merge commits in a lane worktree must carry the project identity (koinon#320). A base
  # from before the hooks existed has no script; that is not an error.
  if [ -x "$dir/hack/git-identity-check.sh" ]; then
    (cd "$dir" && hack/git-identity-check.sh install) >&2 || echo "wt: could not install the identity hooks in $dir; run hack/git-identity-check.sh install there" >&2
  fi
  install_web "$dir"
  echo "$dir"
}

# is_managed <path>: succeeds for a worktree this script manages. $TMPDIR differs between a sandboxed
# and an unsandboxed shell (/private/tmp/claude-502 against /var/folders/.../T), so a worktree is
# managed when it is under the realpath of $root, or, when KOINON_WT_ROOT is not set, when its parent
# is exactly <temp base>/kw. A temp base is $TMPDIR, /tmp, /private/tmp, /private/tmp/claude-*,
# /var/folders/*/*/T (also under /private) or an entry of KOINON_WT_TMP_BASES (colon separated). A
# linked worktree elsewhere (.claude/worktrees/<name>, a human's checkout) is never managed.
is_managed() {
	local path=$1 real parent grand base
	real=$(cd "$path" 2>/dev/null && pwd -P) || return 1
	if [ -d "$root" ]; then
		case $real in "$(cd "$root" && pwd -P)"/*) return 0 ;; esac
	fi
	[ -z "${KOINON_WT_ROOT:-}" ] || return 1
	parent=$(dirname "$real")
	[ "$(basename "$parent")" = kw ] || return 1
	grand=$(dirname "$parent")
	case $grand in
	/tmp | /private/tmp | /private/tmp/claude-* | /var/folders/*/*/T | /private/var/folders/*/*/T) return 0 ;;
	esac
	local IFS=:
	for base in ${TMPDIR:-} ${KOINON_WT_TMP_BASES:-}; do
		[ -n "$base" ] && [ -d "$base" ] || continue
		[ "$(cd "$base" && pwd -P)" = "$grand" ] && return 0
	done
	return 1
}

# managed_wts: prints "<path><TAB><branch>" for every managed linked worktree, from
# `git worktree list --porcelain`; the main checkout is never listed. Branch is "(detached)" if none.
managed_wts() {
	local line path="" branch="(detached)" first=1
	while IFS= read -r line || [ -n "$line" ]; do
		case $line in
		"worktree "*) path=${line#worktree } ;;
		"branch "*) branch=${line#branch refs/heads/} ;;
		"")
			if [ -n "$path" ]; then
				if [ "$first" -eq 1 ]; then
					first=0
				elif is_managed "$path"; then
					printf '%s\t%s\n' "$path" "$branch"
				fi
			fi
			path="" branch="(detached)"
			;;
		esac
	done < <(git worktree list --porcelain; echo)
}

# wt_dir <name>: prints the path of the managed worktree named <name>. Returns 1 when there is none;
# returns 2 and prints every path (one per line) when the name is ambiguous.
wt_dir() {
	local name=$1 path branch n=0 found=""
	while IFS=$'\t' read -r path branch; do
		[ "$(basename "$path")" = "$name" ] || continue
		found+="$path"$'\n'
		n=$((n + 1))
	done < <(managed_wts)
	[ "$n" -ge 1 ] || return 1
	printf '%s' "$found"
	[ "$n" -eq 1 ] || return 2
}

# wt_dir_or_die <name>: wt_dir, but dies on none and on an ambiguous name.
wt_dir_or_die() {
	local out rc=0
	out=$(wt_dir "$1") || rc=$?
	case $rc in
	0) printf '%s\n' "$out" ;;
	1) die "no worktree $1" ;;
	*) die "ambiguous name $1: $(tr '\n' ' ' <<<"$out")" ;;
	esac
}

# claim_reason <branch>: prints a refusal reason when a live claim points at the branch. Claims are
# the refs claim/<issue> on origin (ADR 0082); the issue number is the one in <type>/<N>-<slug>. The
# claim ref is read from the fetched copy, no network call besides the optional fetch.
# The fetch runs once, in the calling shell (claim_reason itself runs in a command substitution);
# a failed fetch leaves claim_fetch_failed=1 and the check then reports unknown claim state.
claim_fetched=0
claim_fetch_failed=0
claim_fetch() {
	[ "$claim_fetched" -eq 0 ] || return 0
	claim_fetched=1
	[ -z "${KOINON_WT_NO_FETCH:-}" ] || return 0
	git fetch -q origin '+refs/heads/claim/*:refs/remotes/origin/claim/*' >/dev/null 2>&1 || claim_fetch_failed=1
}
claim_reason() {
	local branch=$1 n ref msg state holder lease now
	[[ $branch =~ ^[a-z]+/([0-9]+)- ]] || return 0
	n=${BASH_REMATCH[1]}
	if [ "$claim_fetch_failed" -eq 1 ]; then
		echo "could not fetch claim refs from origin; claim state of #$n unknown"
		return 0
	fi
	if ! command -v jq >/dev/null 2>&1; then
		echo "jq is not installed; claim state of #$n unknown"
		return 0
	fi
	for ref in "refs/remotes/origin/claim/$n" "refs/heads/claim/$n"; do
		git rev-parse -q --verify "$ref" >/dev/null 2>&1 || continue
		msg=$(git log -1 --format=%B "$ref" 2>/dev/null) || continue
		state=$(sed -n 's/^state: *//p' <<<"$msg" | head -n1)
		holder=$(sed -n 's/^holder: *//p' <<<"$msg" | head -n1)
		lease=$(sed -n 's/^lease_until: *//p' <<<"$msg" | head -n1)
		[ "$state" = held ] || continue
		now=$(date -u +%s)
		lease=$(jq -rn --arg l "$lease" '$l | fromdateiso8601' 2>/dev/null) || lease=""
		if ! [[ $lease =~ ^[0-9]+$ ]]; then
			echo "claim on #$n has a lease_until that does not parse; claim state of #$n unknown"
			return 0
		fi
		if [ "$now" -le "$lease" ]; then
			echo "a live claim on #$n is held by ${holder:-unknown} (lease until $(jq -rn --argjson e "$lease" '$e | todate'))"
			return 0
		fi
	done
}

# proc_reason <dir>: prints a refusal reason when a process other than this one has its working
# directory inside <dir>. Read-only: lsof, or /proc where there is no lsof.
proc_reason() {
	local dir real pid="" line cwd hits=""
	real=$(cd "$1" && pwd -P)
	dir=$1
	if command -v lsof >/dev/null 2>&1; then
		while IFS= read -r line; do
			case $line in
			p*) pid=${line#p} ;;
			n*)
				cwd=${line#n}
				case $cwd in "$real" | "$real"/* | "$dir" | "$dir"/*)
					[ "$pid" = "$$" ] || hits="$hits $pid"
					;;
				esac
				;;
			esac
		done < <(lsof -w -d cwd -Fpn 2>/dev/null || true)
	else
		for cwd in /proc/[0-9]*/cwd; do
			pid=${cwd#/proc/}
			pid=${pid%/cwd}
			line=$(readlink "$cwd" 2>/dev/null) || continue
			case $line in "$real" | "$real"/*) [ "$pid" = "$$" ] || hits="$hits $pid" ;; esac
		done
	fi
	[ -z "$hits" ] || echo "a process has its working directory inside (pid${hits})"
}

# guard <dir> <name>: fills reasons[] with every reason not to remove the worktree.
reasons=()
guard() {
	local dir=$1 name=$2 branch unpushed r
	reasons=()
	branch=$(git -C "$dir" symbolic-ref -q --short HEAD || true)
	if [ -n "$branch" ]; then
		claim_fetch
		r=$(claim_reason "$branch")
		[ -z "$r" ] || reasons+=("$r")
	fi
	r=$(proc_reason "$dir")
	[ -z "$r" ] || reasons+=("$r")
	[ -z "$(git -C "$dir" status --porcelain)" ] || reasons+=("$name has uncommitted changes")
	unpushed=$(git -C "$dir" rev-list --count HEAD --not --remotes)
	[ "$unpushed" -eq 0 ] || reasons+=("$name has $unpushed commit(s) on no remote branch")
}

# lint_clean <dir>: delete the worktree's golangci-lint lock and cache directory (hack/golangci-lint.sh,
# #236) while the worktree still exists, since its key is the worktree root. Never fails.
lint_clean() {
	[ -x "$1/hack/golangci-lint.sh" ] || return 0
	(cd "$1" && hack/golangci-lint.sh --clean) >/dev/null 2>&1 || true
}

remove_wt() { # <dir> <name> <force> <delbranch>
	local dir=$1 name=$2 force=$3 delbranch=$4 branch="" r
	guard "$dir" "$name"
	if [ "${#reasons[@]}" -gt 0 ]; then
		if [ "$force" -eq 0 ]; then
			for r in "${reasons[@]}"; do echo "wt: refusing to remove $name: $r" >&2; done
			echo "wt: --force overrides these checks" >&2
			return 1
		fi
		for r in "${reasons[@]}"; do echo "wt: --force overrides: $r" >&2; done
	fi
	branch=$(git -C "$dir" symbolic-ref -q --short HEAD || true)
	lint_clean "$dir"
	# Every step checks its status: callers run this left of `||`, where set -e is off.
	if [ "$force" -eq 1 ]; then
		git worktree remove --force "$dir" || return 1
	else
		git worktree remove "$dir" || return 1
	fi
	if [ "$delbranch" -eq 1 ] && [ -n "$branch" ]; then
		case $branch in
		main | master) echo "wt: not deleting branch $branch" >&2 ;;
		*)
			local pr
			pr=$(open_pr "$branch")
			if [ -n "$pr" ]; then
				echo "wt: kept branch $branch (open PR #$pr)" >&2
			else
				git branch -q -D "$branch" || return 1
			fi
			;;
		esac
	fi
	return 0
}

cmd_rm() {
	local name="" delbranch=0 force=0
	while [ $# -gt 0 ]; do
		case $1 in
		--branch) delbranch=1 ;;
		--force) force=1 ;;
		-*) usage ;;
		*)
			[ -z "$name" ] || usage
			name=$1
			;;
		esac
		shift
	done
	check_name "$name"
	local dir
	dir=$(wt_dir_or_die "$name") || exit 1
	remove_wt "$dir" "$name" "$force" "$delbranch" || exit 1
}

# gc_finished <dir>: succeeds only on positive proof the work is finished: the branch's PR is merged,
# or its upstream is gone (the remote branch existed and was deleted). A HEAD that is merely an
# ancestor of origin/main (a fresh `wt new`) or a detached HEAD proves nothing, so it is kept.
gc_finished() {
	local dir=$1 branch track
	branch=$(git -C "$dir" symbolic-ref -q --short HEAD || true)
	[ -n "$branch" ] || return 1
	track=$(git -C "$dir" for-each-ref --format='%(upstream:track)' "refs/heads/$branch")
	[ "$track" = "[gone]" ] && return 0
	[ -n "$(gh pr list --head "$branch" --state merged --json number 2>/dev/null | jq -r '.[0].number // empty' 2>/dev/null || true)" ]
}

cmd_gc() {
	local dry=0 path="" name branch pr r rc=0
	while [ $# -gt 0 ]; do
		case $1 in
		--dry-run) dry=1 ;;
		*) usage ;;
		esac
		shift
	done
	local paths=()
	while IFS=$'\t' read -r path _; do paths+=("$path"); done < <(managed_wts)
	for path in ${paths[@]+"${paths[@]}"}; do
		name=$(basename "$path")
		branch=$(git -C "$path" symbolic-ref -q --short HEAD || true)
		if [ -n "$branch" ]; then
			pr=$(open_pr "$branch")
			if [ -n "$pr" ]; then
				echo "keep $name: open PR #$pr"
				continue
			fi
		fi
		if ! gc_finished "$path"; then
			echo "keep $name: branch is not merged or gone"
			continue
		fi
		guard "$path" "$name"
		if [ "${#reasons[@]}" -gt 0 ]; then
			echo "keep $name: ${reasons[0]}"
			continue
		fi
		if [ "$dry" -eq 1 ]; then
			echo "would remove $name"
		else
			lint_clean "$path"
			if git worktree remove "$path"; then
				echo "removed $name"
			else
				echo "keep $name: git worktree remove failed"
				rc=1
			fi
		fi
	done
	return "$rc"
}

cmd_ls() {
	local path branch
	while IFS=$'\t' read -r path branch; do print_wt "$path" "$branch"; done < <(managed_wts)
}

print_wt() { # <path> <branch>
  local path=$1 branch=$2 state=clean counts ahead behind pr="-"
  [ -z "$(git -C "$path" status --porcelain 2>/dev/null)" ] || state=dirty
  counts=$(git -C "$path" rev-list --left-right --count origin/main...HEAD 2>/dev/null || echo "0 0")
  behind=${counts%%[[:space:]]*}
  ahead=${counts##*[[:space:]]}
  if [ "$branch" != "(detached)" ]; then
    pr=$(gh pr list --head "$branch" --state all --json number 2>/dev/null | jq -r '.[0].number // empty' 2>/dev/null || true)
    pr=${pr:+#$pr}
    pr=${pr:--}
  fi
  echo "$(basename "$path") $branch $state +$ahead/-$behind $pr"
}

cmd_path() {
  [ $# -eq 1 ] || usage
  check_name "$1"
  wt_dir_or_die "$1"
}

[ $# -ge 1 ] || usage
sub=$1
shift
case $sub in
new) cmd_new "$@" ;;
rm) cmd_rm "$@" ;;
ls) cmd_ls ;;
gc) cmd_gc "$@" ;;
path) cmd_path "$@" ;;
*) usage ;;
esac
