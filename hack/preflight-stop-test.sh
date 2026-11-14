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

[[ $fail -eq 0 ]] && echo "all ok"
exit "$fail"
