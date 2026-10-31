#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Wait on a pull request without a hand-written loop. Polls `gh pr view` and prints one line per
# state change, so it is cheap to run in the background and read later. It only reports: it never
# merges, never marks ready and never enables auto-merge.
#
# Usage: hack/pr-wait.sh <PR> [--merged] [--checks] [--timeout DURATION] [--interval SECONDS]
#   --merged   (default) wait until the PR is merged or closed
#   --checks   wait until every check has finished
#   --timeout  90m by default; 30s, 15m, 2h or plain seconds
#   --interval 20 seconds by default
#
# Output lines: `checks 7/9 pending: go,web`, `FAILED go (run 123)`, `MERGED abc1234`,
# `needs-update`, `CLOSED`, `checks 9/9 green`, `TIMEOUT`.
# Exit: 0 merged (or all checks green with --checks), 1 a check failed, 2 timeout,
#       3 closed without merge, 4 merge conflict (mergeStateStatus DIRTY), 64 usage.
set -euo pipefail
export GH_PAGER=cat PAGER=cat

usage() {
  echo "usage: hack/pr-wait.sh <PR> [--merged] [--checks] [--timeout DURATION] [--interval SECONDS]" >&2
  exit 64
}

duration() { # 90m | 30s | 2h | 45 -> seconds
  local n=$1 mul=1
  case $n in
  *h) mul=3600 n=${n%h} ;;
  *m) mul=60 n=${n%m} ;;
  *s) n=${n%s} ;;
  esac
  case $n in '' | *[!0-9]*) return 1 ;; esac
  echo $((n * mul))
}

pr="" mode=merged timeout=5400 interval=20
while [ $# -gt 0 ]; do
  case $1 in
  --merged) mode=merged ;;
  --checks) mode=checks ;;
  --timeout)
    [ $# -ge 2 ] || usage
    timeout=$(duration "$2") || usage
    shift
    ;;
  --interval)
    [ $# -ge 2 ] || usage
    interval=$(duration "$2") || usage
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

# One normalised row per check: CheckRuns (status/conclusion) and commit statuses (state) alike.
norm='[(.statusCheckRollup // [])[] | {
  name: (.name // .context // "?"),
  url: (.detailsUrl // .targetUrl // ""),
  done: ((.status == "COMPLETED") or ((.state // "") | IN("SUCCESS", "FAILURE", "ERROR"))),
  bad: ((.conclusion // .state // "") | IN("FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "ERROR"))
}]'

last=""
emit() { # print only when the line differs from the previous one
  if [ "$1" != "$last" ]; then
    echo "$1"
    last=$1
  fi
}

start=$SECONDS
while :; do
  if json=$(gh pr view "$pr" --json state,mergeStateStatus,mergeCommit,statusCheckRollup 2>/dev/null) && [ -n "$json" ]; then
    state=$(jq -r '.state // ""' <<<"$json")
    mstate=$(jq -r '.mergeStateStatus // ""' <<<"$json")
    case $state in
    MERGED)
      sha=$(jq -r '.mergeCommit.oid // ""' <<<"$json")
      echo "MERGED ${sha:0:7}"
      exit 0
      ;;
    CLOSED)
      echo "CLOSED"
      exit 3
      ;;
    esac
    failed=$(jq -r "$norm | .[] | select(.bad) | [.name, ((.url | capture(\"/runs/(?<r>[0-9]+)\")? | .r) // \"\")] | @tsv" <<<"$json")
    if [ -n "$failed" ]; then
      while IFS=$'\t' read -r name run; do
        if [ -n "$run" ]; then echo "FAILED $name (run $run)"; else echo "FAILED $name"; fi
      done <<<"$failed"
      exit 1
    fi
    if [ "$mstate" = DIRTY ]; then
      echo "needs-update"
      exit 4
    fi
    total=$(jq "$norm | length" <<<"$json")
    pending=$(jq -r "$norm | [.[] | select(.done | not) | .name] | sort | join(\",\")" <<<"$json")
    ndone=$(jq "$norm | [.[] | select(.done)] | length" <<<"$json")
    if [ "$total" -gt 0 ] && [ -z "$pending" ]; then
      if [ "$mode" = checks ]; then
        echo "checks $total/$total green"
        exit 0
      fi
      emit "checks $total/$total green"
    elif [ -n "$pending" ]; then
      emit "checks $ndone/$total pending: $pending"
    else
      emit "checks 0/0"
    fi
  fi
  if [ $((SECONDS - start)) -ge "$timeout" ]; then
    echo "TIMEOUT"
    exit 2
  fi
  [ "$interval" -eq 0 ] || sleep "$interval"
done
