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
# `cd ""` stays in the caller's directory and the trap below would delete that directory (old issue 476).
tmp=$(mktemp -d "${TMPDIR:-/tmp}/wt-test.XXXXXX")
tmp=$(cd "$tmp" && pwd -P)
[[ -n $tmp && -d $tmp && $tmp != "$PWD" && $tmp != "$repo" ]] || exit 1
trap '[ -n "${KEEP_TMP:-}" ] || rm -rf "$tmp"' EXIT
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
# The identity hooks (koinon#320): wt new installs them from the checkout's own hack/.
mkdir -p hack
cp -R "$repo/hack/githooks" hack/githooks
cp "$repo/hack/git-identity-check.sh" "$repo/hack/git-identities.allowed" hack/
git add a.txt hack
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
check "new sets core.hooksPath in the worktree" eq "$(git -C "$tmp/kw/foo" config --get core.hooksPath)" hack/githooks
check "main checkout hooksPath stays unset" eq "$(git config --get core.hooksPath || echo unset)" unset
echo hooked >"$tmp/kw/foo/h.txt"
git -C "$tmp/kw/foo" add h.txt
check "a commit as t@example.invalid in the worktree is refused" refuses git -C "$tmp/kw/foo" commit -q -m refused
git -C "$tmp/kw/foo" reset -q
rm -f "$tmp/kw/foo/h.txt"
# the rest of this test commits as t@example.invalid in worktrees on purpose
export KOINON_ALLOWED_IDENTITIES='t <t@example.invalid>'
check "new refuses a bad name" refuses wt new ../x

# pre-push (#654): the worktree's hooks scan what a push sends. A fake gitleaks records its arguments.
mkdir -p "$tmp/glbin"
cat >"$tmp/glbin/gitleaks" <<'SH'
#!/usr/bin/env bash
echo "$*" >>"$FAKE_GITLEAKS_LOG"
[ -z "${FAKE_GITLEAKS_LEAK:-}" ] || { echo "leaks found: 1" >&2; exit 1; }
SH
chmod +x "$tmp/glbin/gitleaks"
export FAKE_GITLEAKS_LOG="$tmp/gitleaks.log"
: >"$FAKE_GITLEAKS_LOG"
foo0=$(git -C "$tmp/kw/foo" rev-parse HEAD)
git -C "$tmp/kw/foo" checkout -q -b push-test
echo p1 >"$tmp/kw/foo/p.txt"
git -C "$tmp/kw/foo" add p.txt
git -C "$tmp/kw/foo" commit -q -m p1
check "a new branch push is scanned against the remote's refs" env PATH="$tmp/glbin:$PATH" git -C "$tmp/kw/foo" push -q origin push-test
check "new branch scan range" grep -qx "git --redact --no-banner --log-opts=$(git -C "$tmp/kw/foo" rev-parse HEAD) --not --remotes=origin ." "$FAKE_GITLEAKS_LOG"
: >"$FAKE_GITLEAKS_LOG"
old_sha=$(git -C "$tmp/kw/foo" rev-parse HEAD)
echo p2 >>"$tmp/kw/foo/p.txt"
git -C "$tmp/kw/foo" commit -q -am p2
check "an update push is scanned" env PATH="$tmp/glbin:$PATH" git -C "$tmp/kw/foo" push -q origin push-test
check "update scan range is remote..local" grep -qx "git --redact --no-banner --log-opts=$old_sha..$(git -C "$tmp/kw/foo" rev-parse HEAD) ." "$FAKE_GITLEAKS_LOG"
echo p3 >>"$tmp/kw/foo/p.txt"
git -C "$tmp/kw/foo" commit -q -am p3
check "a leak refuses the push" refuses env PATH="$tmp/glbin:$PATH" FAKE_GITLEAKS_LEAK=1 git -C "$tmp/kw/foo" push -q origin push-test
check "the refused commit is not on the remote" test "$(git -C "$tmp/origin.git" rev-parse push-test)" != "$(git -C "$tmp/kw/foo" rev-parse HEAD)"
check "KOINON_SKIP_GITLEAKS lets it through" env PATH="$tmp/glbin:$PATH" FAKE_GITLEAKS_LEAK=1 KOINON_SKIP_GITLEAKS=1 git -C "$tmp/kw/foo" push -q origin push-test
: >"$FAKE_GITLEAKS_LOG"
check "a delete push is not scanned and goes through" env PATH="$tmp/glbin:$PATH" FAKE_GITLEAKS_LEAK=1 git -C "$tmp/kw/foo" push -q origin :push-test
check "no scan ran for the delete" test ! -s "$FAKE_GITLEAKS_LOG"
git -C "$tmp/kw/foo" checkout -q --detach "$foo0"
git -C "$tmp/kw/foo" branch -q -D push-test

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

