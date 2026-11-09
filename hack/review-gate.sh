#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The review workflow's `review` job (ADR 0103): does this pull request need a recorded review, and
# when it does, does its head carry one?
#
#   - A pull request that touches none of the sensitive paths passes. The paths are the ones
#     `hack/claim.sh sensitive-paths` prints (the `# Sensitive paths` section of .github/CODEOWNERS,
#     the one source of ADR 0082 decision 9), matched as ruleset patterns (`dir/**` or an exact path).
#     Renamed files count by both names.
#   - Otherwise the head commit needs the commit status `review-verdict` = success, posted with
#     hack/review-verdict.sh after an independent review of exactly that commit. The newest
#     `review-verdict` status counts; a push makes it stale (a verdict is valid for its SHA only).
#   - A push to main (or any event without a pull request) passes: rulesets judge pull request heads
#     and queue commits only.
#   - merge_group: the queued pull request's number comes from the queue ref
#     (gh-readonly-queue/<base>/pr-<N>-<sha>); its head and file list are read from the API and judged
#     as above. Fails closed when the number or the head cannot be read.
#
# Every read that fails (the sensitive paths, the file list, the statuses) fails the gate; nothing
# falls back to "not sensitive". A status event starts no workflow, so hack/review-verdict.sh re-runs
# this job after it posts.
#
# Trust limit (ADR 0103): the account that posts `review-verdict` is the maintainer's own; this is a
# record that a review took place, not a separation of duties.
#
# Environment: EVENT (github.event_name), PR (the pull request number, pull_request[_target]), SHA (its
# head, pull_request[_target]), GITHUB_REF (merge_group), GH (the gh command), GH_REPO, CLAIM (the claim.sh to ask,
# default the one next to this script), GITHUB_STEP_SUMMARY. Exit 0 pass, 1 fail.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat

EVENT=${EVENT:-} PR=${PR:-} SHA=${SHA:-} GH=${GH:-gh} GH_REPO=${GH_REPO:-}
CLAIM=${CLAIM:-$(cd "$(dirname "$0")" && pwd)/claim.sh}

summary() { [[ -z ${GITHUB_STEP_SUMMARY:-} ]] || printf '## review\n\n%s\n' "$1" >>"$GITHUB_STEP_SUMMARY"; }
pass() { echo "review: $1"; summary "$1"; exit 0; }
fail() {
  echo "::error::$1"
  [[ -z ${2:-} ]] || echo "$2"
  summary "$1${2:+

$2}"
  exit 1
}

case $EVENT in
pull_request | pull_request_target) ;;
merge_group)
  ref=${GITHUB_REF:-}
  n=${ref#refs/heads/gh-readonly-queue/*/pr-}
  n=${n%%-*}
  [[ $ref == refs/heads/gh-readonly-queue/*/pr-* && $n =~ ^[0-9]+$ ]] || fail "cannot read the pull request number from the queue ref '$ref'"
  PR=$n
  SHA=$("$GH" pr view "$PR" --json headRefOid --jq .headRefOid 2>/dev/null) || SHA=""
  ;;
*) pass "no pull request on a $EVENT event: nothing to review" ;;
esac
[[ $PR =~ ^[0-9]+$ ]] || fail "no pull request number (PR='$PR')"
[[ -n $GH_REPO ]] || fail "GH_REPO is not set"
[[ $SHA =~ ^[0-9a-f]{40}$ ]] || fail "cannot read the head commit of pull request #$PR (got '$SHA')"
echo "pull request #$PR, head $SHA"

# The sensitive paths. KOINON_CLAIM_REPO spares claim.sh its own repository lookup.
patterns=$(KOINON_CLAIM_REPO=$GH_REPO "$CLAIM" sensitive-paths 2>&1) || fail "hack/claim.sh sensitive-paths failed: $patterns"
[[ -n $patterns ]] || fail "hack/claim.sh sensitive-paths printed no patterns; the gate cannot tell what is sensitive"

# The pull request's files (GitHub lists at most 3000; a list that long is treated as sensitive).
files=$("$GH" api --paginate "repos/$GH_REPO/pulls/$PR/files?per_page=100" --jq '.[] | .filename, (.previous_filename // empty)' 2>&1) ||
  fail "cannot read the files of pull request #$PR: $files"

hits=()
if [[ -z $files ]]; then
  hits=("(the file list is empty: treated as sensitive)")
elif (($(wc -l <<<"$files") >= 3000)); then
  hits=("(3000 or more files: GitHub truncates the list, treated as sensitive)")
else
  while IFS= read -r f; do
    while IFS= read -r p; do
      [[ -n $p ]] || continue
      # Ruleset patterns here are `dir/**` or an exact path; in [[ ]] a `*` also matches `/`.
      # shellcheck disable=SC2053 # $p is a pattern on purpose
      if [[ $f == $p ]]; then hits+=("$f ($p)"); break; fi
    done <<<"$patterns"
  done <<<"$files"
fi
((${#hits[@]})) || pass "pull request #$PR touches no sensitive path; no recorded review needed"
echo "sensitive paths touched (${#hits[@]}):"
printf '  %s\n' "${hits[@]:0:20}"
((${#hits[@]} <= 20)) || echo "  ... and $((${#hits[@]} - 20)) more"

# The newest review-verdict status on the head. An API error is reported as such, not as "no verdict".
resp=$("$GH" api "repos/$GH_REPO/commits/$SHA/statuses?per_page=100" 2>&1) ||
  fail "cannot read the commit statuses of ${SHA:0:12}: $resp (the job needs statuses: read)"
verdict=$(jq -r '[.[] | select(.context == "review-verdict")][0] // {} | (.state // ""), (.creator.login // ""), ((.description // "") | gsub("[\r\n]"; " "))' <<<"$resp" 2>/dev/null) ||
  fail "the commit statuses of ${SHA:0:12} are not valid JSON"
state="" creator="" desc=""
{ read -r state; read -r creator; read -r desc; } <<<"$verdict" || true

[[ $state != success ]] || pass "review-verdict is success on ${SHA:0:12} (posted by ${creator:-?}: $desc)"

how="Review exactly this head with an independent reviewer, then post the verdict: hack/review-verdict.sh $PR ready <reviewer> $SHA ['<note>'] (it re-runs this job). A push makes an earlier verdict stale."
if [[ -z $state ]]; then
  fail "pull request #$PR touches sensitive paths and its head ${SHA:0:12} has no review-verdict status" "$how"
fi
fail "pull request #$PR touches sensitive paths and review-verdict on ${SHA:0:12} is $state (posted by ${creator:-?}: $desc)" "$how"
