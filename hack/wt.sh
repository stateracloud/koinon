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
#       remove the worktree (<name> is found through `git worktree list`, wherever $TMPDIR points).
#       Refuses, naming each check that failed, when: a live claim (claim/<N> on origin, N from a
#       branch named <type>/<N>-<slug>) is held on its branch; a process has its working directory in
#       it; it has uncommitted changes; or it has commits on no remote branch. --force overrides all
#       of them and prints what it overrides. --branch also deletes its local branch, never one with
#       an open PR. Remove worktrees only through this script, never `git worktree remove`.
#   wt.sh gc [--dry-run]
#       remove every worktree under the root whose branch is merged (or gone) and that has no open PR
#       and passes all the rm checks; prints one keep/remove line each. Never forces.
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
  install_web "$dir"
  echo "$dir"
}

# wt_dir <name>: the worktree's path, resolved through `git worktree list --porcelain` so it works
# when $TMPDIR (and so the default root) differs between a sandbox and the host. A worktree under
# the root wins; otherwise any linked worktree with that directory name. Fails when there is none.
wt_dir() {
	local name=$1 line path="" first=1 found="" real=""
	[ ! -d "$root" ] || real=$(cd "$root" && pwd -P)
	while IFS= read -r line || [ -n "$line" ]; do
		case $line in
		"worktree "*) path=${line#worktree } ;;
		"")
			if [ -n "$path" ]; then
				if [ "$first" -eq 1 ]; then
					first=0 # the main checkout is never a candidate
				elif [ "$(basename "$path")" = "$name" ]; then
					case $path in
					"$real"/*) echo "$path"; return 0 ;;
					*) [ -n "$found" ] || found=$path ;;
					esac
				fi
			fi
			path=""
			;;
		esac
	done < <(git worktree list --porcelain; echo)
	if [ -n "$found" ]; then
		echo "$found"
		return 0
	fi
	[ -d "$root/$name" ] && { echo "$root/$name"; return 0; }
	return 1
}

# claim_reason <branch>: prints a refusal reason when a live claim points at the branch. Claims are
# the refs claim/<issue> on origin (ADR 0082); the issue number is the one in <type>/<N>-<slug>. The
# claim ref is read from the fetched copy, no network call besides the optional fetch.
claim_fetched=0
claim_reason() {
	local branch=$1 n ref msg state holder lease now
	[[ $branch =~ ^[a-z]+/([0-9]+)- ]] || return 0
	n=${BASH_REMATCH[1]}
	if [ "$claim_fetched" -eq 0 ]; then
		claim_fetched=1
		[ -n "${KOINON_WT_NO_FETCH:-}" ] || git fetch -q origin '+refs/heads/claim/*:refs/remotes/origin/claim/*' >/dev/null 2>&1 || true
	fi
	for ref in "refs/remotes/origin/claim/$n" "refs/heads/claim/$n"; do
		git rev-parse -q --verify "$ref" >/dev/null 2>&1 || continue
		msg=$(git log -1 --format=%B "$ref" 2>/dev/null) || continue
		state=$(sed -n 's/^state: *//p' <<<"$msg" | head -n1)
		holder=$(sed -n 's/^holder: *//p' <<<"$msg" | head -n1)
		lease=$(sed -n 's/^lease_until: *//p' <<<"$msg" | head -n1)
		[ "$state" = held ] || continue
		now=$(date -u +%s)
		lease=$(jq -rn --arg l "$lease" '$l | fromdateiso8601? // 0')
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
		r=$(claim_reason "$branch")
		[ -z "$r" ] || reasons+=("$r")
	fi
	r=$(proc_reason "$dir")
	[ -z "$r" ] || reasons+=("$r")
	[ -z "$(git -C "$dir" status --porcelain)" ] || reasons+=("$name has uncommitted changes")
	unpushed=$(git -C "$dir" rev-list --count HEAD --not --remotes)
	[ "$unpushed" -eq 0 ] || reasons+=("$name has $unpushed commit(s) on no remote branch")
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
	if [ "$force" -eq 1 ]; then
		git worktree remove --force "$dir"
	else
		git worktree remove "$dir"
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
				git branch -q -D "$branch"
			fi
			;;
		esac
	fi
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
	dir=$(wt_dir "$name") || die "no worktree $name"
	remove_wt "$dir" "$name" "$force" "$delbranch" || exit 1
}

# gc_reason <dir>: prints why the worktree is not finished (empty: its branch is merged or gone).
gc_finished() { # <dir> -> 0 when HEAD is on origin/main, the PR merged, or the upstream is gone
	local dir=$1 branch track
	git -C "$dir" merge-base --is-ancestor HEAD origin/main 2>/dev/null && return 0
	branch=$(git -C "$dir" symbolic-ref -q --short HEAD || true)
	[ -n "$branch" ] || return 1
	track=$(git -C "$dir" for-each-ref --format='%(upstream:track)' "refs/heads/$branch")
	[ "$track" = "[gone]" ] && return 0
	[ -n "$(gh pr list --head "$branch" --state merged --json number 2>/dev/null | jq -r '.[0].number // empty' 2>/dev/null || true)" ]
}

cmd_gc() {
	local dry=0 line path="" real first=1 name branch pr r
	while [ $# -gt 0 ]; do
		case $1 in
		--dry-run) dry=1 ;;
		*) usage ;;
		esac
		shift
	done
	[ -d "$root" ] || return 0
	real=$(cd "$root" && pwd -P)
	local paths=()
	while IFS= read -r line || [ -n "$line" ]; do
		case $line in
		"worktree "*) path=${line#worktree } ;;
		"")
			if [ -n "$path" ]; then
				[ "$first" -eq 1 ] || case $path in "$real"/*) paths+=("$path") ;; esac
				first=0
			fi
			path=""
			;;
		esac
	done < <(git worktree list --porcelain; echo)
	for path in "${paths[@]}"; do
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
			git worktree remove "$path"
			echo "removed $name"
		fi
	done
}

cmd_ls() {
  [ -d "$root" ] || return 0
  local real line="" path="" branch=""
  real=$(cd "$root" && pwd -P)
  # Records are blank-line separated; the trailing blank line flushes the last one.
  while IFS= read -r line || [ -n "$line" ]; do
    case $line in
    "worktree "*)
      path=${line#worktree }
      branch="(detached)"
      ;;
    "branch "*) branch=${line#branch refs/heads/} ;;
    "")
      if [ -n "$path" ]; then
        case $path in "$real"/*) print_wt "$path" "$branch" ;; esac
      fi
      path=""
      ;;
    esac
  done < <(git worktree list --porcelain)
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
  wt_dir "$1" || die "no worktree $1"
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
