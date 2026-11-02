#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The e2e-kind workflow's `e2e result` job: the one check to require. Exits 0 when the e2e passed or
# was not needed, 1 otherwise, and says why on stdout (`::error::` lines for the run's annotations).
#
#   COVERED=<run id>  decide found that run already ran the e2e on this head (the `e2e` label added
#                     after `gh pr create`, #468): green only when that run's own quickstart job
#                     concluded success. Waits for it if it has not finished.
#   MIRROR=true       another label, or removing `e2e`, started this run without the e2e. A skip must
#                     neither replace an earlier failure nor go green ahead of an e2e still running on
#                     this head (#468): wait while another e2e-kind run on the head is in progress and
#                     runs (or may run) quickstart, then mirror the newest one that ran quickstart
#                     (none: the newest finished run whose decide ran). A run that has not started
#                     writes its own, newer check, and a mirror-only run neither waits for another
#                     mirror-only run nor passes its verdict on: no two label runs wait on each other.
#                     Dispatch runs count only when they tested the head itself (no `commit` input:
#                     the run name is plain `e2e-kind`); a bisect dispatch tests another commit.
#                     Waiting, not failing, keeps the check green on the normal path (a bot's
#                     `needs-update` during an e2e); a red check there would block auto-merge.
#   otherwise         decide and quickstart must each be success or skipped.
#
# Environment: DECIDE, QUICKSTART (the jobs' results), COVERED, MIRROR, HEAD_SHA, RUN_ID (this run),
# GH (the gh command), GH_REPO, POLL (seconds between looks, default 30), WAIT_MAX (seconds, default
# 5400; the quickstart job's own limit is 75 minutes).
set -euo pipefail

GH=${GH:-gh}
POLL=${POLL:-30}
WAIT_MAX=${WAIT_MAX:-5400}
COVERED=${COVERED:-}
MIRROR=${MIRROR:-false}
[[ $COVERED != true && $COVERED != false ]] || COVERED="" # an older decide printed covered=true

fail() {
  echo "::error::$*"
  exit 1
}

# job_of <run id> <job name prefix>: the verdict of the run's jobs of that name: pending, none (no
# such job yet), skipped, success, or the first other conclusion (failure, cancelled, timed_out, ...).
job_of() {
  "$GH" api "repos/$GH_REPO/actions/runs/$1/jobs?per_page=100" --jq '
    [.jobs[] | select(.name | startswith("'"$2"'")) | if .status != "completed" then "pending" else (.conclusion // "pending") end]
    | if length == 0 then "none"
      elif any(.[]; . == "pending") then "pending"
      elif all(.[]; . == "skipped") then "skipped"
      elif all(.[]; . == "success" or . == "skipped") then "success"
      else ([.[] | select(. != "success" and . != "skipped")][0]) end'
}
quickstart_of() { job_of "$1" quickstart; }

# others: the other e2e-kind runs that test the head (pull_request, and dispatch without a `commit`
# input), oldest first, one per line: <id> <status> <conclusion or ->.
others() {
  "$GH" api "repos/$GH_REPO/actions/workflows/e2e-kind.yml/runs?head_sha=$HEAD_SHA&per_page=50" --jq "
    [.workflow_runs[] | select(.id != ${RUN_ID:-0} and (.event == \"pull_request\" or (.event == \"workflow_dispatch\" and .display_title == \"e2e-kind\")))]
    | sort_by(.created_at)[] | \"\(.id) \(.status) \(.conclusion // \"-\")\""
}

waited=0
wait_a_bit() { # <what>
  if [[ $waited -ge $WAIT_MAX ]]; then fail "gave up after ${WAIT_MAX}s waiting for $1"; fi
  sleep "$POLL"
  waited=$((waited + (POLL > 0 ? POLL : 1)))
}

echo "decide: ${DECIDE:-}, quickstart: ${QUICKSTART:-}"

if [[ -n $COVERED ]]; then
  while :; do
    q=$(quickstart_of "$COVERED") || q=unknown
    case $q in
    pending | none | unknown) echo "waiting for the e2e of run $COVERED ($q)"; wait_a_bit "run $COVERED" ;;
    success) echo "run $COVERED ran the e2e on this commit: success"; exit 0 ;;
    *) fail "run $COVERED ran the e2e on this commit: $q. Re-run that run, or: gh workflow run e2e-kind.yml --ref <branch>" ;;
    esac
  done
fi

if [[ $MIRROR == true ]]; then
  while :; do
    runs=$(others) || fail "could not list the e2e-kind runs on ${HEAD_SHA:0:12}"
    live=""
    while read -r id status _; do
      [[ $status == in_progress ]] || continue
      case $(quickstart_of "$id" || echo none) in pending | none) live+=${live:+,}$id ;; esac
    done <<<"$runs"
    [[ -n $live ]] || break
    echo "waiting for e2e-kind run $live on ${HEAD_SHA:0:12}"
    wait_a_bit "run $live"
  done
  newest="" verdict=""
  while read -r id _ _; do
    [[ -n $id ]] || continue
    q=$(quickstart_of "$id") || fail "could not read the jobs of run $id"
    case $q in none | skipped | pending) ;; *) newest=$id verdict=$q ;; esac
  done <<<"$runs"
  if [[ -n $newest ]]; then
    echo "newest run that ran the e2e on this commit: $newest ($verdict)"
    [[ $verdict == success ]] || fail "the e2e failed on this commit (run $newest: $verdict)"
    exit 0
  fi
  prev=none
  while read -r id status concl; do
    [[ $status == completed ]] || continue
    case $(job_of "$id" decide || echo none) in skipped | none) ;; *) prev=$concl ;; esac
  done <<<"$runs"
  echo "no run ran the e2e on this commit; newest finished run that decided: $prev"
  case $prev in failure | cancelled | timed_out | startup_failure) fail "the last e2e-kind run on this commit concluded $prev" ;; esac
  exit 0
fi

for r in "${DECIDE:-}" "${QUICKSTART:-}"; do
  case $r in success | skipped) ;; *) fail "the e2e did not pass ($r)" ;; esac
done
