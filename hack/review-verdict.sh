#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Post the commit status `review-verdict` on a pull request's head: the record that an independent
# review of exactly that commit found it ready (success) or not ready (failure) (ADR 0103). The
# required-to-be check `review` (hack/review-gate.sh, .github/workflows/review.yml) accepts a success
# on the head of a pull request that touches the sensitive paths.
#
#   hack/review-verdict.sh <PR> <ready|not-ready> <reviewer> <reviewed sha> [note]
#
# <reviewer> names who reviewed (for example `opus-review` or a GitHub login); it goes into the status
# description with the verdict and the note (at most 140 characters in all). The script refuses when
# the pull request is not open or its head is no longer <reviewed sha>: a verdict is valid for the
# commit that was reviewed, and a push makes it stale. After it posts, it re-runs the `review` job of
# the pull request's newest finished review run on that commit when the job's result disagrees with
# the verdict (a status event starts no workflow).
#
# Trust limit: the status is posted by whoever runs this, normally the maintainer's own account, so it
# records that a review happened; it does not separate duties.
#
# Posts with `gh api repos/{owner}/{repo}/statuses/<sha>` (GH_REPO, else the repository of the current
# directory). GH overrides the gh command (tests). Exit 0 posted; 1 the post failed; 3 the pull
# request is not open; 4 its head is not <reviewed sha> (STALE); 64 usage.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat
GH=${GH:-gh}

usage() { echo "usage: hack/review-verdict.sh <PR> <ready|not-ready> <reviewer> <reviewed sha> [note]" >&2; exit 64; }
[[ $# -ge 4 && $# -le 5 ]] || usage
pr=$1 verdict=$2 reviewer=$3 sha=$4 note=${5:-}
[[ $pr =~ ^[0-9]+$ ]] || { echo "review-verdict: not a pull request number: $pr" >&2; usage; }
case $verdict in
ready) state=success ;;
not-ready) state=failure ;;
*) echo "review-verdict: the verdict is ready or not-ready, not $verdict" >&2; usage ;;
esac
[[ $reviewer =~ ^[A-Za-z0-9][A-Za-z0-9._@/+-]{0,39}$ ]] || { echo "review-verdict: the reviewer is 1 to 40 characters of [A-Za-z0-9._@/+-]: $reviewer" >&2; usage; }
[[ $sha =~ ^[0-9a-f]{40}$ ]] || { echo "review-verdict: not a full commit sha: $sha" >&2; usage; }
[[ $note != *$'\n'* ]] || { echo "review-verdict: the note is one line" >&2; usage; }

repo=${GH_REPO:-'{owner}/{repo}'}
info=$("$GH" pr view "$pr" --json state,headRefOid --jq '.state + " " + .headRefOid' 2>&1) ||
  { echo "review-verdict: cannot read pull request #$pr: $info" >&2; exit 1; }
read -r prstate head <<<"$info"
[[ $prstate == OPEN ]] || { echo "review-verdict: pull request #$pr is $prstate, not open; nothing posted" >&2; exit 3; }
if [[ $head != "$sha" ]]; then
  echo "review-verdict: STALE: pull request #$pr's head is ${head:0:12}, not the reviewed ${sha:0:12}; review the new head, then post for it. Nothing posted." >&2
  exit 4
fi

desc="$verdict: reviewed by $reviewer${note:+: $note}"
desc=${desc:0:140}
if ! out=$("$GH" api -X POST "repos/$repo/statuses/$sha" -f state="$state" -f context=review-verdict -f description="$desc" --silent 2>&1); then
  echo "review-verdict: could not post review-verdict=$state on ${sha:0:12}: $out" >&2
  exit 1
fi
echo "review-verdict: posted review-verdict=$state on ${sha:0:12} (#$pr): $desc"

# Re-run the pull request's `review` job when its result disagrees with the verdict (best effort).
want=$([[ $state == success ]] && echo failure || echo success)
run=$("$GH" run list --workflow review.yml --commit "$sha" --event pull_request --limit 1 \
  --json databaseId,status,conclusion --jq '.[] | [.databaseId, .status, .conclusion] | @tsv' 2>/dev/null || true)
id="" rstatus="" rconcl=""
[[ -z $run ]] || IFS=$'\t' read -r id rstatus rconcl <<<"$run"
if [[ -z $id ]]; then
  echo "review-verdict: no review run on ${sha:0:12} yet; the next run reads the verdict"
  exit 0
fi
if [[ $rstatus != completed ]]; then
  echo "review-verdict: review run $id is $rstatus; if it ends with the wrong result, use \"Re-run all jobs\" on it"
  exit 0
fi
if [[ $rconcl != "$want" ]]; then
  echo "review-verdict: review run $id is already $rconcl; nothing to re-run"
  exit 0
fi
job=$("$GH" api "repos/$repo/actions/runs/$id/jobs?per_page=100" --jq '.jobs[] | select(.name == "review") | .id' 2>/dev/null | head -n 1 || true)
if [[ -n $job ]] && "$GH" run rerun "$id" --job "$job" >/dev/null 2>&1; then
  echo "review-verdict: re-running review (job $job) of run $id on ${sha:0:12}"
else
  echo "review-verdict: could not re-run the review job of run $id; use \"Re-run all jobs\" on it" >&2
fi
exit 0
