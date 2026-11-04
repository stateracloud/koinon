#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Merge a pull request only after its head has been read, not assumed (#647). It replaces the
# hand-written wait-then-merge loops: a draft's green rollup, a ready run that has not started yet and a
# required context that never reported have each been merged over by hand before.
#
# Usage: hack/pr-merge.sh <PR> [--admin] [--approved-at <time>]
#   1. refuses a draft (exit 5) and a PR that is not open
#   2. hack/pr-wait.sh <PR> --checks, plus `--require e2e` when hack/needs-e2e.sh says the change needs
#      the kind e2e (the ruleset's required contexts are read by --checks itself)
#   3. reads every context of hack/ci-required.txt (not `advisory`) on the head SHA, newest run per
#      name; each must be success or skipped, otherwise it exits 1 without merging
#   4. only then: hack/claim.sh review <N> for each issue the PR closes (a failure is a warning: the
#      merge does not depend on the claim), `gh pr merge --merge` (with --admin when given, else --auto),
#      pinned to the head SHA that was read, and a PR comment naming that SHA and the number of green
#      contexts read
#   --admin        merge now with admin rights (main merges ready PRs this way, AGENTS.md "Merging")
#   --approved-at  when the human approved the admin merge; recorded in the comment, not checked
# Exit: 0 merged (or auto-merge enabled), 1 a required context is not green, 2 the wait timed out or
#       a tool failed, 3 closed, 4 conflict, 5 draft, 64 usage (pr-wait.sh's code passes through).
# Test hooks: PR_MERGE_WAIT, PR_MERGE_NEEDS_E2E, PR_MERGE_CLAIM replace the three helper scripts.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat

here=$(cd "$(dirname "$0")" && pwd)
wait_sh=${PR_MERGE_WAIT:-$here/pr-wait.sh}
needs_sh=${PR_MERGE_NEEDS_E2E:-$here/needs-e2e.sh}
claim_sh=${PR_MERGE_CLAIM:-$here/claim.sh}
required_txt=${PR_MERGE_REQUIRED:-$here/ci-required.txt}

usage() {
  echo "usage: hack/pr-merge.sh <PR> [--admin] [--approved-at <time>]" >&2
  exit 64
}

pr="" admin=0 approved=""
while [ $# -gt 0 ]; do
  case $1 in
  --admin) admin=1 ;;
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

json=$(gh pr view "$pr" --json state,isDraft,headRefOid,baseRefName,closingIssuesReferences) || { echo "pr-merge: cannot read PR $pr" >&2; exit 2; }
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
gh pr view "$pr" --json files --jq '.files[].path' | "$needs_sh" --stdin >/dev/null 2>&1 || rc=$?
case $rc in
1) ;;                  # not needed
*) req=(--require e2e) # needed, or it could not be decided (exit 2): treat as needed
esac

wait_rc=0
"$wait_sh" "$pr" --checks ${req[@]+"${req[@]}"} || wait_rc=$?
[ "$wait_rc" -eq 0 ] || { echo "pr-merge: pr-wait.sh exited $wait_rc; not merging" >&2; exit "$wait_rc"; }

# Every required context of hack/ci-required.txt, read on the head SHA: the newest run of each name.
contexts=$(sed -e 's/#.*//' -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' "$required_txt" |
  awk 'NF && $1 != "advisory" { sub(/^(aggregator|external) /, ""); print }')
[ -n "$contexts" ] || { echo "pr-merge: no required contexts in $required_txt" >&2; exit 2; }
runs=$(gh api "repos/{owner}/{repo}/commits/$sha/check-runs?per_page=100" --paginate \
  --jq '.check_runs[] | {name: .name, ts: (.started_at // ""), v: (if .status == "completed" then (.conclusion // "none") else "pending" end)}' | jq -sc .) ||
  { echo "pr-merge: cannot read the check runs of $sha" >&2; exit 2; }
stats=$(gh api "repos/{owner}/{repo}/commits/$sha/status" \
  --jq '.statuses[] | {name: .context, ts: (.created_at // ""), v: .state}' | jq -sc .) ||
  { echo "pr-merge: cannot read the commit statuses of $sha" >&2; exit 2; }
verdicts=$(jq -nr --argjson a "${runs:-[]}" --argjson b "${stats:-[]}" --arg want "$contexts" '
  ($a + $b) as $all
  | ($want | split("\n") | map(select(. != "")))[]
  | . as $n
  | ([$all[] | select(.name == $n)] | sort_by(.ts) | last // {v: "missing"}) as $r
  | [$n, $r.v] | @tsv')
green=0 bad=0
while IFS=$'\t' read -r name v; do
  case $v in
  success | skipped) green=$((green + 1)) ;;
  *)
    echo "NOT GREEN $name ($v) on ${sha:0:7}" >&2
    bad=$((bad + 1))
    ;;
  esac
done <<<"$verdicts"
if [ "$bad" -gt 0 ]; then
  echo "pr-merge: $bad required context(s) not green on ${sha:0:7}; not merging" >&2
  exit 1
fi

for n in $(jq -r '.closingIssuesReferences[]?.number' <<<"$json"); do
  "$claim_sh" review "$n" || echo "pr-merge: warning: claim.sh review $n failed ($?); merging anyway" >&2
done

mode=--auto
[ "$admin" = 0 ] || mode=--admin
gh pr merge "$pr" --merge "$mode" --match-head-commit "$sha"

note="pr-merge.sh: read $green required contexts on head \`$sha\` (newest run per name; all success or skipped) against $base, then merged ($mode)."
[ -z "$approved" ] || note+=" Approved at $approved."
gh pr comment "$pr" --body "$note"
echo "pr-merge: $mode merge of PR $pr at ${sha:0:7} ($green contexts green)"
