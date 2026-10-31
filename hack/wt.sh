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
#       creates that branch, otherwise HEAD is detached. Prints only the absolute path.
#   wt.sh rm <name> [--branch] [--force]
#       remove the worktree. Refuses when it is dirty or has commits on no remote branch (--force
#       skips both). --branch also deletes its local branch, never one with an open PR.
#   wt.sh ls
#       one line per worktree: name branch clean|dirty +ahead/-behind (vs origin/main) #PR|-
#   wt.sh path <name>
#       print the worktree path (exit 1 when it does not exist)
set -euo pipefail
export GH_PAGER=cat PAGER=cat

root=${KOINON_WT_ROOT:-${TMPDIR:-/tmp}/kw}
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
  echo "$dir"
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
  local dir=$root/$name
  [ -d "$dir" ] || die "no worktree $name"
  if [ "$force" -eq 0 ]; then
    [ -z "$(git -C "$dir" status --porcelain)" ] || die "$name is dirty (--force to remove anyway)"
    local unpushed
    unpushed=$(git -C "$dir" rev-list --count HEAD --not --remotes)
    [ "$unpushed" -eq 0 ] || die "$name has $unpushed commit(s) on no remote branch (--force to remove anyway)"
  fi
  local branch=""
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
  [ -d "$root/$1" ] || die "no worktree $1"
  echo "$root/$1"
}

[ $# -ge 1 ] || usage
sub=$1
shift
case $sub in
new) cmd_new "$@" ;;
rm) cmd_rm "$@" ;;
ls) cmd_ls ;;
path) cmd_path "$@" ;;
*) usage ;;
esac
