#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/wt.sh offline: a temporary repository with a bare origin, and a `gh` stub on PATH that
# answers `gh pr list --head <branch>` from $FAKE_PRS (lines `<branch> <number> <open|merged>`).
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
# mktemp on its own line so set -e sees its failure. Never put it inside a cd: with an empty result
# `cd ""` stays in the caller's directory and the trap below would delete that directory (#476).
tmp=$(mktemp -d)
tmp=$(cd "$tmp" && pwd -P)
[[ -n $tmp && -d $tmp && $tmp != "$PWD" && $tmp != "$repo" ]] || exit 1
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
export PATH="$tmp/bin:$PATH" FAKE_PRS="$tmp/prs" KOINON_WT_ROOT="$tmp/kw"
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
: >"$FAKE_PRS"

cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
[ "$1 $2" = "pr list" ] || exit 9
head="" state=""
while [ $# -gt 0 ]; do
  case $1 in --head) head=$2 ;; --state) state=$2 ;; esac
  shift
done
while read -r b n s; do
  [ "$b" = "$head" ] || continue
  if [ "$state" = all ] || [ "$state" = "$s" ]; then
    echo "[{\"number\":$n}]"
    exit 0
  fi
done <"$FAKE_PRS"
echo "[]"
SH
chmod +x "$tmp/bin/gh"

git init -q --bare -b main "$tmp/origin.git"
git clone -q "$tmp/origin.git" "$tmp/main" 2>/dev/null
cd "$tmp/main"
git checkout -q -b main
echo one >a.txt
git add a.txt
git commit -q -m one
git push -q origin main
head_before=$(git rev-parse HEAD)

wt() { "$repo/hack/wt.sh" "$@"; }
fail=0
check() { # <description> <command...>
  local d=$1
  shift
  if "$@" >/dev/null 2>&1; then echo "ok: $d"; else
    echo "FAIL: $d" >&2
    fail=1
  fi
}
# shellcheck disable=SC2329 # called through check
refuses() { ! "$@"; }
# shellcheck disable=SC2329 # called through check
eq() { [ "$1" = "$2" ]; }

# new: detached, prints only the path
out=$(wt new foo 2>/dev/null)
check "new prints only the path" eq "$out" "$tmp/kw/foo"
check "new creates a detached worktree" test -z "$(git -C "$tmp/kw/foo" symbolic-ref -q HEAD || true)"
check "new starts at origin/main" eq "$(git -C "$tmp/kw/foo" rev-parse HEAD)" "$(git rev-parse origin/main)"
check "new refuses an existing name" refuses wt new foo
check "new refuses a bad name" refuses wt new ../x

# new --branch, --from
echo two >>a.txt
git commit -q -am two
git push -q origin main
git reset -q --hard HEAD~1 # the main checkout is now behind origin/main
out=$(wt new bar --branch feat/bar 2>/dev/null)
check "new --branch prints the path" eq "$out" "$tmp/kw/bar"
check "new --branch checks the branch out" eq "$(git -C "$tmp/kw/bar" symbolic-ref --short HEAD)" feat/bar
check "new fetches origin first" eq "$(git -C "$tmp/kw/bar" rev-parse HEAD)" "$(git rev-parse origin/main)"
out=$(wt new old --from HEAD 2>/dev/null)
check "new --from REF" eq "$(git -C "$out" rev-parse HEAD)" "$(git rev-parse HEAD)"

# path
check "path prints the path" eq "$(wt path bar)" "$tmp/kw/bar"
check "path of a missing worktree fails" refuses wt path nope

# ls
echo three >"$tmp/kw/bar/b.txt"
git -C "$tmp/kw/bar" add b.txt
git -C "$tmp/kw/bar" commit -q -m three
echo "feat/bar 42 open" >"$FAKE_PRS"
ls_out=$(wt ls)
check "ls: one line per worktree" eq "$(printf '%s\n' "$ls_out" | wc -l | tr -d ' ')" 3
check "ls: branch, clean, ahead, PR" eq "$(printf '%s\n' "$ls_out" | grep '^bar ')" "bar feat/bar clean +1/-0 #42"
check "ls: detached worktree" eq "$(printf '%s\n' "$ls_out" | grep '^foo ')" "foo (detached) clean +0/-1 -"
check "ls: the main checkout is not listed" eq "$(printf '%s\n' "$ls_out" | grep -c main || true)" 0
echo dirt >"$tmp/kw/foo/untracked.txt"
check "ls: dirty" eq "$(wt ls | grep '^foo ')" "foo (detached) dirty +0/-1 -"

# rm refusals
check "rm refuses a dirty worktree" refuses wt rm foo
check "rm leaves a dirty worktree in place" test -d "$tmp/kw/foo"
check "rm refuses unpushed commits" refuses wt rm bar
check "rm leaves the unpushed worktree in place" test -d "$tmp/kw/bar"
check "rm of a missing worktree fails" refuses wt rm nope

# rm --force, rm clean, rm --branch
check "rm --force removes a dirty worktree" wt rm foo --force
check "rm --force removed it" test ! -e "$tmp/kw/foo"
check "rm removes a clean detached worktree" wt rm old
git -C "$tmp/kw/bar" push -q origin feat/bar
check "rm --branch keeps a branch with an open PR" wt rm bar --branch
check "rm removed the worktree" test ! -e "$tmp/kw/bar"
check "rm --branch kept the branch with an open PR" git show-ref --verify -q refs/heads/feat/bar
wt new baz --branch feat/baz >/dev/null 2>&1
echo "feat/baz 43 merged" >"$FAKE_PRS"
echo x >"$tmp/kw/baz/c.txt"
git -C "$tmp/kw/baz" add c.txt
git -C "$tmp/kw/baz" commit -q -m c
git -C "$tmp/kw/baz" push -q origin feat/baz
check "rm --branch deletes the branch of a merged PR" wt rm baz --branch
check "rm --branch deleted it" refuses git show-ref --verify -q refs/heads/feat/baz

# the main checkout was never touched
check "main checkout HEAD unchanged" eq "$(git rev-parse HEAD)" "$head_before"
check "main checkout stays clean" eq "$(git status --porcelain)" ""
check "main checkout stays on main" eq "$(git symbolic-ref --short HEAD)" main

check "usage error" test "$(wt bogus >/dev/null 2>&1 || echo $?)" = 64

exit "$fail"
