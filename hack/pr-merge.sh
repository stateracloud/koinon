#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Merge a pull request only after its head has been read, not assumed (old issue 647). It replaces the
# hand-written wait-then-merge loops: a draft's green rollup, a ready run that has not started yet and a
# required context that never reported have each been merged over by hand before.
#
# Usage: hack/pr-merge.sh <PR> [--jump | --break-glass "<reason>" | --admin] [--e2e] [--approved-at <time>]
#   1. refuses a draft (exit 5) and a PR that is not open
#   2. hack/pr-wait.sh <PR> --checks (the ruleset's required contexts are read by --checks itself). The e2e is not
#      checked on the head: the merge queue's quickstart is the one e2e proof (ADR 0105).
#   3. reads every context of hack/ci-required.txt (not `advisory`) on the head SHA and judges it with
#      hack/lib/pr-verdict.sh, the function pr-wait.sh uses too (#133): per workflow run, the newest counting run
#      of each workflow decides; each context must be success or skipped, otherwise it exits 1 without merging
#   4. only then: re-reads the PR (no longer OPEN: exit 3; head moved: exit 2; head not origin's
#      branch tip: exit 6 STALE-HEAD), and merges pinned to the head SHA that was read, then a PR comment naming
#      that SHA and the number of green contexts read.
#   The route (ADR 0105):
#   (default)      `gh pr merge --merge --auto`: with the merge queue rule it adds the PR to the queue, which builds main
#                  plus the PR, runs every required check and the kind e2e when the entry needs it, and merges that
#                  commit; the output says `added to the merge queue`. Without the rule: auto-merge.
#   --jump         urgent: GraphQL enqueuePullRequest(jump: true, expectedHeadOid), the front of the queue. The change
#                  is still tested on main's tip. For a red main or a security fix, not for "slow". Needs the queue
#                  rule (exit 2 without it).
#   --e2e          force the queue e2e: adds the `e2e` label before enqueueing. The label is read when the queue builds
#                  the entry; on a PR that is already queued it is not (the output says to dequeue and enqueue again).
#   --break-glass "<reason>"
#                  emergency, for a queue that cannot merge anything (GitHub's queue or the hosted runners down). Needs
#                  --approved-at (the owner approves; exit 64 without). Runs the checks above,
#                  merges with `gh pr merge --admin --merge`, and opens a `break-glass` issue (sha, reason, approver).
#   --admin        merge now with admin rights, for a pull request that changes at least one path under `.github/`: a
#                  workflow file runs from the pull request, so those keep the
#                  CODEOWNERS approval, which one human cannot give (ADR 0105). Any other pull request is
#                  refused with exit 9 and told to use the queue or --break-glass. It skips the queue and so the queue
#                  e2e, so it is fail-closed for it: when the base's needs-e2e.sh says the change needs the e2e (or cannot
#                  say), the head needs a green `quickstart` of its own, else exit 10. Likewise for `flavors` (exit 10): when the base's hack/ci-changes.sh (ignoring the CI-plumbing
#                  files, CI_CHANGES_IGNORE_PLUMBING=1) says real flavor inputs changed, the head needs a real, not skipped, flavors success; split such a pull request. Exit 2 when the file list
#                  cannot be read.
#   Unresolved review threads (#187): pr-wait.sh --checks exits 7 when every check is green but the PR is
#   BLOCKED by them (it lists them, path:line and author), and this script passes the 7 through; it also
#   looks again right before merging (mergeStateStatus BLOCKED). It never replies to or resolves a thread.
#   --approved-at  when the human approved the merge; recorded in the comment (required by --break-glass)
# Exit: 0 merged (or auto-merge enabled, or queued), 1 a required context is not green, 2 the wait timed out or
#       a tool failed (also: a file list that cannot
#       be read, --jump without a queue, the base fetch or a rule file for --admin), 3 closed, 4 conflict, 5 draft, 6 stale head, 7 unresolved review threads,
#       9 --admin on a PR without a `.github/` path (ADR 0105), 10 --admin on an
#       e2e-relevant PR without a green quickstart on the head, 64 usage
#       (pr-wait.sh's code passes through).
#   The closing line says what GitHub did, not what was asked: `merged`, `added to the merge queue`, `auto-merge
#   armed but NOT queued: ... required review`, or `auto-merge enabled` (prv_merge_outcome).
# Test hooks: PR_MERGE_WAIT, PR_MERGE_THREADS replace the helper scripts; PR_MERGE_NEEDS_E2E replaces needs-e2e.sh
# (with PR_MERGE_TEST=1).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat

