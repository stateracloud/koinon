#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests for hack/preflight.sh (old issue 391): per-worktree log directories, the pid file and --stop
# stopping only its own worktree's run. Each fixture repo carries stub hack/adr-lint.sh and hack/adr-lint-test.sh.
# shellcheck disable=SC2015,SC2329 # "A && ok || bad" is the test idiom; cleanup runs from a trap

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
pre=$here/preflight.sh
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }
eq() { if [[ $2 == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/preflight-stop-test.XXXXXX")
pids=""
cleanup() {
	local p
	for p in $pids; do kill "$p" 2>/dev/null || true; done
	rm -rf "$tmp"
}
trap cleanup EXIT
export KUBECONFIG=/dev/null TMPDIR=$tmp/tmp GIT_CONFIG_GLOBAL=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
unset CI KOINON_GATE_REUSE # reuse is off in CI; the cases below set CI themselves
mkdir -p "$TMPDIR"

mkrepo() { # <name> <adr-lint body>
	local d=$tmp/$1
	mkdir -p "$d/hack" "$d/docs/adr"
	printf '#!/usr/bin/env bash\n%s\n' "$2" >"$d/hack/adr-lint.sh"
	printf '#!/usr/bin/env bash\nexit 0\n' >"$d/hack/adr-lint-test.sh" # the adr gate runs it first
	printf '#!/usr/bin/env bash\nexit 0\n' >"$d/hack/doc-meta-lint-test.sh" # and the metadata lint after it
	printf '#!/usr/bin/env bash\nexit 0\n' >"$d/hack/doc-meta-lint.sh"
	chmod +x "$d/hack/adr-lint.sh" "$d/hack/adr-lint-test.sh" "$d/hack/doc-meta-lint-test.sh" "$d/hack/doc-meta-lint.sh"
	echo x >"$d/docs/adr/0001-x.md"
	git -C "$d" init -q
	git -C "$d" add -A
	git -C "$d" commit -q -m init
	git -C "$d" update-ref refs/remotes/origin/main HEAD
	echo x >>"$d/docs/adr/0001-x.md" # uncommitted change selects the adr gate
}

mkrepo a 'echo log-a'
mkrepo b 'echo log-b'
mkrepo slowa 'sleep 60'
mkrepo slowb 'sleep 60'

(cd "$tmp/a" && "$pre" --only adr >"$tmp/a.out")
(cd "$tmp/b" && "$pre" --only adr >"$tmp/b.out")
eq "run passes" "$(tail -1 "$tmp/a.out")" "preflight ok (1 gates)"
dirs=$(find "$TMPDIR/koinon-preflight" -name adr.log | wc -l | tr -d ' ')
eq "two worktrees, two log directories" "$dirs" "2"
la=$(sed -n 's/.*log=\(.*\)/\1/p' "$tmp/a.out" | head -1)
[[ -z $la ]] && la=$(find "$TMPDIR/koinon-preflight" -name adr.log -exec grep -l log-a {} +)
eq "a's log holds only a's output" "$(cat "$la")" "log-a"
[[ -z $(find "$TMPDIR/koinon-preflight" -name "preflight.*.pid") ]] && ok "pid file removed after a run" || bad "stale pid file"

# --stop with nothing running
eq "stop with no run" "$(cd "$tmp/a" && "$pre" --stop)" "preflight: no run in progress for $(cd "$tmp/a" && pwd -P)"

# --stop stops only this worktree's run
(cd "$tmp/slowa" && exec "$pre" --only adr >/dev/null 2>&1) &
pa=$!
(cd "$tmp/slowb" && exec "$pre" --only adr >/dev/null 2>&1) &
pb=$!
pids="$pa $pb"
for _ in $(seq 1 100); do
	[[ $(find "$TMPDIR/koinon-preflight" -name "preflight.*.pid" | wc -l | tr -d ' ') -ge 2 ]] && break
	python3 -c 'import time; time.sleep(0.1)'
done
eq "both runs have pid files" "$(find "$TMPDIR/koinon-preflight" -name "preflight.*.pid" | wc -l | tr -d ' ')" "2"
(cd "$tmp/slowa" && "$pre" --stop) >/dev/null
for _ in $(seq 1 50); do
	kill -0 "$pa" 2>/dev/null || break
	python3 -c 'import time; time.sleep(0.1)'
done
kill -0 "$pa" 2>/dev/null && bad "--stop did not stop its own run" || ok "--stop stops its own run"
kill -0 "$pb" 2>/dev/null && ok "--stop leaves the other worktree's run" || bad "--stop killed another worktree's run"

# a second run of the same gate in a worktree that is already running it is refused
rc=0
(cd "$tmp/slowb" && "$pre" --only adr >/dev/null 2>&1) || rc=$?
eq "second concurrent run of one gate is refused" "$rc" "2"

# one pid file per gate (#990): a run of another gate in the same worktree is not refused, and --stop stops both
printf '#!/usr/bin/env bash\nsleep 60\n' >"$tmp/slowb/hack/mktemp-lint.sh"
printf '#!/usr/bin/env bash\nexit 0\n' >"$tmp/slowb/hack/mktemp-lint-test.sh"
chmod +x "$tmp/slowb/hack/mktemp-lint.sh" "$tmp/slowb/hack/mktemp-lint-test.sh"
(cd "$tmp/slowb" && exec "$pre" --only mktemp >/dev/null 2>&1) &
pc=$!
pids="$pids $pc"
for _ in $(seq 1 100); do
	[[ $(find "$TMPDIR/koinon-preflight" -name "preflight.*.pid" | wc -l | tr -d ' ') -ge 2 ]] && break
	python3 -c 'import time; time.sleep(0.1)'
done
eq "two gates of one worktree run at once, a pid file each" "$(find "$TMPDIR/koinon-preflight" -name "preflight.*.pid" | wc -l | tr -d ' ')" "2"
kill -0 "$pc" 2>/dev/null && ok "the second gate was not refused" || bad "a run of another gate was refused"
(cd "$tmp/slowb" && "$pre" --stop) >/dev/null
for _ in $(seq 1 50); do
	kill -0 "$pb" 2>/dev/null || kill -0 "$pc" 2>/dev/null || break
	python3 -c 'import time; time.sleep(0.1)'
done
if kill -0 "$pb" 2>/dev/null || kill -0 "$pc" 2>/dev/null; then bad "--stop left a run of the worktree alive"; else ok "--stop stops every gate of the worktree"; fi
eq "--stop removes every pid file of the worktree" "$(find "$TMPDIR/koinon-preflight" -name "preflight.*.pid" | wc -l | tr -d ' ')" "0"

# verdict reuse (#990): an unchanged tree reuses the green verdict, a change, --no-reuse and CI=true run the gate again
echo first >>"$tmp/a/docs/adr/0001-x.md" # a new input: nothing is stored for it yet
(cd "$tmp/a" && "$pre" --only adr >"$tmp/r1.out")
(cd "$tmp/a" && "$pre" --only adr >"$tmp/r2.out")
grep -q '^adr ok .*s$' "$tmp/r1.out" && ok "the first run executes the gate" || bad "first run: $(cat "$tmp/r1.out")"
grep -q '^adr ok 0s (reused' "$tmp/r2.out" && ok "a rerun on an unchanged tree reuses the verdict" || bad "rerun: $(cat "$tmp/r2.out")"
(cd "$tmp/a" && "$pre" --only adr --no-reuse >"$tmp/r3.out")
grep -q 'reused' "$tmp/r3.out" && bad "--no-reuse reused" || ok "--no-reuse runs the gate"
(cd "$tmp/a" && CI=true "$pre" --only adr >"$tmp/r4.out")
grep -q 'reused' "$tmp/r4.out" && bad "CI=true reused" || ok "CI=true runs the gate (CI always runs all)"
echo more >>"$tmp/a/docs/adr/0001-x.md"
(cd "$tmp/a" && "$pre" --only adr >"$tmp/r5.out")
grep -q 'reused' "$tmp/r5.out" && bad "a changed input reused the verdict" || ok "a changed input runs the gate again"
printf '#!/usr/bin/env bash\necho broken; exit 1\n' >"$tmp/a/hack/adr-lint.sh"
rc=0
(cd "$tmp/a" && "$pre" --only adr >"$tmp/r6.out") || rc=$?
eq "a failing gate fails and is never stored" "$rc" "1"
rc=0
(cd "$tmp/a" && "$pre" --only adr >"$tmp/r7.out") || rc=$?
eq "a failure is not reused" "$rc" "1"

# reuse fingerprints (#990, review): a gate's fingerprint covers what it reads, not only what selects it
fp_in() { # <kind> <name>: the fingerprint in the fixture repo `a` (the real libs and rules)
	# shellcheck disable=SC1091 # sourced by a path computed at run time
	(cd "$tmp/a" && . "$here/paths-lib.sh" && paths_load && . "$here/gate-reuse-lib.sh" && gate_fingerprint "$1" "$2" base)
}
mkdir -p "$tmp/a/internal/db/migrations"
echo "select 1;" >"$tmp/a/internal/db/migrations/00001_x.sql"
f1=$(fp_in job go)
echo "select 2;" >"$tmp/a/internal/db/migrations/00001_x.sql"
[[ $f1 != "$(fp_in job go)" ]] && ok "a non-.go change moves the go fingerprint (no stale reuse)" || bad "go fingerprint ignored a .sql change"
t1=$(fp_in tree migrate)
echo "x: 1" >"$tmp/a/.golangci.yml"
[[ $t1 != "$(fp_in tree migrate)" ]] && ok "a gate-only gate fingerprints the whole tree" || bad "tree fingerprint ignored a config file"
s1=$(fp_in job scripts)
git -C "$tmp/a" -c commit.gpgsign=false commit -q --allow-empty -m "feat: a new commit"
[[ $s1 != "$(fp_in job scripts)" ]] && ok "a new commit moves the scripts fingerprint (commit checks rerun)" || bad "scripts fingerprint ignored HEAD"

c1=$(fp_in job chartsmoke)
mkdir -p "$tmp/a/cmd/koinon-api"
echo "package main" >"$tmp/a/cmd/koinon-api/main.go"
[[ $c1 != "$(fp_in job chartsmoke)" ]] && ok "a cmd/ edit moves the chartsmoke fingerprint (computed package graph)" || bad "chartsmoke fingerprint ignored a cmd/ file"
h1=$(fp_in job harnesses)
mkdir -p "$tmp/a/internal/x"
echo "package x" >"$tmp/a/internal/x/x.go"
[[ $h1 != "$(fp_in job harnesses)" ]] && ok "an internal/ edit moves the harnesses fingerprint" || bad "harnesses fingerprint ignored an internal/ file"
w1=$(fp_in job web)
echo "all:" >"$tmp/a/Makefile"
[[ $w1 != "$(fp_in job web)" ]] && ok "a Makefile change moves a job fingerprint (the everything inputs)" || bad "web fingerprint ignored the Makefile"
g1=$(fp_in gate migrate)
echo "# x" >>"$tmp/a/go.mod"
[[ $g1 != "$(fp_in gate migrate)" ]] && ok "a go.mod change moves a gate fingerprint" || bad "gate fingerprint ignored go.mod"

# deletion, escape and mode (#990, review round 3)
echo "package y" >"$tmp/a/internal/x/y.go"
git -C "$tmp/a" add internal/x/y.go
git -C "$tmp/a" -c commit.gpgsign=false commit -q -m "feat: y"
d1=$(fp_in tree migrate)
dj1=$(fp_in job go)
rm "$tmp/a/internal/x/y.go"
[[ $d1 != "$(fp_in tree migrate)" ]] && ok "deleting a tracked file moves the tree fingerprint" || bad "tree fingerprint ignored a deleted file"
[[ $dj1 != "$(fp_in job go)" ]] && ok "deleting a tracked file moves a job fingerprint" || bad "job fingerprint ignored a deleted file"
dj2=$(fp_in job scripts)
git -C "$tmp/a" checkout -q -- internal/x/y.go
[[ $dj2 != "$(fp_in job scripts)" ]] && ok "restoring the deleted file moves the fingerprint back" || bad "restore not seen"
e1=$(fp_in job escape)
echo "package x // changed" >"$tmp/a/internal/x/x.go"
[[ $e1 != "$(fp_in job escape)" ]] && ok "a non-test internal/ edit moves the escape fingerprint" || bad "escape fingerprint ignored internal/"
printf '#!/usr/bin/env bash\n' >"$tmp/a/hack/modeprobe.sh"
git -C "$tmp/a" add hack/modeprobe.sh
git -C "$tmp/a" -c commit.gpgsign=false commit -q -m "feat: modeprobe"
mo1=$(fp_in tree migrate)
chmod +x "$tmp/a/hack/modeprobe.sh"
mo2=$(fp_in tree migrate)
[[ $mo1 != "$mo2" ]] && ok "chmod +x moves the fingerprint (committed file)" || bad "fingerprint ignored a mode change"
chmod -x "$tmp/a/hack/modeprobe.sh"
echo "# edit" >>"$tmp/a/hack/modeprobe.sh"
mo3=$(fp_in tree migrate)
chmod +x "$tmp/a/hack/modeprobe.sh"
[[ $mo3 != "$(fp_in tree migrate)" ]] && ok "chmod on a dirty file moves the fingerprint" || bad "dirty-file mode ignored"
git -C "$tmp/a" checkout -q -- hack/modeprobe.sh

# tree fingerprint, property style (#990): every kind of change moves it, on a clean tree and on a dirty one;
# fingerprinting touches neither the real index nor `git status`
prop_fixture() { # <name> <dirty yes|no>: tracked files, a file symlink and a directory symlink
	local r=$tmp/$1
	mkdir -p "$r/hack" "$r/d1" "$r/d2" "$r/src"
	cp "$here/paths-lib.sh" "$here/gate-reuse-lib.sh" "$here/paths.rules" "$r/hack/"
	echo "package a" >"$r/src/a.go"
	echo "package b" >"$r/src/b.go"
	printf '#!/bin/sh\n' >"$r/src/run.sh"
	chmod +x "$r/src/run.sh"
	echo one >"$r/d1/f"
	echo two >"$r/d2/f"
	ln -s src/a.go "$r/link-file"
	ln -s d1 "$r/link-dir"
	git -C "$r" init -q
	git -C "$r" add -A
	git -C "$r" -c commit.gpgsign=false commit -q -m "feat: base"
	if [[ $2 == yes ]]; then
		echo "package c" >"$r/src/untracked.go"
		echo "// staged" >>"$r/src/b.go"
		git -C "$r" add src/b.go
		echo "// unstaged" >>"$r/src/b.go"
	fi
}
prop_fp() { (cd "$1" && . ./hack/paths-lib.sh && paths_load && . ./hack/gate-reuse-lib.sh && gate_fingerprint tree migrate base); }
for dirty in no yes; do
	prop_fixture "p$dirty" "$dirty"
	pr=$tmp/p$dirty
	f0=$(prop_fp "$pr")
	eq "[$dirty] an unchanged rerun gives the same fingerprint" "$(prop_fp "$pr")" "$f0"
	idx0=$(git -C "$pr" ls-files -s | shasum -a 256)
	st0=$(git -C "$pr" status --porcelain=v1 -uall)
	snap=$tmp/snap-$dirty
	cp -R "$pr" "$snap"
	mut() { # <label> <command...>: run the mutation in the fixture, the fingerprint must move, then restore
		local label=$1
		shift
		(cd "$pr" && "$@")
		[[ $(prop_fp "$pr") != "$f0" ]] && ok "[$dirty] $label moves the tree fingerprint" || bad "[$dirty] $label did not move the tree fingerprint"
		rm -rf "$pr" && cp -R "$snap" "$pr"
	}
	mut "editing a committed file" sh -c 'echo "// e" >>src/a.go'
	mut "deleting a tracked file" rm src/a.go
	mut "chmod -x" chmod -x src/run.sh
	mut "retargeting a file symlink" sh -c 'rm link-file && ln -s src/b.go link-file'
	mut "retargeting a directory symlink" sh -c 'rm link-dir && ln -s d2 link-dir'
	mut "git mv" git mv src/a.go src/a2.go
	mut "adding an untracked file" sh -c 'echo new >src/new.go'
	mut "staging an edit" sh -c 'echo "// s" >>src/a.go && git add src/a.go'
	mut "committing an edit" sh -c 'echo "// c" >>src/a.go && git add src/a.go && git -c commit.gpgsign=false commit -q -m "feat: c"'
	eq "[$dirty] fingerprinting leaves the index alone" "$(git -C "$pr" ls-files -s | shasum -a 256)" "$idx0"
	eq "[$dirty] fingerprinting leaves git status alone" "$(git -C "$pr" status --porcelain=v1 -uall)" "$st0"
done

# a stale pid file (a run that died) is taken over; the takeover lock is gone afterwards
sleep 0 & dead=$!
wait "$dead"
mkdir -p "$TMPDIR/koinon-preflight"
# shellcheck disable=SC1091 # sourced by a path computed at run time
key=$(cd "$tmp/a" && . "$here/gate-reuse-lib.sh" && basename "$(gate_logdir)")
mkdir -p "$TMPDIR/koinon-preflight/$key"
echo "$dead" >"$TMPDIR/koinon-preflight/$key/preflight.adr.pid"
(cd "$tmp/a" && "$pre" --only adr --no-reuse >"$tmp/r8.out" 2>&1) || true # the adr-lint stub fails by now; only the claim matters
grep -q 'already in progress' "$tmp/r8.out" && bad "a stale pid file refused the run" || ok "a stale pid file is taken over"
eq "no takeover lock is left" "$(find "$TMPDIR/koinon-preflight" -name "*.lock" | wc -l | tr -d ' ')" "0"

[[ $fail -eq 0 ]] && echo "all ok"
exit "$fail"
