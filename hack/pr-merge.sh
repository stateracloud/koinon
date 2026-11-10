#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Merge a pull request only after its head has been read, not assumed (old issue 647). It replaces the
# hand-written wait-then-merge loops: a draft's green rollup, a ready run that has not started yet and a
# required context that never reported have each been merged over by hand before.
#
# Usage: hack/pr-merge.sh <PR> [--admin] [--approved-at <time>]
#   1. refuses a draft (exit 5) and a PR that is not open
#   2. hack/pr-wait.sh <PR> --checks, plus `--require e2e` when hack/needs-e2e.sh says the change needs
#      the kind e2e (the ruleset's required contexts are read by --checks itself)
#   3. reads every context of hack/ci-required.txt (not `advisory`) on the head SHA and judges it with
#      hack/lib/pr-verdict.sh, the function pr-wait.sh uses too (#133): per workflow run, the newest counting run
#      of each workflow decides; each context must be success or skipped, otherwise it exits 1 without merging
#   4. only then: re-reads the PR (no longer OPEN: exit 3; head moved: exit 2; head not origin's
#      branch tip: exit 6 STALE-HEAD), `gh pr merge --merge` (with --admin when given, else --auto),
#      pinned to the head SHA that was read, then a PR comment naming that SHA and the number of green contexts read. With --auto the PR is merged later:
#      the output says `auto-merge enabled`.
#   Merge queue (#225): when the base branch's ruleset has a `merge_queue` rule (read at run time), the
#   same `gh pr merge --merge --auto` adds the PR to the queue and the output says `added to the merge queue`;
#   --admin still merges directly (bypass, and no queue-commit quickstart: #444, ADR 0102, an e2e-relevant PR should be enqueued). Without the rule nothing changes. docs/dev/ci.md "Merging".
#   Unresolved review threads (#187): pr-wait.sh --checks exits 7 when every check is green but the PR is
#   BLOCKED by them (it lists them, path:line and author), and this script passes the 7 through; it also
#   looks again right before merging (mergeStateStatus BLOCKED). It never replies to or resolves a thread.
#   The e2e decision reads the PR's whole file list (`gh api pulls/<PR>/files --paginate`); when that read
#   fails or is empty, e2e is required (fail closed).
#   --admin        merge now with admin rights: the exception (a CODEOWNERS or sensitive path, AGENTS.md "Merging"), not the
#                  default. With a merge queue and a change that needs the e2e it warns: the queue-commit quickstart is skipped.
#                  It runs hack/review-gate.sh on the head first (the `review` check it bypasses, ADR 0103) and
#                  refuses with exit 9 when a sensitive-path PR has no success review-verdict from a listed owner.
#                  The gate, claim.sh and CODEOWNERS are the base branch's (origin/<base>, fetched), not this
#                  checkout's; only when the base has no gate yet does it use the local copy, and says so.
#                  That fallback also runs for a PR whose base branch has no gate (a base other than main), and
#                  the local copy can then be the PR's own; harmless while the ruleset protects only main.
#   --approved-at  when the human approved the admin merge; recorded in the comment, not checked
# Exit: 0 merged (or auto-merge enabled, or queued), 1 a required context is not green, 2 the wait timed out or
#       a tool failed (also: the repository name, the base fetch or the gate copy for --admin), 3 closed,
#       4 conflict, 5 draft, 6 stale head, 7 unresolved review threads, 9 --admin without a review verdict
#       (ADR 0103), 64 usage
#       (pr-wait.sh's code passes through).
#   The closing line says what GitHub did, not what was asked: `merged`, `added to the merge queue`, `auto-merge
#   armed but NOT queued: ... required review`, or `auto-merge enabled` (prv_merge_outcome).
# Test hooks: PR_MERGE_WAIT, PR_MERGE_NEEDS_E2E, PR_MERGE_THREADS replace the helper scripts.
# PR_MERGE_REVIEW_GATE replaces the review gate only when PR_MERGE_TEST=1 is set too (a test hook). Not a
# security boundary: whoever runs this script can set both variables or edit it. The boundary is the required
# `review` check in the `main` ruleset; `--admin` and the web UI bypass it (ADR 0103).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat

