#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The branch updater (updater.yml, old issue 596): bring the oldest ready, green auto-merge pull request
# that is behind main up to date, and label a conflicting one needs-update. Moved out of hack/claim.sh
# by ADR 0104 (it has nothing to do with claims). Retired with updater.yml once the merge queue is live
# (#227, ADR 0094 decision 4).
#
# Usage:
#   pr-updater.sh update-oldest
#
# Exit codes: 0 done (also: nothing to do), 1 error.
#
# Environment (all optional):
#   KOINON_CLAIM_REPO      owner/name (default: the current repository; the name is kept from claim.sh
#                          so updater.yml did not change its environment)
#   KOINON_BASE_BRANCH     the base branch whose ruleset is read for a merge queue (default main)
#   KOINON_CLAIM_REQUIRED  the required-checks file (default hack/ci-required.txt)
#   KOINON_CLAIM_NOW_EPOCH the current time as an epoch (tests)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

die() { echo "pr-updater: $*" >&2; exit 1; }
warn() { echo "pr-updater: warning: $*" >&2; }

REPO=${KOINON_CLAIM_REPO:-}
OWNER=
NAME=
init_repo() {
  [[ -n $REPO ]] || REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
  OWNER=${REPO%%/*}
  NAME=${REPO#*/}
}

TRY_OUT=
TRY_RC=0
gh_try() {
  TRY_RC=0
  TRY_OUT=$(gh api "$@" 2>&1) || TRY_RC=$?
}

if date -u -d @0 +%s >/dev/null 2>&1; then
  http_date_to_epoch() { LC_ALL=C date -u -d "$1" +%s; }
else
  http_date_to_epoch() { LC_ALL=C date -u -j -f '%a, %d %b %Y %H:%M:%S GMT' "$1" +%s; }
fi

server_now() {
  if [[ -n ${KOINON_CLAIM_NOW_EPOCH:-} ]]; then echo "$KOINON_CLAIM_NOW_EPOCH"; return; fi
  local d
  d=$(gh api -i /rate_limit 2>/dev/null | tr -d '\r' | sed -n 's/^[Dd]ate: *//p' | head -1)
  [[ -n $d ]] || die "cannot read the server time (Date header)"
  http_date_to_epoch "$d"
}

# The updater runs on a GitHub App token without issues: write; its label only informs the author, so a
# failed label write warns and never stops the update-branch step (koinon#315).
add_label_soft() { gh api -X POST "repos/$REPO/issues/$1/labels" -f "labels[]=$2" >/dev/null 2>&1 || warn "cannot label #$1 $2"; }
drop_label() { gh api -X DELETE "repos/$REPO/issues/$1/labels/$2" >/dev/null 2>&1 || true; }

