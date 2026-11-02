#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/mktemp-lint.sh offline with fixture trees in a temp dir.
# shellcheck disable=SC2015  # ok and bad never fail, so A && ok || bad is an if-else here
# shellcheck disable=SC2016  # fixtures are written with single quotes on purpose
set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
script="$repo/hack/mktemp-lint.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/mktemp-lint-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
fail=0
ok() { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

n=0
# case <name> <want rc> <file under root> <content line>
case_() {
	n=$((n + 1))
	local r=$tmp/r$n out rc
	mkdir -p "$r/$(dirname "$3")"
	printf '%s\n' "$4" >"$r/$3"
	out=$("$script" "$r" 2>&1)
	rc=$?
	if [[ $rc -eq $2 ]]; then ok "$1"; else bad "$1: exit $rc, want $2 ($out)"; fi
}

case_ "bare mktemp"             1 hack/a.sh 'x=$(mktemp)'
case_ "bare mktemp -d"          1 hack/a.sh 'x=$(mktemp -d)'
case_ "mktemp -t x"             1 hack/a.sh 'x=$(mktemp -t x)'
case_ "mktemp -q"               1 hack/a.sh 'x=$(mktemp -q)'
case_ "bare in a pipeline"      1 test/b.sh 'cd /x && f=$(mktemp -d) || exit 1'
case_ "default-valued bare"     1 hack/a.sh 'OUT=${OUT:-$(mktemp -d)}'
case_ "backtick bare"           1 hack/a.sh 'x=`mktemp`'
case_ "bare in the Makefile"    1 Makefile 'x: ; t=$$(mktemp -d); true'
case_ "bare in a workflow"      1 .github/workflows/ci.yml '      - run: d=$(mktemp -d)'
case_ "template file"           0 hack/a.sh 'x=$(mktemp "${TMPDIR:-/tmp}/a.XXXXXX")'
case_ "template dir"            0 hack/a.sh 'x=$(mktemp -d "${TMPDIR:-/tmp}/a.XXXXXX") || exit 1'
case_ "template under a var"    0 hack/a.sh 'w=$(mktemp -d "$tmp/w.XXXXXX")'
case_ "comment only"            0 hack/a.sh '# call mktemp -d here is fine in a comment'
case_ "indented comment"        0 hack/a.sh '   # x=$(mktemp)'
case_ "word in a string"        0 hack/a.sh 'echo "mktemp gave no directory" >&2'
case_ "word in a for list"      0 hack/a.sh 'for t in git rm mktemp tr; do :; done'
case_ "stub file path"          0 hack/a.sh 'chmod +x "$tmp/failbin/mktemp"'
case_ "mktemp_fail_rc var"      0 hack/a.sh 'mktemp_fail_rc=0'
case_ "excluded file (#512)"    0 hack/e2e-remote.sh 'x=$(mktemp -d)'
case_ "not a script"            0 docs/x.md 'x=$(mktemp -d)'

# A mixed file reports the bad line and its number.
r=$tmp/mixed
mkdir -p "$r/hack"
printf '%s\n' 'a=$(mktemp -d "${TMPDIR:-/tmp}/m.XXXXXX")' 'b=$(mktemp)' >"$r/hack/m.sh"
out=$("$script" "$r" 2>&1)
rc=$?
[[ $rc -eq 1 && $out == *"hack/m.sh:2:"* && $out != *"hack/m.sh:1:"* ]] && ok "reports the file and line" || bad "reports the file and line: $rc $out"

# The real tree is clean.
out=$("$script" 2>&1) && ok "repository is clean" || bad "repository has bare mktemp calls: $out"

exit "$fail"
