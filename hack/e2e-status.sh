#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Post the commit status `e2e-lab` on a commit: the record that a lab run of the kind e2e passed (or
# failed) on exactly that commit (#547). hack/e2e-remote.sh --argo calls it after the run; run it by
# hand to post a result again, for example when the commit was not pushed yet at the time.
#
#   hack/e2e-status.sh <sha> <success|failure> <description> [target url]
#
# `hack/pr-wait.sh <PR> --checks --require e2e` accepts a green `e2e-lab` on the PR head. The status is
# not a required check of the ruleset (contributors without a lab cannot produce it).
#
# Posts with `gh api repos/{owner}/{repo}/statuses/<sha>` from the repository of the current directory.
# KOINON_E2E_NO_STATUS=1 posts nothing (local experiments). Exit 0 posted or skipped on purpose; 1 the
# post failed (most often: the commit is not on GitHub yet, push the branch and run this again);
# 64 usage. The caller treats a failed post as a warning: the run's own result is not affected.
set -euo pipefail
export GH_PAGER=cat PAGER=cat

[[ $# -ge 3 ]] || { echo "usage: hack/e2e-status.sh <sha> <success|failure> <description> [target url]" >&2; exit 64; }
sha=$1 state=$2 desc=$3 url=${4:-}
[[ $sha =~ ^[0-9a-f]{40}$ ]] || { echo "e2e-status: not a full commit sha: $sha" >&2; exit 64; }
case $state in success | failure) ;; *) echo "e2e-status: state is success or failure, not $state" >&2; exit 64 ;; esac

if [[ -n ${KOINON_E2E_NO_STATUS:-} ]]; then
  echo "e2e-status: KOINON_E2E_NO_STATUS is set; not posting e2e-lab=$state on ${sha:0:12}"
  exit 0
fi

# A status description is at most 140 characters.
desc=${desc:0:140}
args=(-X POST "repos/{owner}/{repo}/statuses/$sha" -f state="$state" -f context=e2e-lab -f description="$desc")
[[ -z $url ]] || args+=(-f target_url="$url")

if gh api "${args[@]}" --silent 2>/dev/null; then
  echo "e2e-status: posted e2e-lab=$state on ${sha:0:12}"
  exit 0
fi
{
  echo "e2e-status: could not post e2e-lab=$state on ${sha:0:12} (is the commit pushed? is gh logged in?)"
  echo "  push the branch, then: hack/e2e-status.sh $sha $state '<description>'${url:+ \"$url\"}"
} >&2
exit 1
