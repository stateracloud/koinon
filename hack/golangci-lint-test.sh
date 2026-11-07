#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests of hack/golangci-lint.sh (#236): two worktrees of one repository under one shared
# TMPDIR get their own lock and cache directories and can lint at the same time, while two runs in
# the same worktree still exclude each other. golangci-lint is a stub that takes a lock in $TMPDIR
# the way the real one does (mkdir instead of flock, so it also runs on macOS) and records the
# directories it was given. Nothing is linted for real.
# shellcheck disable=SC2016 # the stub is written with single quotes on purpose

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/koinon-test.XXXXXX") || exit 1 # explicit template: macOS mktemp -d ignores TMPDIR (old issue 466)
trap '[[ -n ${KEEP_TMP:-} ]] || rm -rf "$tmp"' EXIT
export TMPDIR="$tmp/shared-tmp" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
unset KOINON_LINT_DIR GOLANGCI_LINT_CACHE
mkdir -p "$tmp/bin" "$TMPDIR"

# The stub: "parallel golangci-lint is running" (exit 3) when the lock in its TMPDIR is held. With
# FAKE_HOLD=<name> it holds the lock, creates $tmp/<name>.held, and waits (at most 10 s) for
# $tmp/<name>.release, so overlapping runs depend on a handshake, not on timing.
cat >"$tmp/bin/golangci-lint" <<'EOF2'
#!/usr/bin/env bash
echo "args=$* TMPDIR=$TMPDIR GOLANGCI_LINT_CACHE=${GOLANGCI_LINT_CACHE:-}" >>"$FAKE_LOG"
mkdir "$TMPDIR/golangci-lint.lock" 2>/dev/null || { echo "Error: parallel golangci-lint is running" >&2; exit 3; }
if [[ -n ${FAKE_HOLD:-} ]]; then
	: >"$FAKE_DIR/$FAKE_HOLD.held"
	for _ in $(seq 100); do [[ -e $FAKE_DIR/$FAKE_HOLD.release ]] && break; sleep 0.1; done
fi
rmdir "$TMPDIR/golangci-lint.lock"
exit "${FAKE_LINT_RC:-0}"
EOF2
chmod +x "$tmp/bin/golangci-lint"
export PATH="$tmp/bin:$PATH"

git init -q -b main "$tmp/main"
git -C "$tmp/main" -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m seed
mkdir -p "$tmp/main/hack"
cp "$repo/hack/golangci-lint.sh" "$tmp/main/hack/"
git -C "$tmp/main" worktree add -q "$tmp/lane" 2>/dev/null
mkdir -p "$tmp/lane/hack"
cp "$repo/hack/golangci-lint.sh" "$tmp/lane/hack/"

