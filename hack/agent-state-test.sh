#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/agent-state.sh on a temp state dir (KOINON_STATE_DIR); the real state files are never touched.
# shellcheck disable=SC2015  # ok and bad never fail, so A && ok || bad is an if-else here
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
script="$repo/hack/agent-state.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/agent-state-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
export KOINON_STATE_DIR="$tmp/state"
export KOINON_STATE_TODAY=2026-11-10
mkdir "$KOINON_STATE_DIR"
lanes="$KOINON_STATE_DIR/lanes.md"
pending="$KOINON_STATE_DIR/pending.md"
fail=0
ok() { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

run() { out=$("$script" "$@" 2>&1); rc=$?; }
expect() { # expect <name> <rc> [<substring in out>] [<substring not in out>]
	if [[ $rc -ne $2 ]]; then bad "$1: exit $rc, want $2 ($out)"; return; fi
	if [[ -n ${3:-} && $out != *"$3"* ]]; then bad "$1: output lacks '$3': $out"; return; fi
	if [[ -n ${4:-} && $out == *"$4"* ]]; then bad "$1: output has '$4': $out"; return; fi
	ok "$1"
}
has() { # has <name> <file> <fixed string>
	if grep -qF -- "$3" "$2"; then ok "$1"; else bad "$1: '$3' missing from $2"; fi
}
hasnt() {
	if grep -qF -- "$3" "$2"; then bad "$1: '$3' present in $2"; else ok "$1"; fi
}

# Existing content in the real format: a free first line, a log, a headed block.
printf 'HANDOFF 2026-11-02 ~08:30 CET (session x)\nONE lane open:\n- #507 old\n\n- 2026-11-02 ~09:35 resumed\n' >"$lanes"
printf 'Pending 2026-11-02 ~08:30 CET\n- Waiting on Andre: move #510 to Ready\n\n- 2026-11-02 ~09:50 Rivet: order #511\n- 2026-11-02 ~10:00 other thing' >"$pending"
cp "$lanes" "$tmp/lanes.orig"
cp "$pending" "$tmp/pending.orig"

# pending
run pending add "check #517 | with pipe"
expect "pending add" 0
has "pending add appends a dated line" "$pending" "- $(date '+%Y-%m-%d') ~"
has "pending add keeps the text (pipe cleaned)" "$pending" "check #517 / with pipe"
has "pending add added the missing final newline" "$pending" "- 2026-11-02 ~10:00 other thing"
run pending "done" "Rivet: order"
expect "pending done" 0 "marked done: 1"
has "pending done marks in place" "$pending" "Rivet: order #511 [done $(date '+%Y-%m-%d') ~"
has "pending done keeps the leading date" "$pending" "- 2026-11-02 ~09:50 Rivet: order #511"
cp "$pending" "$tmp/pending.after1"
run pending "done" "Rivet: order"
expect "pending done twice" 0 "already done"
cmp -s "$pending" "$tmp/pending.after1" && ok "second done leaves the file identical" || bad "second done changed the file"
run pending "done" "no such text anywhere"
expect "pending done without a match" 1 "no pending item matches"
run pending add
expect "pending add without text" 2
for s in "Pending 2026-11-02 ~08:30 CET" "- Waiting on Andre: move #510 to Ready" "- 2026-11-02 ~10:00 other thing"; do
	has "pending keeps: $s" "$pending" "$s"
done

# lanes
run lane add 517 local feat/517-x '#556' "first note"
expect "lane add" 0
run lane add 518 cloud feat/518-y '#557'
expect "lane add second" 0
run lane add 517 local b '#1'
expect "lane add duplicate" 1 "already open"
run lane add abc local b '#1'
expect "lane add bad number" 2
head -1 "$lanes" | grep -qF '<!-- agent-state:lanes begin -->' && ok "table is at the top" || bad "table is not at the top"
has "table row" "$lanes" "| #517 | local | feat/517-x | #556 | first note |"
has "second row" "$lanes" "| #518 | cloud | feat/518-y | #557 |  |"
has "log line" "$lanes" "lane add #517 local feat/517-x #556: first note"
run lane set 517 pr='#600' note="ready for review"
expect "lane set" 0
has "set changes the row" "$lanes" "| #517 | local | feat/517-x | #600 | ready for review |"
hasnt "set replaces the old row" "$lanes" "| #517 | local | feat/517-x | #556 |"
run lane set 517 color=red
expect "lane set unknown field" 2 "unknown field"
run lane set 999 pr=1
expect "lane set unknown lane" 1 "not open"
run lane "done" 518
expect "lane done" 0
hasnt "done drops the row" "$lanes" "| #518 |"
has "done is logged" "$lanes" "lane done #518"
run lane "done" 518
expect "lane done twice" 1 "not open"
for s in "HANDOFF 2026-11-02 ~08:30 CET (session x)" "ONE lane open:" "- #507 old" "- 2026-11-02 ~09:35 resumed"; do
	has "lanes keeps: $s" "$lanes" "$s"
done
# the original text is still there, in order, after the table
grep -vF -e agent-state -e '| ' -e '|---' -e 'lane add' -e 'lane set' -e 'lane done' "$lanes" | grep -v '^## Open lanes' | grep -v '^$' >"$tmp/lanes.now"
grep -v '^$' "$tmp/lanes.orig" | cmp -s - "$tmp/lanes.now" && ok "original lanes content unchanged and in order" || bad "original lanes content changed"

# show
KOINON_STATE_TODAY=2026-11-04 run show
expect "show" 0 "#517 local feat/517-x PR #600 - ready for review" "#518"
[[ $out == *"Open pending"* && $out == *"- Waiting on Andre"* && $out != *"Rivet: order"* ]] && ok "show lists open pending only" || bad "show pending wrong: $out"
rm -rf "$KOINON_STATE_DIR"
mkdir "$KOINON_STATE_DIR"
run show
expect "show on an empty dir" 0 "Open lanes:"


# one match only; --all, --line, --before
printf -- '- 2026-10-01 ~09:00 alpha old\n- 2026-10-02 ~09:00 alpha older\n- 2026-11-09 ~09:00 alpha new\n- undated alpha\n' >"$pending"
run pending "done" alpha
expect "done with several matches refuses" 1 "4 open items" 
[[ $out == *"1: - 2026-10-01"* && $out == *"3: - 2026-11-09"* ]] && ok "refusal lists them numbered" || bad "refusal list: $out"
hasnt "refusal changes nothing" "$pending" "[done"
run pending "done" --line 2
expect "done --line" 0 "marked done: 1"
has "--line marks that line" "$pending" "alpha older [done"
hasnt "--line marks only that line" "$pending" "alpha old [done"
run pending "done" --line 2
expect "done --line twice" 0 "already done"
run pending "done" --line 99
expect "done --line out of range" 1 "no pending item"
run show
expect "show hides items older than 3 days" 0 "3: - 2026-11-09" "alpha old"
[[ $out == *"(1 older open items: show --all)"* && $out == *"4: - undated alpha"* ]] && ok "show counts the older ones, keeps undated" || bad "show older note: $out"
run show --all
expect "show --all" 0 "1: - 2026-10-01 ~09:00 alpha old"
run pending "done" --before 2026-11-01
expect "done --before" 0 "marked done: 1"
has "--before marks the old item" "$pending" "alpha old [done"
hasnt "--before spares newer and undated" "$pending" "alpha new [done"
hasnt "--before spares undated" "$pending" "undated alpha [done"
run pending "done" --all alpha
expect "done --all" 0 "marked done: 2"
run pending "done" --before 2026-11-01
expect "done --before twice" 0 "already done"

# mode kept, symlink written through
printf -- '- 2026-11-09 ~09:00 mode\n' >"$pending"
chmod 640 "$pending"
run pending add "mode check"
expect "add on a 640 file" 0
[[ $(stat -c %a "$pending" 2>/dev/null || stat -f %Lp "$pending") == 640 ]] && ok "existing mode kept" || bad "mode changed"
rm -f "$lanes" "$pending"
run pending add "fresh file"
[[ $(stat -c %a "$pending" 2>/dev/null || stat -f %Lp "$pending") == 600 ]] && ok "new file is 600" || bad "new file mode"
mkdir "$tmp/real"
printf -- '- 2026-11-09 ~09:00 linked\n' >"$tmp/real/pending.md"
rm -f "$pending"
ln -s "$tmp/real/pending.md" "$pending"
run pending add "through link"
expect "add through a symlink" 0
[[ -L $pending ]] && ok "symlink kept" || bad "symlink replaced"
has "target was written" "$tmp/real/pending.md" "through link"
rm -f "$pending"

# one write per lane command: the row and its log line appear together (a single rename)
run lane add 7 l b '#1'
n=$(grep -c -e '| #7 |' -e 'lane add #7' "$lanes")
[[ $n -eq 2 ]] && ok "lane add wrote table and log together" || bad "lane add wrote $n of 2"
run lane "done" 7
rm -f "$lanes"

# default dir honours XDG_CONFIG_HOME
xdg_out=$(env -u KOINON_STATE_DIR XDG_CONFIG_HOME="$tmp/xdg" "$script" pending add "xdg" 2>&1)
[[ -f $tmp/xdg/koinon/state/pending.md ]] && ok "XDG_CONFIG_HOME default dir" || bad "XDG default: $xdg_out"

# stale locks
mkdir "$KOINON_STATE_DIR/.lock"
bash -c 'echo $$' >"$KOINON_STATE_DIR/.lock/pid"   # a pid whose process has exited
KOINON_STATE_LOCK_TIMEOUT=2 run pending add "after a killed writer"
expect "lock of a dead pid is reclaimed" 0
has "write went through" "$pending" "after a killed writer"
mkdir "$KOINON_STATE_DIR/.lock"
echo $$ >"$KOINON_STATE_DIR/.lock/pid"
KOINON_STATE_LOCK_TIMEOUT=1 run pending add "blocked by a live pid"
expect "lock of a live pid is kept" 1 "could not take"
rm -rf "$KOINON_STATE_DIR/.lock"
mkdir "$KOINON_STATE_DIR/.lock"
KOINON_STATE_LOCK_TIMEOUT=1 run pending add "blocked fresh pidless"
expect "fresh pidless lock is kept" 1 "could not take"
touch -t 200001010000 "$KOINON_STATE_DIR/.lock"
KOINON_STATE_LOCK_TIMEOUT=2 run pending add "after an old pidless lock"
expect "old pidless lock is reclaimed" 0
rm -f "$lanes" "$pending"

# concurrent writers: no line lost
rm -f "$lanes" "$pending"
pids=()
for w in 1 2 3 4; do
	(for i in 1 2 3 4 5 6 7 8 9 10; do "$script" pending add "writer $w line $i" >/dev/null 2>&1; done) &
	pids+=($!)
done
(for i in 1 2 3 4 5 6 7 8 9 10; do
	"$script" lane add $((100 + i)) l b "#$i" >/dev/null 2>&1
done) &
pids+=($!)
wait "${pids[@]}"
n=$(grep -c 'writer . line' "$pending")
[[ $n -eq 40 ]] && ok "40 concurrent pending adds, none lost" || bad "concurrent pending adds: $n of 40"
n=$(grep -c '^| #1' "$lanes")
[[ $n -eq 10 ]] && ok "10 lane adds beside them, none lost" || bad "concurrent lane adds: $n of 10"
[[ ! -e $KOINON_STATE_DIR/.lock ]] && ok "lock released" || bad "lock dir left behind"
[[ -z $(find "$KOINON_STATE_DIR" -name '.tmp.*') ]] && ok "no temp files left" || bad "temp files left"

# a held lock times out
mkdir "$KOINON_STATE_DIR/.lock"
KOINON_STATE_LOCK_TIMEOUT=1 run pending add "blocked"
expect "held lock times out" 1 "could not take"
rmdir "$KOINON_STATE_DIR/.lock"

exit $fail