here=$(cd "$(dirname "$0")" && pwd)
wait_sh=${PR_MERGE_WAIT:-$here/pr-wait.sh}
needs_sh=${PR_MERGE_NEEDS_E2E:-$here/needs-e2e.sh}
threads_sh=${PR_MERGE_THREADS:-$here/pr-threads.sh}
required_txt=${PR_MERGE_REQUIRED:-$here/ci-required.txt}
review_gate=""
[ "${PR_MERGE_TEST:-}" != 1 ] || review_gate=${PR_MERGE_REVIEW_GATE:-}
# shellcheck source=/dev/null
. "$here/lib/pr-verdict.sh"

usage() {
  echo "usage: hack/pr-merge.sh <PR> [--admin] [--approved-at <time>]" >&2
  exit 64
}

pr="" admin=0 approved=""
while [ $# -gt 0 ]; do
  case $1 in
  --admin) admin=1 ;;
  --lane)
    echo "pr-merge: --lane is gone (ADR 0104): the merge releases the claim, pr-merge.sh no longer calls claim.sh" >&2
    exit 64
    ;;
  --approved-at)
    [ $# -ge 2 ] || usage
    approved=$2
    shift
    ;;
  -*) usage ;;
  *)
    [ -z "$pr" ] || usage
    pr=$1
    ;;
  esac
  shift
done
case $pr in '' | *[!0-9]*) usage ;; esac

pr_json=$(gh pr view "$pr" --json state,isDraft,headRefOid,baseRefName,closingIssuesReferences) || { echo "pr-merge: cannot read PR $pr" >&2; exit 2; }
json=$pr_json
state=$(jq -r '.state' <<<"$json")
sha=$(jq -r '.headRefOid' <<<"$json")
base=$(jq -r '.baseRefName' <<<"$json")
[ "$state" = OPEN ] || { echo "pr-merge: PR $pr is $state, not open" >&2; exit 3; }
if [ "$(jq -r '.isDraft' <<<"$json")" = true ]; then
  echo "DRAFT: PR $pr is a draft; mark it ready for review first (hack/ship.sh --ready, gh pr ready)" >&2
  exit 5
fi

# Does the change need the kind e2e? Judged on the PR's own file list, not on the local checkout.
req=()
rc=0
# Fail closed: an unreadable or empty list requires e2e. `gh pr view --json files` stops at 100 files.
if files=$(gh api "repos/{owner}/{repo}/pulls/$pr/files" --paginate --jq '.[].filename' 2>/dev/null) && [ -n "$files" ]; then
  "$needs_sh" --stdin <<<"$files" >/dev/null 2>&1 || rc=$?
else
  echo "pr-merge: cannot read the file list of PR $pr; requiring e2e" >&2
  rc=2
fi
case $rc in
1) ;;                  # not needed
*) req=(--require e2e) # needed, or it could not be decided (exit 2): treat as needed
esac

wait_rc=0
"$wait_sh" "$pr" --checks ${req[@]+"${req[@]}"} || wait_rc=$?
[ "$wait_rc" -eq 0 ] || { echo "pr-merge: pr-wait.sh exited $wait_rc; not merging" >&2; exit "$wait_rc"; }

# Every required context of hack/ci-required.txt, read on the head SHA and judged by hack/lib/pr-verdict.sh, the
# same function hack/pr-wait.sh uses (#133): per workflow run, the newest counting run of each workflow decides.
contexts=$(sed -e 's/#.*//' -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' "$required_txt" |
  awk 'NF && $1 != "advisory" { sub(/^(aggregator|external) /, ""); print }')
[ -n "$contexts" ] || { echo "pr-merge: no required contexts in $required_txt" >&2; exit 2; }
runs=$(prv_check_rows "$sha") || { echo "pr-merge: cannot read the check runs of $sha" >&2; exit 2; }
stats=$(prv_status_rows "$sha") || { echo "pr-merge: cannot read the commit statuses of $sha" >&2; exit 2; }
wfruns=$(prv_runs "$sha") # empty when unreadable: nothing is dropped, so the verdict only gets stricter
[ -n "$wfruns" ] || echo "pr-merge: cannot list the workflow runs of $sha; judging every check run on it" >&2
rows=$(jq -nc --argjson a "${runs:-[]}" --argjson b "${stats:-[]}" '$a + $b')
mapfile -t ctx_list <<<"$contexts"
verdicts=$(prv_judge "$rows" "$wfruns" false "quickstart" "${ctx_list[@]}")
green=0 bad=0
while IFS=$'\t' read -r name v _run; do
  case $v in
  success | skipped) green=$((green + 1)) ;;
  *)
    echo "NOT GREEN $name ($v${_run:+, run $_run}) on ${sha:0:7}" >&2
    bad=$((bad + 1))
    ;;
  esac
