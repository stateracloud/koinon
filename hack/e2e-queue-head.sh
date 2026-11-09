#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# BOOTSTRAP SHIM (#444, ADR 0102; remove with the follow-up issue, see PR #451).
#
# The merge_group `decide` job runs hack/e2e-decide.sh from merge_group.base_sha. For the queue entry
# that merges #444 itself, and for any entry built on a main older than #444, that is the OLD decide: it
# answers needed=false, required=true (ADR 0094 decision 4), so the queue commit gets no quickstart and
# `e2e result` must find the evidence on the queued pull request's head, as it did before.
#
# Prints `SHA=<head sha>` and `BRANCH=<head branch>` of the pull request queued in $GITHUB_REF
# (gh-readonly-queue/<base>/pr-<N>-<sha>) for hack/e2e-gate.sh. Fails closed (exit 1).
#
# Exit condition: remove this script, its call in .github/workflows/e2e-kind.yml and its cases in
# hack/ci-merge-group-test.sh once main's decide is the one from #444 and no queue entry built on an
# older base can remain (the first queue entry after #444 merged has merged or left the queue).
#
# Environment: GITHUB_REF, GH (the gh command), GH_REPO.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -euo pipefail

GH=${GH:-gh}
ref=${GITHUB_REF:?GITHUB_REF}
n=${ref#refs/heads/gh-readonly-queue/*/pr-}
n=${n%%-*}
if [[ ! $n =~ ^[0-9]+$ ]]; then echo "::error::cannot read the pull request number from $ref" >&2; exit 1; fi
sha=$("$GH" pr view "$n" --json headRefOid --jq .headRefOid)
if [[ -z $sha ]]; then echo "::error::cannot read the head of pull request $n" >&2; exit 1; fi
branch=$("$GH" pr view "$n" --json headRefName --jq .headRefName)
echo "merge queue entry for pull request $n, head $sha (old-style decide; the PR head carries the evidence)" >&2
printf 'SHA=%s\nBRANCH=%s\n' "$sha" "$branch"
