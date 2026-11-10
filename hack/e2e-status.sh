#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Post the commit status `e2e-lab` on a commit: the record that an external run of the kind e2e passed (or
# failed) on exactly that commit (old issue 547). The run's own driver calls it after the run; run it by
# hand to post a result again, for example when the commit was not pushed yet at the time.
#
#   hack/e2e-status.sh <sha> <success|failure> <description> [target url]
#
# The `e2e-lab` status is information only (ADR 0105): nothing in CI or in hack/pr-wait.sh, hack/pr-merge.sh reads
# it, and a success posts no re-run. The merge queue's `quickstart` is the one e2e proof for a merge. The status
# is never a required check of the ruleset.
#
# Posts with `gh api repos/{owner}/{repo}/statuses/<sha>` from the repository of the current directory.
# KOINON_E2E_NO_STATUS=1 posts nothing (local experiments). Exit 0 posted or skipped on purpose; 1 the
# post failed (most often: the commit is not on GitHub yet, push the branch and run this again);
# 64 usage. The caller treats a failed post as a warning: the run's own result is not affected.
#
# KOINON_STATUS_CONTEXT names another context (default e2e-lab). An external go-test run posts
# `go-test-lab` this way (old issue 610, hack/go-test-lab.sh).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat

[[ $# -ge 3 ]] || { echo "usage: hack/e2e-status.sh <sha> <success|failure> <description> [target url]" >&2; exit 64; }
sha=$1 state=$2 desc=$3 url=${4:-}
context=${KOINON_STATUS_CONTEXT:-e2e-lab}
[[ $context =~ ^(e2e-lab|go-test-lab)$ ]] || { echo "e2e-status: KOINON_STATUS_CONTEXT is e2e-lab or go-test-lab, not $context" >&2; exit 64; }
[[ $sha =~ ^[0-9a-f]{40}$ ]] || { echo "e2e-status: not a full commit sha: $sha" >&2; exit 64; }
case $state in success | failure) ;; *) echo "e2e-status: state is success or failure, not $state" >&2; exit 64 ;; esac

if [[ -n ${KOINON_E2E_NO_STATUS:-} ]]; then
  echo "e2e-status: KOINON_E2E_NO_STATUS is set; not posting $context=$state on ${sha:0:12}"
  exit 0
fi

# A status description is at most 140 characters.
desc=${desc:0:140}
args=(-X POST "repos/{owner}/{repo}/statuses/$sha" -f state="$state" -f context="$context" -f description="$desc")
[[ -z $url ]] || args+=(-f target_url="$url")

if gh api "${args[@]}" --silent 2>/dev/null; then
  echo "e2e-status: posted $context=$state on ${sha:0:12}"
  exit 0
fi
{
  echo "e2e-status: could not post $context=$state on ${sha:0:12} (is the commit pushed? is gh logged in?)"
  echo "  push the branch, then: KOINON_STATUS_CONTEXT=$context hack/e2e-status.sh $sha $state '<description>'${url:+ \"$url\"}"
} >&2
exit 1