here=$(cd "$(dirname "$0")" && pwd)
wait_sh=${PR_MERGE_WAIT:-$here/pr-wait.sh}
threads_sh=${PR_MERGE_THREADS:-$here/pr-threads.sh}
required_txt=${PR_MERGE_REQUIRED:-$here/ci-required.txt}
# shellcheck source=/dev/null
. "$here/lib/pr-verdict.sh"

usage() {
  echo "usage: hack/pr-merge.sh <PR> [--jump | --break-glass \"<reason>\" | --admin] [--e2e] [--approved-at <time>]" >&2
  exit 64
}

pr="" admin=0 approved="" jump=0 glass=0 reason="" e2e=0
while [ $# -gt 0 ]; do
  case $1 in
  --admin) admin=1 ;;
  --jump) jump=1 ;;
  --e2e) e2e=1 ;;
  --break-glass)
    [ $# -ge 2 ] || usage
    glass=1 reason=$2
    shift
    ;;
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
[ $((admin + jump + glass)) -le 1 ] || { echo "pr-merge: --jump, --break-glass and --admin are exclusive" >&2; exit 64; }
if [ "$glass" = 1 ]; then
  [ -n "${reason//[[:space:]]/}" ] || { echo "pr-merge: --break-glass needs a reason" >&2; exit 64; }
  [ -n "$approved" ] || { echo "pr-merge: --break-glass needs --approved-at <time>: only the owner approves it" >&2; exit 64; }
fi
[ "$e2e" = 0 ] || [ $((admin + glass)) -eq 0 ] || { echo "pr-merge: --e2e forces the queue e2e; --admin and --break-glass skip the queue" >&2; exit 64; }

pr_json=$(gh pr view "$pr" --json id,state,isDraft,headRefOid,baseRefName,closingIssuesReferences) || { echo "pr-merge: cannot read PR $pr" >&2; exit 2; }
json=$pr_json
state=$(jq -r '.state' <<<"$json")
sha=$(jq -r '.headRefOid' <<<"$json")
node=$(jq -r '.id // ""' <<<"$json")
base=$(jq -r '.baseRefName' <<<"$json")
[ "$state" = OPEN ] || { echo "pr-merge: PR $pr is $state, not open" >&2; exit 3; }
if [ "$(jq -r '.isDraft' <<<"$json")" = true ]; then
  echo "DRAFT: PR $pr is a draft; mark it ready for review first (hack/ship.sh --ready, gh pr ready)" >&2
  exit 5
fi

# --admin is for `.github/**` pull requests only (ADR 0105): judged on the PR's own file list, not on the local
# checkout. `gh pr view --json files` stops at 100 files, so page through the API. Fail closed: an unreadable or
# empty list is a tool failure (exit 2), never a license to bypass the queue.
if [ "$admin" = 1 ]; then
  files=$(gh api "repos/{owner}/{repo}/pulls/$pr/files" --paginate --jq '.[].filename' 2>/dev/null) || files=""
  [ -n "$files" ] || { echo "pr-merge: cannot read the file list of PR $pr; not merging with --admin" >&2; exit 2; }
  if ! grep -q '^\.github/' <<<"$files"; then
    echo "pr-merge: --admin is for pull requests that change a path under .github/ (ADR 0105); PR $pr changes none. Use hack/pr-merge.sh $pr (the merge queue), --jump for an urgent one, or --break-glass \"<reason>\" --approved-at <time> when the queue cannot merge anything" >&2
    exit 9
  fi
fi

wait_rc=0 wait_warn=""
"$wait_sh" "$pr" --checks || wait_rc=$?
if [ "$wait_rc" -ne 0 ]; then
  if [ "$glass" = 1 ] && [ "$wait_rc" -ne 3 ] && [ "$wait_rc" -ne 5 ]; then
    # Break glass exists for a queue or runners that are down, when no check can pass: the wait is a warning that
    # goes into the record, not a stop (ADR 0105). A closed PR (3) and a draft (5) still stop.
    wait_warn="pr-wait.sh --checks exited $wait_rc on ${sha:0:7} (the required checks were not all green when this merged)"
    echo "pr-merge: WARNING: $wait_warn; continuing: --break-glass" >&2
  else
    echo "pr-merge: pr-wait.sh exited $wait_rc; not merging" >&2
    exit "$wait_rc"
  fi
fi

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
verdicts=$(prv_judge "$rows" "$wfruns" false "${ctx_list[@]}")
green=0 bad=0 notgreen=""
while IFS=$'\t' read -r name v _run; do
  case $v in
  success | skipped) green=$((green + 1)) ;;
  *)
    echo "NOT GREEN $name ($v${_run:+, run $_run}) on ${sha:0:7}" >&2
    bad=$((bad + 1))
    notgreen+="${notgreen:+, }$name ($v)"
    ;;
  esac