# Both spellings are needed on macOS, where /tmp is a symlink to /private/tmp.
check "sandbox allows the shared root and its macOS canonical path" jq -e '.sandbox.filesystem.allowWrite | index("/tmp/kw") != null and index("/private/tmp/kw") != null' "$repo/.claude/settings.json"

# Creation must use the same writable root from parent and sandbox shells, whose TMPDIR differs.
mkdir -p "$tmp/parent-temp" "$tmp/lane-temp"
default_name=wt-test-${tmp##*/}
out=$(env -u KOINON_WT_ROOT TMPDIR="$tmp/parent-temp" KOINON_WT_NO_INSTALL=1 "$repo/hack/wt.sh" new "$default_name" 2>/dev/null)
default_root=$(cd /tmp/kw && pwd -P)
check "new ignores the parent's TMPDIR by default" eq "$out" "$default_root/$default_name"
check "lane with a different TMPDIR finds the same worktree" eq "$(env -u KOINON_WT_ROOT TMPDIR="$tmp/lane-temp" "$repo/hack/wt.sh" path "$default_name")" "$out"
# shellcheck disable=SC2016 # $1 belongs to the child shell
check "lane can remove a file in the shared worktree" env TMPDIR="$tmp/lane-temp" sh -c 'touch "$1/probe" && rm "$1/probe"' sh "$out"
check "lane with a different TMPDIR removes the worktree" env -u KOINON_WT_ROOT TMPDIR="$tmp/lane-temp" "$repo/hack/wt.sh" rm "$default_name"

# Overrides may be relative or pass through a symlink; stdout and Git must use the physical path.
ln -s "$tmp/kw" "$tmp/root-link"
out=$(KOINON_WT_ROOT="$tmp/root-link" wt new canonical 2>/dev/null)
check "new canonicalizes a symlink root" eq "$out" "$tmp/kw/canonical"
check "path agrees with the canonical new output" eq "$(wt path canonical)" "$out"
check "rm accepts the canonical worktree through its root alias" env KOINON_WT_ROOT="$tmp/root-link" "$repo/hack/wt.sh" rm canonical
mkdir -p relative-root
out=$(KOINON_WT_ROOT=relative-root wt new relative 2>/dev/null)
check "new makes a relative override absolute" eq "$out" "$tmp/main/relative-root/relative"
check "rm finds the relative override" env KOINON_WT_ROOT=relative-root "$repo/hack/wt.sh" rm relative
rmdir relative-root

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
claim_ref() { # <issue> <state> <holder> [lease_until]: a claim ref on origin, as hack/claim.sh writes it
	# (ADR 0104); with a lease_until, as the claim.sh from before ADR 0104 wrote it
	local tree c lease=""
	tree=$(git hash-object -t tree -w /dev/null)
	[ -z "${4:-}" ] || lease="lease_until: $4"$'\n'
	c=$(printf 'claim #%s: x\n\nholder: %s\nhuman: h\n%spr: \nnote: \nstate: %s\n' "$1" "$3" "$lease" "$2" |
		git commit-tree "$tree")
	git push -q -f origin "$c:refs/heads/claim/$1"
}
past=$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v-1H +%Y-%m-%dT%H:%M:%SZ)
: >"$FAKE_PRS"
wt new live --branch fix/700-live >/dev/null 2>&1
git -C "$tmp/kw/live" push -q origin fix/700-live
claim_ref 700 held "alice#l700"
err=$(wt rm live 2>&1 || true)
check "rm refuses a worktree with a held claim" test -d "$tmp/kw/live"
check "rm names the claim check and the holder" grep -q "live claim on #700 is held by alice#l700" <<<"$err"
claim_ref 700 held "alice/host/sess#l700" "$past"
err=$(wt rm live 2>&1 || true)
check "rm refuses an old-format claim whose lease has passed (no lease since ADR 0104)" test -d "$tmp/kw/live"
claim_ref 700 released "alice#l700"
# the claim is released: the claim no longer guards, the process does
(cd "$tmp/kw/live" && exec sleep 60) &
sleeper=$!
for _ in $(seq 1 50); do # wait until the dummy process runs with its cwd in the worktree
	lsof -w -d cwd -Fpn 2>/dev/null | grep -q "^n$tmp/kw/live\$" && break
	read -r -t 0.1 <> <(:) || true
