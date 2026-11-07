#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The e2e-label workflow: the `e2e` label was added to a pull request. Label events do not start
# e2e-kind (old issue 468), so this starts it once on the PR's head, by dispatch, unless that is not needed:
#
#   - a run of e2e-kind on the head runs quickstart now, or one passed it: nothing to do.
#   - a run on the head has not decided yet (queued, or decide still running): wait for it. The
#     `opened` run of `gh pr create --label e2e` reads the labels live and normally runs the e2e
#     itself; only if it decided before the label landed does this dispatch.
#   - otherwise (no run, or every run skipped, failed or was cancelled): dispatch e2e-kind on the
#     branch. Removing and adding the label again is a way to retry a red e2e.
#
# A dispatch run's checks, `e2e result` among them, land on the head commit (run 41265167595 on
# old issue 462's head, 2026-11-01). Its `e2e result` appears only once quickstart ends; until then the head
# keeps its earlier result (green when that run skipped the e2e). hack/pr-wait.sh waits while an
# e2e-kind run on the head is live, so after the dispatch this waits until the new run is listed
# (it takes a few seconds) and fails loudly if it never is.
#
# Environment: HEAD_SHA, BRANCH (the PR's head branch), GH (the gh command), GH_REPO, POLL (seconds
# between looks, default 15), WAIT_MAX (seconds to wait for a run to decide, default 900; then it
# dispatches anyway: a second e2e beats none), LIST_POLL and LIST_MAX (seconds between looks for the
# dispatched run, default 5, and how long, default 60).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

GH=${GH:-gh}
POLL=${POLL:-15}
WAIT_MAX=${WAIT_MAX:-900}
LIST_POLL=${LIST_POLL:-5}
LIST_MAX=${LIST_MAX:-60}
: "${HEAD_SHA:?HEAD_SHA}" "${BRANCH:?BRANCH}" "${GH_REPO:?GH_REPO}"

# runs: the e2e-kind runs that test the head (pull_request, and dispatch without a `commit` input:
# a bisect dispatch tests another commit), one per line: <id> <status> <event>.
runs() {
  "$GH" api "repos/$GH_REPO/actions/workflows/e2e-kind.yml/runs?head_sha=$HEAD_SHA&per_page=50" --jq '
    .workflow_runs[] | select(.event == "pull_request" or (.event == "workflow_dispatch" and .display_title == "e2e-kind"))
    | "\(.id) \(.status) \(.event)"'
}
# quickstart_of <run id>: none (no quickstart job yet: decide has not finished), pending, skipped,
# success, or the first other conclusion. A job that needs another is created when that one ends.
quickstart_of() {
  "$GH" api "repos/$GH_REPO/actions/runs/$1/jobs?per_page=100" --jq '
    [.jobs[] | select(.name | startswith("quickstart")) | if .status != "completed" then "pending" else (.conclusion // "pending") end]
    | if length == 0 then "none"
      elif any(.[]; . == "pending") then "pending"
      elif all(.[]; . == "skipped") then "skipped"
      elif all(.[]; . == "success" or . == "skipped") then "success"
      else ([.[] | select(. != "success" and . != "skipped")][0]) end'
}

waited=0
while :; do
  list=$(runs) || { echo "::error::could not list the e2e-kind runs on ${HEAD_SHA:0:12}"; exit 1; }
  undecided=""
  while read -r id status _; do
    [[ -n $id ]] || continue
    q=$(quickstart_of "$id") || q=none
    case $q in
    success) echo "run $id already passed the e2e on ${HEAD_SHA:0:12}; nothing to do"; exit 0 ;;
    pending) echo "run $id runs the e2e on ${HEAD_SHA:0:12}; nothing to do"; exit 0 ;;
    none) [[ $status == completed ]] || undecided+=${undecided:+,}$id ;;
    esac
  done <<<"$list"
  [[ -n $undecided ]] || break
  if [[ $waited -ge $WAIT_MAX ]]; then
    echo "run $undecided has not decided after ${WAIT_MAX}s; dispatching anyway"
    break
  fi
  echo "waiting for run $undecided to decide"
  sleep "$POLL"
  waited=$((waited + (POLL > 0 ? POLL : 1)))
done

# A push since the label: its own run reads the label live and runs the e2e.
now=$("$GH" api "repos/$GH_REPO/branches/$BRANCH" --jq .commit.sha) || now=$HEAD_SHA
if [[ $now != "$HEAD_SHA" ]]; then
  echo "$BRANCH moved to ${now:0:12}; its own run takes the label"
  exit 0
fi
echo "dispatching e2e-kind on $BRANCH (${HEAD_SHA:0:12})"
before=$(awk '$3 == "workflow_dispatch" {print $1}' <<<"$list")
"$GH" workflow run e2e-kind.yml --ref "$BRANCH"

# Until the new run is listed, hack/pr-wait.sh sees nothing live on the head.
looked=0
while :; do
  new=$(runs | awk -v before=" $(tr '\n' ' ' <<<"$before") " '$3 == "workflow_dispatch" && index(before, " " $1 " ") == 0 {print $1; exit}') || new=""
  if [[ -n $new ]]; then
    echo "dispatched run $new on ${HEAD_SHA:0:12}"
    exit 0
  fi
  if [[ $looked -ge $LIST_MAX ]]; then
    echo "::error::no e2e-kind dispatch run on ${HEAD_SHA:0:12} appeared within ${LIST_MAX}s of the dispatch"
    exit 1
  fi
  sleep "$LIST_POLL"
  looked=$((looked + (LIST_POLL > 0 ? LIST_POLL : 1)))
done