done <<<"$verdicts"
if [ "$bad" -gt 0 ]; then
  if [ "$glass" = 1 ]; then
    # With the runners down no context turns green: a warning in the record, not a stop (ADR 0105).
    wait_warn+="${wait_warn:+; }$bad required context(s) not green on ${sha:0:7}: $notgreen"
    echo "pr-merge: WARNING: $bad required context(s) not green on ${sha:0:7}; continuing: --break-glass" >&2
  else
    echo "pr-merge: $bad required context(s) not green on ${sha:0:7}; not merging" >&2
    exit 1
  fi
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
if [ "$jump" = 1 ] && [ "$queue" != 1 ]; then
  echo "pr-merge: --jump needs the merge queue rule on $base, and the ruleset shows none; use hack/pr-merge.sh $pr" >&2
  exit 2
fi
if [ $((admin + glass)) != 0 ]; then
  mode=--admin
  if [ "$glass" = 1 ]; then
    echo "pr-merge: BREAK GLASS: merging PR $pr without the queue (approved at $approved): $reason" >&2
  else
    # --admin skips the queue's quickstart, so it stays fail-closed for the e2e: when the BASE's hack/needs-e2e.sh says
    # this change needs it (or cannot say), the head needs a `quickstart` success of its own (exit 10).
    needs_sh=""
    [ "${PR_MERGE_TEST:-}" != 1 ] || needs_sh=${PR_MERGE_NEEDS_E2E:-}
    if [ -z "$needs_sh" ]; then
      # The base's rule, not this checkout's (it can be stale or the PR's own worktree).
      git fetch -q --no-tags origin "$base" 2>/dev/null || { echo "pr-merge: git fetch origin $base failed; cannot decide the e2e; not merging" >&2; exit 2; }
      ne_dir=$(mktemp -d "${TMPDIR:-/tmp}/pr-merge-ne.XXXXXX") || { echo "pr-merge: mktemp failed; not merging" >&2; exit 2; }
      trap 'rm -rf "$ne_dir"' EXIT
      mkdir -p "$ne_dir/hack"
      for f in hack/needs-e2e.sh hack/paths-lib.sh hack/paths.rules; do
        git show "refs/remotes/origin/$base:$f" >"$ne_dir/$f" 2>/dev/null || { echo "pr-merge: cannot read $f from origin/$base to decide the e2e; not merging" >&2; exit 2; }
      done
      chmod +x "$ne_dir/hack/needs-e2e.sh"
      needs_sh=$ne_dir/hack/needs-e2e.sh
    fi
    nrc=0
    "$needs_sh" --stdin <<<"$files" >/dev/null 2>&1 || nrc=$?
    if [ "$nrc" != 1 ]; then
      # 0 = needed; anything else = the rule could not be applied: needed.
      qv=$(prv_judge "$rows" "$wfruns" false quickstart | awk -F'\t' 'NR == 1 { print $2 }')
      if [ "$qv" != success ]; then
        echo "pr-merge: --admin: this change needs the kind e2e (hack/needs-e2e.sh on origin/$base, exit $nrc) and the head ${sha:0:7} has no green quickstart (${qv:-missing}); add the e2e label (hack/ship.sh --e2e or gh pr edit $pr --add-label e2e) to get a quickstart on the head, or use the queue; not merging" >&2
        exit 10
      fi
    fi
    # The same for `flavors`, with one difference: it is required, but a pull request head only has its green skip
    # (the job runs on the queue commit and on pushes to main, and ci.yml has no workflow_dispatch trigger to run it
    # for a branch). A `.github/**` pull request cannot use the queue (code-owner approval, ADR 0105 decision 6), so
    # the guard must not hold a change of the CI plumbing itself (ci.yml and the scripts that decide what it runs):
    # that would deadlock every ci.yml change. It asks the BASE's hack/ci-changes.sh with
    # CI_CHANGES_IGNORE_PLUMBING=1, which drops the `everything ci` rule and decides on the real flavor inputs only
    # (the `job flavors` rule, the suite's build graph, Makefile, go.mod, go.sum). When those changed, the head needs a
    # real (not skipped) success, and there is none to get: the change must be split (exit 10).
    # The build graph (`go list`) comes from the local checkout, not from the base or the head: run this script from a
    # checkout that is in sync with the pull request head. A variable exported in the caller's shell must not change
    # the answer, so the base copy runs with the rule-file and directory overrides unset.
    ci_sh=${here}/ci-changes.sh
    ci_root=""
    if [ "${PR_MERGE_TEST:-}" = 1 ] && [ -n "${PR_MERGE_CI_CHANGES:-}" ]; then
      ci_sh=$PR_MERGE_CI_CHANGES
    else
      ci_dir=$(mktemp -d "${TMPDIR:-/tmp}/pr-merge-ci.XXXXXX") || { echo "pr-merge: mktemp failed; not merging" >&2; exit 2; }
      trap 'rm -rf "${ne_dir:-}" "$ci_dir"' EXIT
      mkdir -p "$ci_dir/hack"
      for f in hack/ci-changes.sh hack/paths-lib.sh hack/paths.rules; do
        git show "refs/remotes/origin/$base:$f" >"$ci_dir/$f" 2>/dev/null || { echo "pr-merge: cannot read $f from origin/$base to decide the flavors; not merging" >&2; exit 2; }
      done
      chmod +x "$ci_dir/hack/ci-changes.sh"
      ci_sh=$ci_dir/hack/ci-changes.sh
      ci_root=$(cd "$here/.." && pwd)
    fi
    fl=$(env -u HACK_PATHS_RULES -u CI_CHANGES_FLAVORS_DIRS CI_CHANGES_IGNORE_PLUMBING=1 CI_CHANGES_ROOT="$ci_root" "$ci_sh" --stdin <<<"$files" 2>/dev/null | sed -n 's/^flavors=//p') || fl=""
    if [ "$fl" != false ]; then
      # true, or the rule could not be applied: needed.
      fv=$(prv_judge "$rows" "$wfruns" false flavors | awk -F'\t' 'NR == 1 { print $2 }')
      if [ "$fv" != success ]; then
        echo "pr-merge: --admin: this pull request changes flavor inputs and .github paths together (hack/ci-changes.sh on origin/$base says flavors=${fl:-unknown}) and the head ${sha:0:7} has no successful flavors run (${fv:-missing}); flavors runs only on the queue commit and on main, so split it: the flavor change goes through the merge queue, the .github change through --admin; not merging" >&2
        exit 10
      fi
    fi
    echo "pr-merge: --admin skips the merge queue and so the queue e2e (.github/** pull request, ADR 0105); the push e2e on main is the backstop" >&2
  fi