done <<<"$verdicts"
if [ "$bad" -gt 0 ]; then
  echo "pr-merge: $bad required context(s) not green on ${sha:0:7}; not merging" >&2
  exit 1
fi

# The wait can take an hour: read the PR again before merging (state, head, and origin's branch tip).
json=$(gh pr view "$pr" --json state,headRefOid,headRefName,isCrossRepository,mergeStateStatus) || { echo "pr-merge: cannot re-read PR $pr" >&2; exit 2; }
[ "$(jq -r '.state' <<<"$json")" = OPEN ] || { echo "pr-merge: PR $pr is no longer open ($(jq -r '.state' <<<"$json")); not merging" >&2; exit 3; }
now=$(jq -r '.headRefOid' <<<"$json")
[ "$now" = "$sha" ] || { echo "pr-merge: PR $pr head moved from ${sha:0:7} to ${now:0:7} while waiting; run it again" >&2; exit 2; }
br=$(jq -r '.headRefName // ""' <<<"$json")
if [ -n "$br" ] && [ "$(jq -r '.isCrossRepository // false' <<<"$json")" != true ]; then
  rem=$(git ls-remote origin "refs/heads/$br" 2>/dev/null | awk 'NR == 1 { print $1 }') || rem=""
  if [ -n "$rem" ] && [ "$rem" != "$sha" ]; then
    echo "STALE-HEAD: PR $pr head is ${sha:0:7} but origin/$br is ${rem:0:7}; GitHub has not picked up the push. Re-sync: gh api -X PATCH repos/{owner}/{repo}/pulls/$pr -f base=$base" >&2
    exit 6
  fi
fi

# BLOCKED with every required context green: unresolved review threads (the ruleset's
# required_review_thread_resolution, #187; the wait above already checks, but threads can arrive during it).
# Never resolved from here; the PR is not merged and --auto is not enabled on a PR that cannot merge.
# Read whatever mergeStateStatus says: GitHub often reports UNKNOWN right after a push, and --admin
# bypasses the rule, so a BLOCKED-only read could admin-merge over unresolved threads.
{
  trc=0
  threads=$("$threads_sh" "$pr" 2>&1) || trc=$?
  case $trc in
  0) ;;
  7)
    echo "$threads" >&2
    echo "pr-merge: PR $pr is BLOCKED by unresolved review threads; reply and resolve each one (docs/dev/ci.md \"Merging\"), then run it again; not merging" >&2
    exit 7
    ;;
  *)
    echo "pr-merge: PR $pr is BLOCKED and its review threads could not be read: $threads" >&2
    echo "pr-merge: cannot read review threads; not merging" >&2
    exit 2
    ;;
  esac
}

# Does the base branch have a merge queue (#225)? Read from the ruleset at run time, so the same script
# serves the repository before and after the ruleset gains the rule. Only an exact `true` counts: an
# unreadable answer is "no queue" (the merge below is the same command either way; only the report differs).
queue=0
if [ "$(gh api "repos/{owner}/{repo}/rules/branches/$base" --jq 'any(.[]; .type == "merge_queue")' 2>/dev/null || true)" = true ]; then
  queue=1
fi

