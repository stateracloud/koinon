#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/old-issue-refs-lint.sh offline with fixture trees in a temp dir.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
script="$repo/hack/old-issue-refs-lint.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/old-issue-refs-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
fail=0
ok() { printf 'ok   %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }
name="koinon-""private"

n=0
# case_ <name> <want rc> <content line>
case_() {
	n=$((n + 1))
	local r=$tmp/r$n rc
	mkdir -p "$r/docs"
	printf '%s\n' "$3" >"$r/docs/a.md"
	"$script" "$r" >/dev/null 2>&1
	rc=$?
	if [[ $rc -eq $2 ]]; then ok "$1"; else bad "$1 (rc $rc, want $2)"; fi
}

case_ "plain old issue text passes" 0 "fixed in old issue 615 and moved to #122"
case_ "bare issue reference fails" 1 "see $name#615"
case_ "link fails" 1 "[x](https://github.com/stateracloud/$name/issues/1)"
case_ "bare repo name fails" 1 "the $name repository"

# The real tree is clean.
if "$script" >/dev/null 2>&1; then ok "repository is clean"; else bad "repository still names the old repository"; fi

exit "$fail"