done
err=$(wt rm live 2>&1 || true)
check "rm refuses when a process has its cwd inside" test -d "$tmp/kw/live"
check "rm names the process check" grep -q "working directory inside" <<<"$err"
claim_ref 700 held "alice#l700"
err=$(wt rm live --force 2>&1) || true
check "rm --force prints what it overrides" grep -q -- "--force overrides: a live claim" <<<"$err"
check "rm --force removes it" test ! -e "$tmp/kw/live"
kill "$sleeper" 2>/dev/null || true
wait "$sleeper" 2>/dev/null || true
claim_ref 700 released "alice#l700"
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
ln -s "$tmp/kw" "$tmp/kwlink"
check "rm finds a worktree when the root is spelled differently" env KOINON_WT_ROOT="$tmp/kwlink" "$repo/hack/wt.sh" rm moved
check "rm removed it" test ! -e "$tmp/kw/moved"

# a linked worktree of the same name outside the root is never resolved
git worktree add -q --detach "$tmp/outside-wt" origin/main 2>/dev/null
mkdir -p "$tmp/other"
git worktree add -q --detach "$tmp/other/ext" origin/main 2>/dev/null
check "path ignores a same-named worktree outside the root" refuses wt path ext
check "rm ignores a same-named worktree outside the root" refuses wt rm ext
check "rm left the outside worktree alone" test -d "$tmp/other/ext"
git worktree remove --force "$tmp/other/ext"
git worktree remove --force "$tmp/outside-wt"

# a worktree created under another $TMPDIR (sandbox against host) is found by ls, path, rm and gc
mkdir -p "$tmp/otherroot/kw"
git worktree add -q -b feat/other "$tmp/otherroot/kw/otmp" origin/main 2>/dev/null
git -C "$tmp/otherroot/kw/otmp" push -q origin feat/other
wtx() { env -u KOINON_WT_ROOT TMPDIR="$tmp" KOINON_WT_TMP_BASES="$tmp/otherroot" "$repo/hack/wt.sh" "$@"; }
check "path finds a worktree under another TMPDIR" eq "$(wtx path otmp)" "$tmp/otherroot/kw/otmp"
check "ls lists a worktree under another TMPDIR" grep -q '^otmp feat/other ' <<<"$(wtx ls)"
check "gc sees a worktree under another TMPDIR" grep -q '^keep otmp:' <<<"$(wtx gc --dry-run)"
check "an explicit KOINON_WT_ROOT does not widen the managed set" refuses wt path otmp

# the same name under two temp bases is ambiguous: refused, both paths listed, nothing removed
mkdir -p "$tmp/kw"
git worktree add -q --detach "$tmp/kw/dup" origin/main 2>/dev/null
git worktree add -q --detach "$tmp/otherroot/kw/dup" origin/main 2>/dev/null
err=$(wtx path dup 2>&1 >/dev/null) && err="succeeded"
check "path of an ambiguous name fails and lists the first" grep -q "ambiguous name dup: .*$tmp/kw/dup" <<<"$err"
check "path of an ambiguous name lists the second" grep -q "ambiguous name dup: .*$tmp/otherroot/kw/dup" <<<"$err"
err=$(wtx rm dup 2>&1 >/dev/null) && err="succeeded"
check "rm of an ambiguous name fails and lists both" grep -q "ambiguous name dup: " <<<"$err"
check "rm left the first dup in place" test -d "$tmp/kw/dup"
check "rm left the second dup in place" test -d "$tmp/otherroot/kw/dup"
git worktree remove --force "$tmp/kw/dup"
git worktree remove --force "$tmp/otherroot/kw/dup"
check "rm removes a worktree under another TMPDIR" wtx rm otmp
check "rm removed it" test ! -e "$tmp/otherroot/kw/otmp"