fi

# --e2e: the label is read when the queue builds the entry (ADR 0105 decision 3). On a PR that is already queued it
# does not rebuild the entry.
if [ "$e2e" = 1 ]; then
  e2e_qs=$(prv_queue_state "$pr")
  if [ "$e2e_qs" = no ] || [ "$e2e_qs" = unknown ]; then
    gh pr edit "$pr" --add-label e2e >/dev/null || { echo "pr-merge: cannot add the e2e label to PR $pr; not merging" >&2; exit 2; }
  else
    echo "pr-merge: PR $pr is already in the merge queue: the e2e label is read when the queue builds the entry, so it would not apply. Remove the PR from the queue (PR page), then run this again with --e2e" >&2
    exit 2
  fi
fi

if [ "$jump" = 1 ]; then
  # shellcheck disable=SC2016 # $id, $oid are GraphQL variables
  jump_query='mutation($id: ID!, $oid: GitObjectID!) { enqueuePullRequest(input: {pullRequestId: $id, jump: true, expectedHeadOid: $oid}) { mergeQueueEntry { position } } }'
  [ -n "$node" ] || { echo "pr-merge: cannot read the node id of PR $pr; not merging" >&2; exit 2; }
  gh api graphql -f query="$jump_query" -f id="$node" -f oid="$sha" >/dev/null || { rc=$?; echo "pr-merge: enqueuePullRequest(jump: true) failed ($rc)" >&2; exit "$rc"; }