# With a queue, `gh pr merge --auto` enqueues the PR (it already passed every check that gates entry, read
# above) and GitHub merges it after the queue build is green: no manual sync. --admin is GitHub's "merge
# without waiting for requirements" (a bypass actor merges directly, skipping the queue and the review rule).
mode=--auto
if [ "$admin" != 0 ]; then
  mode=--admin
  # --admin bypasses the ruleset, so the `review` check too (ADR 0103): run the gate on the head here. A
  # sensitive-path PR without a success review-verdict from a listed owner is refused (exit 9).
  repo=${GH_REPO:-$(gh repo view --json nameWithOwner --jq .nameWithOwner 2>/dev/null || true)}
  [ -n "$repo" ] || { echo "pr-merge: cannot read the repository name for the review gate; not merging" >&2; exit 2; }
  if [ -z "$review_gate" ]; then
    # The base's gate, claim.sh and CODEOWNERS: this checkout can be stale or the PR's own worktree.
    git fetch -q --no-tags origin "$base" 2>/dev/null || { echo "pr-merge: git fetch origin $base failed; cannot run the review gate; not merging" >&2; exit 2; }
    if git cat-file -e "refs/remotes/origin/$base:hack/review-gate.sh" 2>/dev/null; then
      gate_dir=$(mktemp -d "${TMPDIR:-/tmp}/pr-merge-gate.XXXXXX") || { echo "pr-merge: mktemp failed; not merging" >&2; exit 2; }
      trap 'rm -rf "$gate_dir"' EXIT
      mkdir -p "$gate_dir/hack" "$gate_dir/.github"
      for f in hack/review-gate.sh hack/claim.sh .github/CODEOWNERS; do
        git show "refs/remotes/origin/$base:$f" >"$gate_dir/$f" 2>/dev/null || { echo "pr-merge: cannot read $f from origin/$base; not merging" >&2; exit 2; }
      done
      chmod +x "$gate_dir/hack/review-gate.sh" "$gate_dir/hack/claim.sh"
      review_gate=$gate_dir/hack/review-gate.sh
    else
      echo "pr-merge: origin/$base has no hack/review-gate.sh yet (this PR adds it?): using the local copy" >&2
      review_gate=$here/review-gate.sh
    fi
  fi
  grc=0
  gate_out=$(env -u CLAIM -u CODEOWNERS -u KOINON_CLAIM_CODEOWNERS EVENT=pull_request PR="$pr" SHA="$sha" GH_REPO="$repo" "$review_gate" 2>&1) || grc=$?
  if [ "$grc" != 0 ]; then
    printf '%s\n' "$gate_out" >&2
    echo "pr-merge: --admin: the review gate (hack/review-gate.sh) fails on ${sha:0:7}; record the independent review with hack/review-verdict.sh $pr ready <reviewer> $sha, then run this again; not merging" >&2
    exit 9
  fi
  if [ "$queue" = 1 ] && [ "${#req[@]}" -gt 0 ]; then
    echo "pr-merge: WARNING: --admin skips the merge queue, so the quickstart on the queue commit (the merge evidence, #444, ADR 0102) does not run for this e2e-relevant change; enqueue it (no --admin) unless the path is CODEOWNERS-only" >&2
  fi
fi
gh pr merge "$pr" --merge "$mode" --match-head-commit "$sha" || { rc=$?; echo "pr-merge: gh pr merge failed ($rc)" >&2; exit "$rc"; }

# No claim step: the merge releases the claims of the issues the PR closes, through claims.yml's
# release job (ADR 0104). A close without a merge releases nothing: `claim.sh release-pr <PR>` or `release <N>`.

# What did `gh pr merge` achieve? It succeeds when GitHub accepted the request, not when the PR is queued or mergeable:
# a PR blocked by a required review (required_reviewers) has auto-merge armed and is not in the queue (2026-11-07
# printed "added to the merge queue" for one). Read the PR again and report what GitHub says (prv_merge_outcome).
qs=no
[ "$queue" = 0 ] || qs=$(prv_queue_state "$pr")
after=$(gh pr view "$pr" --json state,mergeStateStatus,reviewDecision,autoMergeRequest 2>/dev/null) || after="{}"
verb=$(prv_merge_outcome "$mode" "$qs" "$after")
note="pr-merge.sh: read $green required contexts on head \`$sha\` (newest counting run of each workflow; all success or skipped) against $base, then $verb."
[ -z "$approved" ] || note+=" Approved at $approved."
gh pr comment "$pr" --body "$note"
echo "pr-merge: PR $pr at ${sha:0:7}: $verb ($green contexts green)"