# Claude Code's .claude/worktrees/<name> is left alone, also when the root ends in "worktrees"
mkdir -p "$tmp/home/worktrees" "$tmp/proj/.claude/worktrees"
git worktree add -q --detach "$tmp/proj/.claude/worktrees/cc" origin/main 2>/dev/null
check "path ignores .claude/worktrees with a root named worktrees" refuses env KOINON_WT_ROOT="$tmp/home/worktrees" "$repo/hack/wt.sh" path cc
check "rm ignores .claude/worktrees with a root named worktrees" refuses env KOINON_WT_ROOT="$tmp/home/worktrees" "$repo/hack/wt.sh" rm cc
check "rm left .claude/worktrees alone" test -d "$tmp/proj/.claude/worktrees/cc"
git worktree remove --force "$tmp/proj/.claude/worktrees/cc"

# an old-format claim with a malformed lease is still held (the lease is ignored)
wt new badl --branch fix/703-badl >/dev/null 2>&1
git -C "$tmp/kw/badl" push -q origin fix/703-badl
claim_ref 703 held "alice/host/sess#l703" "not-a-date"
err=$(wt rm badl 2>&1 || true)
check "rm refuses an old-format claim with a malformed lease" test -d "$tmp/kw/badl"
check "rm names the holder" grep -q "live claim on #703 is held by alice/host/sess#l703" <<<"$err"
claim_ref 703 released "alice/host/sess#l703"
check "rm removes it once the claim is released" wt rm badl

# gc: removes only on proof (merged PR, gone upstream)
wt new gaalock --branch fix/719-lock >/dev/null 2>&1
git worktree lock "$tmp/kw/gaalock"
wt new gdone --branch fix/710-done >/dev/null 2>&1
wt new ggone --branch fix/715-gone >/dev/null 2>&1
git -C "$tmp/kw/ggone" push -q -u origin fix/715-gone 2>/dev/null
git push -q origin --delete fix/715-gone
wt new gfresh --branch fix/714-fresh >/dev/null 2>&1
wt new gdet >/dev/null 2>&1
wt new gwip --branch fix/711-wip >/dev/null 2>&1
echo w >"$tmp/kw/gwip/w.txt"
git -C "$tmp/kw/gwip" add w.txt
git -C "$tmp/kw/gwip" commit -q -m w
git -C "$tmp/kw/gwip" push -q origin fix/711-wip
wt new gopen --branch fix/712-open >/dev/null 2>&1
wt new gdirty --branch fix/713-dirty >/dev/null 2>&1
echo d >"$tmp/kw/gdirty/d.txt"
printf '%s\n' "fix/712-open 55 open" "fix/710-done 56 merged" "fix/719-lock 57 merged" >"$FAKE_PRS"
check "rm fails on a locked worktree" refuses wt rm gaalock
check "rm left the locked worktree" test -d "$tmp/kw/gaalock"
out=$(wt gc --dry-run)
check "gc --dry-run would remove the worktree with a merged PR" grep -q "^would remove gdone$" <<<"$out"
check "gc --dry-run would remove the gone-upstream worktree" grep -q "^would remove ggone$" <<<"$out"
check "gc keeps a fresh worktree with no commits" grep -q "^keep gfresh:" <<<"$out"
check "gc keeps a detached worktree" grep -q "^keep gdet:" <<<"$out"
check "gc keeps an unmerged worktree" grep -q "^keep gwip:" <<<"$out"
check "gc keeps a worktree with an open PR" grep -q "^keep gopen: open PR #55" <<<"$out"
check "gc keeps a dirty worktree" grep -q "^keep gdirty:" <<<"$out"
check "gc --dry-run removes nothing" test -d "$tmp/kw/gdone"
gc_rc=0
out=$(wt gc 2>/dev/null) || gc_rc=$?
check "gc reports a failed removal" grep -q "^keep gaalock: git worktree remove failed" <<<"$out"
check "gc exits non-zero after a failed removal" eq "$gc_rc" 1
check "gc continues after a failure (merged PR)" test ! -e "$tmp/kw/gdone"
check "gc continues after a failure (gone upstream)" test ! -e "$tmp/kw/ggone"
check "gc leaves the others" test -d "$tmp/kw/gwip" -a -d "$tmp/kw/gopen" -a -d "$tmp/kw/gdirty" -a -d "$tmp/kw/gfresh" -a -d "$tmp/kw/gdet" -a -d "$tmp/kw/gaalock"

# the main checkout was never touched
check "main checkout HEAD unchanged" eq "$(git rev-parse HEAD)" "$head_before"
check "main checkout stays clean" eq "$(git status --porcelain)" ""
check "main checkout stays on main" eq "$(git symbolic-ref --short HEAD)" main

check "usage error" test "$(wt bogus >/dev/null 2>&1 || echo $?)" = 64

exit "$fail"
