#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The merge queue (koinon-private#225) builds a commit that belongs to no pull request and waits for every
# required check on it. A workflow that produces a required context but does not listen to
# `merge_group` leaves that check absent, and the queue entry waits until it times out. This test
# keeps the workflows that produce a required context (hack/ci-required.txt) subscribed, and keeps
# the jobs that read pull request data from running on a queue build.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -uo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
wfdir=${1:-$repo/.github/workflows}

pass=0 fail=0
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then echo "ok: $d"; pass=$((pass + 1)); else echo "FAIL: $d" >&2; fail=$((fail + 1)); fi; }
# The workflows that produce a required context: ci.yml (the jobs), and the external ones named in
# the header of hack/ci-required.txt.
for wf in ci dco secrets claims e2e-kind; do
	f=$wfdir/$wf.yml
	check "$wf.yml listens to merge_group" grep -Eq '^  merge_group:' "$f"
	# shellcheck disable=SC2016
	check "$wf.yml asks for checks_requested only" awk '/^  merge_group:/ { getline; if ($0 ~ /types: \[checks_requested\]/) ok = 1 } END { exit !ok }' "$f"
done
# Every external name of ci-required.txt is produced by one of the workflows above.
# shellcheck disable=SC2016
check "ci-required.txt names those workflows for its external checks" bash -c '
	for w in dco.yml secrets.yml claims.yml e2e-kind.yml; do grep -q "\.github/workflows/$w" "$1" || exit 1; done' _ "$repo/hack/ci-required.txt"
# The jobs that are not part of a queue build stay off it: they read a pull request or push to main.
# shellcheck disable=SC2016
check "claims.yml: release and sweeper do not run on merge_group" bash -c '
	! awk "/^  (release|sweeper):/ { j = 1 } /^  claim:/ { j = 0 } j && /merge_group/ { bad = 1 } END { exit !bad }" "$1"' _ "$wfdir/claims.yml"
check "claims.yml: the claim job reports on merge_group" grep -Eq "if: github.event_name == 'merge_group' \|\|" "$wfdir/claims.yml"
# shellcheck disable=SC2016
check "claims.yml: on merge_group the claim job runs check-pr for the queued PR from the base sha" bash -c '
	grep -q "ref: \${{ github.event.merge_group.base_sha }}" "$1" && grep -q "gh-readonly-queue/\*/pr-" "$1" &&
	awk "/event_name == .merge_group.$/ { g = 1 } g && /claim.sh check-pr/ { ok = 1 } END { exit !ok }" "$1"' _ "$wfdir/claims.yml"
# shellcheck disable=SC2016
check "e2e-kind.yml: e2e result reads the queued PR head on merge_group" bash -c '
	grep -q "gh-readonly-queue/\*/pr-" "$1" && grep -q "headRefOid" "$1"' _ "$wfdir/e2e-kind.yml"
# shellcheck disable=SC2016
check "e2e-kind.yml: a queue build is never cancelled" bash -c '
	grep -q "cancel-in-progress: \${{ github.event_name == .pull_request. }}" "$1"' _ "$wfdir/e2e-kind.yml"

echo "$pass passed, $fail failed"
exit $((fail > 0))
