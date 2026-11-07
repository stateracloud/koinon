#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Checks the concurrency rule of ci.yml (old issue 675): runs on one head never cancel each other, a push to
# main is in its own group, and the older heads are cancelled explicitly. Plus actionlint when it is
# installed (CI installs it for the workflow lint job; a machine without it skips that one line).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -uo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
wf=${1:-$repo/.github/workflows/ci.yml}

pass=0 fail=0
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then echo "ok: $d"; pass=$((pass + 1)); else echo "FAIL: $d" >&2; fail=$((fail + 1)); fi; }
# The top-level concurrency block: from "^concurrency:" to the next top-level key.
block=$(awk '/^concurrency:/ { f = 1; next } f && /^[^ #]/ { exit } f' "$wf")
# shellcheck disable=SC2329 # called through check
inblock() { grep -Eq -- "$1" <<<"$block"; }
# shellcheck disable=SC2329
notinblock() { ! grep -Eq -- "$1" <<<"$block"; }

check "the workflow has a concurrency block" test -n "$block"
check "pull_request runs are grouped by the head SHA" inblock 'github\.event\.pull_request\.head\.sha'
check "the group is not the ref (the old per-ref group)" notinblock 'github\.ref'
check "pushes have their own group" inblock "ci-push-"
check "merge group builds have their own group per queue commit (#225)" inblock "ci-mq-"
check "ci.yml listens to merge_group" grep -Eq '^  merge_group:' "$wf"
check "the supersede step is pull_request only" grep -Eq "if: github.event_name == 'pull_request'" "$wf"
check "cancel-in-progress is off" inblock 'cancel-in-progress: false'
check "cancel-in-progress is never true or an expression" notinblock 'cancel-in-progress: (true|\$)'
check "changes cancels older heads" grep -q 'hack/ci-supersede.sh' "$wf"
check "changes may cancel runs (actions: write)" grep -q 'actions: write # hack/ci-supersede.sh' "$wf"
check "ci-supersede-test runs in CI" grep -q 'hack/ci-supersede-test.sh' "$wf"
if command -v actionlint >/dev/null; then
	check "actionlint is clean on the workflow" actionlint "$wf"
else
	echo "skip: actionlint is not installed"
fi

echo "$pass passed, $fail failed"
exit $((fail > 0))