# open_prs prints the open pull requests in the shape of `gh pr list --json` (number, createdAt,
# isDraft, headRefOid, labels, autoMergeRequest, mergeStateStatus, statusCheckRollup). It asks
# GraphQL for the check fields it uses only: `gh pr list --json statusCheckRollup` also reads each
# check's workflow run, which an App token without Actions read is refused.
open_prs() {
  # shellcheck disable=SC2016
  gh api graphql -f o="$OWNER" -f n="$NAME" -f query='
query($o: String!, $n: String!) {
  repository(owner: $o, name: $n) {
    pullRequests(states: OPEN, first: 100, orderBy: {field: CREATED_AT, direction: ASC}) {
      nodes {
        number createdAt isDraft headRefOid mergeStateStatus isInMergeQueue
        autoMergeRequest { enabledAt }
        labels(first: 50) { nodes { name } }
        commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100) { nodes {
          __typename
          ... on CheckRun { name status conclusion startedAt }
          ... on StatusContext { context state createdAt }
        } } } } } }
      }
    }
  }
}' --jq '[.data.repository.pullRequests.nodes[] | {number, createdAt, isDraft, headRefOid, mergeStateStatus, isInMergeQueue,
    autoMergeRequest, labels: .labels.nodes,
    statusCheckRollup: (.commits.nodes[0].commit.statusCheckRollup.contexts.nodes // [])}]'
}

# required_contexts [FILE]: the required check names of hack/ci-required.txt as a JSON array (the
# repository's source of truth; `advisory` lines are not required). The updater runs on a checkout
# of the default branch, so the file is there.
required_contexts() {
  local f=${1:-${KOINON_CLAIM_REQUIRED:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/hack/ci-required.txt}}
  [[ -r $f ]] || die "cannot read $f"
  sed -e '/^[[:space:]]*#/d' -e '/^[[:space:]]*$/d' -e '/^advisory /d' -e 's/^aggregator //' -e 's/^external //' "$f" | jq -R . | jq -s .
}

# JQ_LATEST: jq definitions shared by the updater's picks. A rollup keeps every run of a check name
# on the head (a draft run, a cancelled run, a rerun), so only the newest run per name counts, and
# only the required names: an advisory check, a third-party check or the updater's own run says
# nothing about whether the pull request can merge. A queued run without a start time is the newest.
# shellcheck disable=SC2016
JQ_LATEST='
def nm: .name // .context;
def ts: if (.__typename == "CheckRun") and (.startedAt == null) and (.status != "COMPLETED") then "9999" else (.startedAt // .createdAt // "") end;
def latest($req): [.statusCheckRollup[]? | select(nm | IN($req[]))] | group_by(nm) | map(max_by(ts));
def green: if .__typename == "StatusContext" then .state == "SUCCESS"
  else .status == "COMPLETED" and (.conclusion | IN("SUCCESS", "SKIPPED", "NEUTRAL")) end;
'

# update_target REQUIRED_JSON: stdin is open_prs output; prints the number of the pull request to
# bring up to date, or nothing. The oldest auto-merge pull request that is behind main, not a draft,
# and green on its own head: every required check has reported and its newest run succeeded.
# Updating any other one only starts CI runs that are thrown away (old issue 596): a draft is not asking to
# merge, a red or running head changes anyway, and a conflicting one (DIRTY) is labelled needs-update.
update_target() {
  jq -r --argjson req "${1:?required contexts}" "$JQ_LATEST"'
    [.[] | select(.autoMergeRequest != null and .mergeStateStatus == "BEHIND" and (.isDraft | not) and ((.isInMergeQueue // false) | not)
      and ($req | length > 0)
      and (latest($req) as $l | all($req[]; . as $r | any($l[]; nm == $r)) and all($l[]; green)))]
    | sort_by(.createdAt) | first.number // empty'
}

cmd_update_oldest() {
  local prs inflight target head tries=0 now req
  # With a merge queue on main (#225, ADR 0094 decision 4) the queue builds main plus the entry, so
  # nothing here is needed, and a push to a queued PR's branch would drop it from the queue. Read at run time;
  # an unreadable answer means "no queue" (the pre-queue behaviour). Exit condition for removing this command
  # and updater.yml: #227 (also linked from docs/dev/ci.md "The merge queue").
  if [[ $(gh api "repos/$REPO/rules/branches/${KOINON_BASE_BRANCH:-main}" --jq 'any(.[]; .type == "merge_queue")' 2>/dev/null || true) == true ]]; then
    echo "::notice::main has a merge queue; the updater has nothing to do"; return 0
  fi
  now=$(server_now)
  req=$(required_contexts)
  while :; do
    prs=$(open_prs)
    if ((tries < 3)) && jq -e '[.[] | select(.autoMergeRequest != null and .mergeStateStatus == "UNKNOWN")] | length > 0' <<<"$prs" >/dev/null; then
      tries=$((tries + 1)); sleep 5; continue
    fi
    break
  done
  # In flight: an up-to-date auto-merge PR whose required checks are still running, started within the last hour
  # (a stuck check does not block the others for ever).
  inflight=$(jq -r --argjson now "$now" --argjson req "$req" "$JQ_LATEST"'
    [.[] | select(.autoMergeRequest != null and .mergeStateStatus != "BEHIND" and ((.isInMergeQueue // false) | not)
    and ([latest($req)[] | select(((.status // "COMPLETED") != "COMPLETED" or .state == "PENDING")
         and (((.startedAt // "1970-01-01T00:00:00Z") | fromdateiso8601) > ($now - 3600)))] | length > 0)) | .number] | first // empty' <<<"$prs")
  if [[ -n $inflight ]]; then echo "::notice::#$inflight is up to date and its checks are running; not updating another branch"; return 0; fi
  # A conflicting auto-merge pull request is labelled for its author and skipped; the bot never
  # pushes code to a pull request branch. The label goes when the conflict does.
  local n
  for n in $(jq -r '.[] | select(.autoMergeRequest != null and .mergeStateStatus == "DIRTY" and ((.isInMergeQueue // false) | not)) | .number' <<<"$prs"); do
    if ! jq -e --argjson n "$n" '.[] | select(.number == $n) | .labels[]? | select(.name == "needs-update")' <<<"$prs" >/dev/null; then
      echo "#$n conflicts with main; labelling needs-update"
      add_label_soft "$n" needs-update
    fi
  done
  for n in $(jq -r '.[] | select(.mergeStateStatus != "DIRTY" and ([.labels[]? | select(.name == "needs-update")] | length > 0)) | .number' <<<"$prs"); do
    drop_label "$n" needs-update
  done
  target=$(update_target "$req" <<<"$prs")
  if [[ -z $target ]]; then echo "no ready, green auto-merge pull request is behind main"; return 0; fi
  head=$(jq -r --argjson n "$target" '.[] | select(.number == $n) | .headRefOid' <<<"$prs")
  gh_try -X PUT "repos/$REPO/pulls/$target/update-branch" -f "expected_head_sha=$head"
  if ((TRY_RC == 0)); then echo "updated #$target with main"
  elif [[ $TRY_OUT == *"onflict"* ]]; then echo "#$target conflicts with main; labelling needs-update"; add_label_soft "$target" needs-update
  else die "update-branch failed for #$target: $TRY_OUT"; fi
}

main() {
  command -v gh >/dev/null || die "gh is required"
  command -v jq >/dev/null || die "jq is required"
  case ${1:-} in
    update-oldest) init_repo; cmd_update_oldest ;;
    -h | --help | help) sed -n '4,/^# Needs bash 5/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' ;;
    *) echo "usage: pr-updater.sh update-oldest" >&2; exit 1 ;;
  esac
}

if [[ -z ${PR_UPDATER_LIB_ONLY:-} ]]; then main "$@"; fi
