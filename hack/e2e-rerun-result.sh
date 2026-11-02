#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Re-run the failed jobs of the pull request's e2e-kind run on a commit, i.e. its `e2e result`, so it
# looks again for the e2e evidence that arrived later (the lab's e2e-lab status, a dispatched run that
# passed). hack/e2e-gate.sh explains why a status event cannot do this by itself.
#
#   hack/e2e-rerun-result.sh <sha>
#
# Best effort: exit 0 also when there is nothing to re-run or gh cannot do it (the message says so);
# a person re-runs the job by hand then. Only a run that has finished and failed is re-run, only the
# pull_request ones, and only their failed jobs. GH_REPO or the repository of the current directory.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat
GH=${GH:-gh}

[[ $# -eq 1 && $1 =~ ^[0-9a-f]{40}$ ]] || { echo "usage: hack/e2e-rerun-result.sh <full commit sha>" >&2; exit 64; }
sha=$1

id=$("$GH" run list --workflow e2e-kind.yml --commit "$sha" --event pull_request --limit 1 \
  --json databaseId,status,conclusion --jq '.[] | select(.status == "completed" and .conclusion == "failure") | .databaseId' 2>/dev/null || true)
if [[ -z $id ]]; then
  echo "e2e-rerun-result: no failed pull request e2e-kind run on ${sha:0:12} to re-run"
  exit 0
fi
if "$GH" run rerun "$id" --failed >/dev/null 2>&1; then
  echo "e2e-rerun-result: re-running the failed jobs of e2e-kind run $id on ${sha:0:12}"
else
  echo "e2e-rerun-result: could not re-run run $id; use \"Re-run failed jobs\" on it" >&2
fi
exit 0
