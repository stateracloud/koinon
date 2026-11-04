#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/wt.sh offline: a temporary repository with a bare origin, and a `gh` stub on PATH that
# answers `gh pr list --head <branch>` from $FAKE_PRS (lines `<branch> <number> <open|merged>`).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
# mktemp on its own line so set -e sees its failure. Never put it inside a cd: with an empty result
# `cd ""` stays in the caller's directory and the trap below would delete that directory (#476).
tmp=$(mktemp -d "${TMPDIR:-/tmp}/wt-test.XXXXXX")
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

# rm: live claim, process in the worktree, --force and what it prints
claim_ref() { # <issue> <state> <lease_until> <holder>: a claim ref on origin, as hack/claim.sh writes it
	local tree c
	tree=$(git hash-object -t tree -w /dev/null)
	c=$(printf 'claim #%s: x\n\nholder: %s\nhuman: h\nlease_until: %s\npr: \nnote: \nstate: %s\n' "$1" "$4" "$3" "$2" |
		git commit-tree "$tree")
	git push -q -f origin "$c:refs/heads/claim/$1"
}
future=$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v+1H +%Y-%m-%dT%H:%M:%SZ)
past=$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v-1H +%Y-%m-%dT%H:%M:%SZ)
: >"$FAKE_PRS"
wt new live --branch fix/700-live >/dev/null 2>&1
git -C "$tmp/kw/live" push -q origin fix/700-live
claim_ref 700 held "$future" "alice/host/sess#l700"
err=$(wt rm live 2>&1 || true)
check "rm refuses a worktree with a live claim" test -d "$tmp/kw/live"
check "rm names the claim check and the holder" grep -q "live claim on #700 is held by alice/host/sess#l700" <<<"$err"
claim_ref 700 held "$past" "alice/host/sess#l700"
# the lease has expired: the claim no longer guards, the process does
(cd "$tmp/kw/live" && exec sleep 60) &
sleeper=$!
for _ in $(seq 1 50); do # wait until the dummy process runs with its cwd in the worktree
	lsof -w -d cwd -Fpn 2>/dev/null | grep -q "^n$tmp/kw/live\$" && break
	read -r -t 0.1 <> <(:) || true
done
err=$(wt rm live 2>&1 || true)
check "rm refuses when a process has its cwd inside" test -d "$tmp/kw/live"
check "rm names the process check" grep -q "working directory inside" <<<"$err"
claim_ref 700 held "$future" "alice/host/sess#l700"
err=$(wt rm live --force 2>&1) || true
check "rm --force prints what it overrides" grep -q -- "--force overrides: a live claim" <<<"$err"
check "rm --force removes it" test ! -e "$tmp/kw/live"
kill "$sleeper" 2>/dev/null || true
wait "$sleeper" 2>/dev/null || true
claim_ref 700 released "$future" "alice/host/sess#l700"
wt new live2 --branch fix/700-live2 >/dev/null 2>&1
git -C "$tmp/kw/live2" push -q origin fix/700-live2
check "rm ignores a released claim" wt rm live2

# rm fails closed when the claim refs cannot be fetched
wt new offl --branch fix/702-offl >/dev/null 2>&1
git -C "$tmp/kw/offl" push -q origin fix/702-offl
origin_url=$(git remote get-url origin)
git remote set-url origin "$tmp/missing.git"
err=$(wt rm offl 2>&1 || true)
git remote set-url origin "$origin_url"
check "rm refuses when claim refs cannot be fetched" test -d "$tmp/kw/offl"
check "rm says the claim state is unknown" grep -q "could not fetch claim refs from origin" <<<"$err"
check "rm removes it once origin is back" wt rm offl

# rm resolves <name> through git worktree list, not the root
wt new moved --branch fix/701-moved >/dev/null 2>&1
git -C "$tmp/kw/moved" push -q origin fix/701-moved
check "rm finds a worktree when the root differs" env KOINON_WT_ROOT="$tmp/elsewhere" "$repo/hack/wt.sh" rm moved
check "rm removed it" test ! -e "$tmp/kw/moved"

# gc
wt new gdone --branch fix/710-done >/dev/null 2>&1
wt new gwip --branch fix/711-wip >/dev/null 2>&1
echo w >"$tmp/kw/gwip/w.txt"
git -C "$tmp/kw/gwip" add w.txt
git -C "$tmp/kw/gwip" commit -q -m w
git -C "$tmp/kw/gwip" push -q origin fix/711-wip
wt new gopen --branch fix/712-open >/dev/null 2>&1
echo "fix/712-open 55 open" >"$FAKE_PRS"
wt new gdirty --branch fix/713-dirty >/dev/null 2>&1
echo d >"$tmp/kw/gdirty/d.txt"
out=$(wt gc --dry-run)
check "gc --dry-run would remove the merged clean worktree" grep -q "^would remove gdone$" <<<"$out"
check "gc keeps an unmerged worktree" grep -q "^keep gwip:" <<<"$out"
check "gc keeps a worktree with an open PR" grep -q "^keep gopen: open PR #55" <<<"$out"
check "gc keeps a dirty worktree" grep -q "^keep gdirty:" <<<"$out"
check "gc --dry-run removes nothing" test -d "$tmp/kw/gdone"
wt gc >/dev/null
check "gc removes the merged clean worktree" test ! -e "$tmp/kw/gdone"
check "gc leaves the others" test -d "$tmp/kw/gwip" -a -d "$tmp/kw/gopen" -a -d "$tmp/kw/gdirty"

# the main checkout was never touched
check "main checkout HEAD unchanged" eq "$(git rev-parse HEAD)" "$head_before"
check "main checkout stays clean" eq "$(git status --porcelain)" ""
check "main checkout stays on main" eq "$(git symbolic-ref --short HEAD)" main

check "usage error" test "$(wt bogus >/dev/null 2>&1 || echo $?)" = 64

exit "$fail"