fail=0
check() { # <description> <command...>
	local d=$1
	shift
	if "$@"; then echo "ok   $d"; else echo "FAIL $d"; fail=1; fi
}
eq() { [[ $1 == "$2" ]] || { echo "  got '$1', want '$2'" >&2; return 1; }; }
ne() { [[ $1 != "$2" ]] || { echo "  both are '$1'" >&2; return 1; }; }
under() { [[ $1 == "$2"/* ]] || { echo "  '$1' is not under '$2'" >&2; return 1; }; }

dirs() { (cd "$1" && hack/golangci-lint.sh --print-dirs) | sed -n "s/^$2=//p"; }

# 1. per-worktree paths
mt=$(dirs "$tmp/main" TMPDIR)
mc=$(dirs "$tmp/main" GOLANGCI_LINT_CACHE)
lt=$(dirs "$tmp/lane" TMPDIR)
lc=$(dirs "$tmp/lane" GOLANGCI_LINT_CACHE)
check "TMPDIR is under the shared TMPDIR's koinon-lint/" under "$mt" "$TMPDIR/koinon-lint"
check "two worktrees get different lock directories" ne "$mt" "$lt"
check "two worktrees get different caches" ne "$mc" "$lc"
check "the same worktree gets the same directories again" eq "$(dirs "$tmp/main" TMPDIR)" "$mt"
check "a subdirectory of a worktree uses the worktree's directories" eq "$(cd "$tmp/main/hack" && ./golangci-lint.sh --print-dirs | head -1)" "TMPDIR=$mt"
check "the directories exist" test -d "$mt" -a -d "$mc"
check "KOINON_LINT_DIR overrides the base" eq "$(cd "$tmp/main" && KOINON_LINT_DIR="$tmp/own" hack/golangci-lint.sh --print-dirs | head -1)" "TMPDIR=$tmp/own/tmp"

# 2. the stub is called with the arguments and the per-worktree directories, and its exit code passes through
export FAKE_LOG="$tmp/calls.log"
: >"$FAKE_LOG"
rc=0
(cd "$tmp/main" && FAKE_LINT_RC=1 hack/golangci-lint.sh run ./internal/x) || rc=$?
check "exit code passes through" eq "$rc" 1
check "arguments pass through" grep -q "^args=run ./internal/x " "$FAKE_LOG"
check "golangci-lint sees the worktree's TMPDIR and cache" grep -qF "TMPDIR=$mt GOLANGCI_LINT_CACHE=$mc" "$FAKE_LOG"

# hold <name> <dir> <command...>: start the command in <dir> in the background as a lock holder
# and wait until the stub holds the lock; release <name> lets it finish and prints its exit code.
hold() {
	local n=$1 d=$2
	shift 2
	(cd "$d" && FAKE_HOLD=$n "$@" >"$tmp/$n.out" 2>&1) &
	pids[$n]=$!
	for _ in $(seq 100); do [[ -e $tmp/$n.held ]] && return 0; sleep 0.1; done
	echo "  $n never took the lock: $(cat "$tmp/$n.out")" >&2
	return 1
}
declare -A pids=() rcs=()
release() { # sets rcs[<name>] to the holder's exit code (no subshell: only this shell can wait for it)
	local n=$1 r=0
	: >"$tmp/$n.release"
	wait "${pids[$n]}" || r=$?
	rcs[$n]=$r
}
export FAKE_DIR=$tmp

# 3. two worktrees at once: both pass (the bug: the second exited 3 on the shared lock)
hold a "$tmp/main" hack/golangci-lint.sh run
rcb=0
(cd "$tmp/lane" && hack/golangci-lint.sh run >"$tmp/b.out" 2>&1) || rcb=$?
check "worktree 2 lints while worktree 1 holds its lock" eq "$rcb" 0
release a
check "worktree 1 lints while worktree 2 lints" eq "${rcs[a]}" 0

# 4. the same worktree twice at once: the lock still keeps them apart
hold c "$tmp/main" hack/golangci-lint.sh run
rcd=0
(cd "$tmp/main" && hack/golangci-lint.sh run >"$tmp/d.out" 2>&1) || rcd=$?
check "a second run in the same worktree still hits the lock" eq "$rcd" 3
release c
check "the first run in that worktree passes" eq "${rcs[c]}" 0

# 5. the bug itself, as a control: bare golangci-lint in two worktrees with one TMPDIR clashes
hold e "$tmp/main" golangci-lint run
rcf=0
(cd "$tmp/lane" && golangci-lint run >/dev/null 2>&1) || rcf=$?
check "control: without the wrapper the second worktree fails" eq "$rcf" 3
release e

# 5b. --clean deletes the worktree's directory and nothing else
(cd "$tmp/main" && hack/golangci-lint.sh --clean)
check "--clean deletes the worktree's directory" test ! -e "${mt%/tmp}"
check "--clean leaves another worktree's directory" test -d "$lt"

# 6. every repo script and the Makefile go through the wrapper
# Any golangci-lint invocation (a subcommand, after any flags) in a tracked script or makefile, except
# the wrapper, this test, preflight-test.sh's stub, comments and `command -v` probes.
bare=$(git -C "$repo" ls-files -z '*.sh' 'Makefile' '*.mk' | (cd "$repo" && xargs -0 grep -nE '(^|[^-/.[:alnum:]_"])golangci-lint([[:space:]]+-[^[:space:]]+)*[[:space:]]+(run|fmt|cache|config|linters|formatters|migrate)([[:space:]]|$)' 2>/dev/null) |
	grep -vE '^hack/golangci-lint(-test)?\.sh:|^hack/preflight-test\.sh:' | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#|command -v golangci-lint' || true)
check "no script runs bare golangci-lint run" eq "$bare" ""

if [[ $fail != 0 ]]; then echo "golangci-lint-test: FAILED" >&2; exit 1; fi
echo "golangci-lint-test: all passed"
