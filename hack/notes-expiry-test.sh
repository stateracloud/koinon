#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/notes-expiry.sh offline with fixtures in a temp dir. The clock is pinned with --today.
# shellcheck disable=SC2015  # ok and bad never fail, so A && ok || bad is an if-else here

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
script="$repo/hack/notes-expiry.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/notes-expiry.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
fail=0
ok() { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

# run <args...>: sets $out and $rc.
run() { out=$("$script" "$@" 2>"$tmp/err"); rc=$?; }
expect() { # expect <name> <rc> [<substring that must be in out>] [<substring that must not>]
	if [[ $rc -ne $2 ]]; then bad "$1: exit $rc, want $2 ($out)"; return; fi
	if [[ -n ${3:-} && $out != *"$3"* ]]; then bad "$1: output lacks '$3': $out"; return; fi
	if [[ -n ${4:-} && $out == *"$4"* ]]; then bad "$1: output has '$4': $out"; return; fi
	ok "$1"
}

d="$tmp/notes"
mkdir "$d"
printf -- '---\nname: old\ndescription: an expired note\nmetadata:\n  type: project\n---\nbody\n' >"$d/noexp.md"
printf -- '---\nname: expired\ndescription: "an expired note"\nexpires: 2026-10-01\n---\nbody\n' >"$d/expired.md"
printf -- '---\nname: review\ndescription: past review date\nreview_by: 2026-09-30\n---\n' >"$d/review.md"
printf -- '---\nname: current\ndescription: still good\nexpires: 2026-12-01\n---\n' >"$d/current.md"
printf -- '---\nname: today\ndescription: expires today, still fine\nexpires: 2026-11-02\n---\n' >"$d/today.md"
printf -- '---\nname: durable\ndescription: standing rule\ndurable: true\n---\n' >"$d/durable.md"
printf -- '---\nname: nested\ndescription: nested field\nmetadata:\n  expires: 2026-01-01\n---\n' >"$d/nested.md"
printf -- '---\nname: bad\ndescription: malformed date\nexpires: soon\n---\n' >"$d/bad.md"
printf -- 'no frontmatter, first line is the text\n' >"$d/plain.md"
printf -- '- index line\n' >"$d/MEMORY.md"
printf 'x\n' >"$d/ignored.txt"

run "$d" --today 2026-11-02 --exclude MEMORY.md
expect "directory lists something" 1
for n in noexp expired review nested bad plain; do
	[[ $out == *"$d/$n.md"* ]] && ok "listed: $n" || bad "not listed: $n"
done
for n in current today durable MEMORY ignored; do
	[[ $out == *"/$n."* ]] && bad "wrongly listed: $n" || ok "not listed: $n"
done
[[ $out == *"$d/expired.md"$'\t'"2026-10-01"$'\t'"an expired note"* ]] && ok "line shape: path, date, text" || bad "line shape: $out"
[[ $out == *"$d/noexp.md"$'\t'"no-expiry"* ]] && ok "missing field shows no-expiry" || bad "no-expiry column: $out"

# MEMORY.md is listed without --exclude.
run "$d" --today 2026-11-02
[[ $out == *"MEMORY.md"* ]] && ok "no --exclude lists MEMORY.md" || bad "MEMORY.md not listed by default"

# Nothing expired: exit 0.
clean="$tmp/clean"
mkdir "$clean"
printf -- '---\nname: a\nexpires: 2027-01-01\n---\n' >"$clean/a.md"
printf -- '---\nname: b\ndurable: true\n---\n' >"$clean/b.md"
run "$clean" --today 2026-11-02
expect "clean directory exits 0" 0
[[ -z $out ]] && ok "clean directory prints nothing" || bad "clean directory printed: $out"
run "$clean" --today 2027-01-02
expect "same directory, later clock" 1 "a.md"

# Long descriptions are cut to 80 characters.
long=$(printf 'x%.0s' $(seq 1 120))
mkdir "$tmp/long"
printf -- '---\ndescription: %s\n---\n' "$long" >"$tmp/long/l.md"
run "$tmp/long" --today 2026-11-02
text=${out##*$'\t'}
((${#text} == 80)) && ok "text cut to 80 characters" || bad "text length ${#text}"

# Log mode.
log="$tmp/pending.md"
printf 'Pending header, not a bullet\n- 2026-10-01 ~08:00 old bullet to drop\n- 2026-10-19 ~09:00 exactly 14 days old\n- 2026-10-18 ~09:00 15 days old\n  continuation line\n- 2026-11-01 ~10:00 fresh\n- undated bullet\n' >"$log"
run --log "$log" --days 14 --today 2026-11-02
expect "log lists old bullets" 1 "old bullet to drop"
[[ $out == *"15 days old"* ]] && ok "log: 15 days old is listed" || bad "log: 15 days old missing"
[[ $out != *"exactly 14"* && $out != *fresh* && $out != *undated* ]] && ok "log: 14 days, fresh and undated are kept" || bad "log: wrong keep set: $out"
[[ $out == "2"$'\t'"2026-10-01"$'\t'* ]] && ok "log: line number and date lead" || bad "log line shape: $out"
run --log "$log" --days 60 --today 2026-11-02
expect "log: nothing older than 60 days" 0
run --log "$log" --days 14 --today 2026-11-02
# year and leap-day boundary
printf -- '- 2024-02-29 leap\n- 2025-03-01 after\n' >"$tmp/leap.md"
run --log "$tmp/leap.md" --days 365 --today 2025-03-01
expect "log: leap day is 366 days before 2025-03-01" 1 "leap" "after"

# Usage errors: exit 2.
run
expect "no arguments" 2
run "$tmp/nope"
expect "missing directory" 2
run "$d" --bogus
expect "unknown option" 2
run --log "$log"
expect "--log without --days" 2
run --log "$log" --days many
expect "non-numeric --days" 2
run --log "$tmp/nope.md" --days 3
expect "missing log file" 2
run "$d" --log "$log" --days 3
expect "directory and --log together" 2
run "$d" --today yesterday
expect "bad --today" 2
run "$d" "$d"
expect "two directories" 2

exit "$fail"
