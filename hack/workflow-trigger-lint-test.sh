#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/workflow-trigger-lint.sh offline with fixture trees in a temp dir.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
script="$repo/hack/workflow-trigger-lint.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/workflow-trigger-lint-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
fail=0
n=0
# case_ <name> <want rc> <workflow content>
case_() {
	n=$((n + 1))
	local r=$tmp/r$n rc=0
	mkdir -p "$r/.github/workflows"
	printf '%s\n' "$3" >"$r/.github/workflows/w.yml"
	"$script" "$r" >/dev/null 2>&1 || rc=$?
	if [[ $rc == "$2" ]]; then printf 'ok   %s\n' "$1"; else printf 'FAIL %s (rc %s, want %s)\n' "$1" "$rc" "$2"; fail=1; fi
}

case_ "a map key is flagged" 1 $'on:\n  pull_request_target:\n    types: [closed]'
case_ "a flow list is flagged" 1 'on: [push, pull_request_target]'
case_ "a plain pull_request passes" 0 $'on:\n  pull_request:\n    types: [opened]'
case_ "a comment mention passes" 0 $'# no pull_request_target here\non:\n  push: {}  # not pull_request_target'
case_ "a longer name passes" 0 $'on:\n  pull_request_targets_x: {}'
case_ "an event_name comparison is flagged" 1 $'jobs:\n  a:\n    if: github.event_name == \'pull_request_target\''

# the real tree is clean
if "$script" "$repo" >/dev/null 2>&1; then echo "ok   the repository has no pull_request_target"; else echo "FAIL the repository uses pull_request_target"; fail=1; fi
exit "$fail"