else
  gh pr merge "$pr" --merge "$mode" --match-head-commit "$sha" || { rc=$?; echo "pr-merge: gh pr merge failed ($rc)" >&2; exit "$rc"; }
fi

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
[ "$jump" = 0 ] || note+=" Jumped to the front of the queue."
[ "$glass" = 0 ] || note+=" BREAK GLASS: $reason."
[ -z "$wait_warn" ] || note+=" Checks not green when merged: $wait_warn."
[ -z "$approved" ] || note+=" Approved at $approved."
if [ "$glass" = 1 ]; then
  # The record of a break-glass merge (ADR 0105): an issue the retro and the overnight report read. The `break-glass`
  # label is created by the owner; without it the issue is still opened, unlabelled.
  gtitle="break-glass: PR $pr merged without the merge queue (${sha:0:7})"
  gbody="PR #$pr was merged with \`hack/pr-merge.sh --break-glass\` at head \`$sha\`, skipping the merge queue and its required checks on the queue commit.

- Reason: $reason
- Approved at: $approved
- Checks not green at merge: ${wait_warn:-none (every required context was green on the head)}
- Skipped: the queue build (every required check on main plus the PR, and the kind e2e)

Follow-up: find why the queue could not merge, and run the e2e on main (\`gh workflow run e2e-kind.yml --ref main\`)."
  gh issue create --title "$gtitle" --body "$gbody" --label break-glass >/dev/null 2>&1 ||
    gh issue create --title "$gtitle" --body "$gbody" >/dev/null 2>&1 ||
    echo "pr-merge: WARNING: could not open the break-glass issue; open one by hand (title: $gtitle)" >&2
fi
# The comment comes after the record: a failed comment is a warning, never a reason to skip the issue (set -e).
gh pr comment "$pr" --body "$note" || echo "pr-merge: WARNING: could not post the PR comment" >&2
echo "pr-merge: PR $pr at ${sha:0:7}: $verb ($green contexts green)"
