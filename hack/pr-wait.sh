#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Wait on a pull request without a hand-written loop. Polls `gh pr view` and prints one line per
# state change, so it is cheap to run in the background and read later. It only reports: it never
# merges, never marks ready and never enables auto-merge.
#
# Usage: hack/pr-wait.sh <PR> [--merged] [--checks [--require CHECK]...] [--timeout DURATION] [--interval SECONDS]
#   --merged   (default) wait until the PR is merged or closed
#   --checks   wait until every check has finished
#   --require  (repeatable, with --checks) a check that must be green: once the others finish, a
#              required check that is missing, skipped or not green exits 1 (`REQUIRED <name> missing|skipped|failed`).
#              Only the newest check run of that name on the head counts: a later success supersedes an
#              earlier skip or cancel, and a cancelled or skipped run never outranks one that is queued or
#              in progress (even at the same startedAt): it keeps waiting for the live run. A required check
#              that is missing, skipped or cancelled while an e2e-kind run on the head is still queued or in
#              progress (whose jobs may not exist yet, #468) is waited for too: `waiting for e2e-kind run <id>`.
#              Otherwise, if the newest is skipped or cancelled, the next line suggests
#              `gh workflow run e2e-kind.yml --ref <branch>`.
#   --timeout  90m by default; 30s, 15m, 2h or plain seconds
#   --interval 20 seconds by default
#
# Output lines: `checks 7/9 pending: go,web`, `FAILED go (run 123)`, `MERGED abc1234`,
# `needs-update`, `CLOSED`, `checks 9/9 green`, `REQUIRED e2e result skipped`, `TIMEOUT`.
# Exit: 0 merged (or all checks green with --checks), 1 a check failed, 2 timeout,
#       3 closed without merge, 4 merge conflict (mergeStateStatus DIRTY), 64 usage.
set -euo pipefail
export GH_PAGER=cat PAGER=cat

usage() {
  echo "usage: hack/pr-wait.sh <PR> [--merged] [--checks] [--require CHECK] [--timeout DURATION] [--interval SECONDS]" >&2
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
required=()
while [ $# -gt 0 ]; do
  case $1 in
  --merged) mode=merged ;;
  --checks) mode=checks ;;
  --require)
    [ $# -ge 2 ] || usage
    required+=("$2")
    shift
    ;;
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
[ ${#required[@]} -eq 0 ] || [ "$mode" = checks ] || usage

# One normalised row per check: CheckRuns (status/conclusion) and commit statuses (state) alike.
# A required check is judged on its newest run only (by start time; rollup order breaks ties). A
# cancelled or skipped run never outranks a queued or in-progress one (#431): wait for the live run.
# shellcheck disable=SC2016 # $req is a jq variable, bound by jqn
norm='def newest: sort_by(.ts) | . as $s | ($s | last) as $l
  | if ($l.done and ($l.cancelled or $l.skipped)) and any($s[]; .done | not) then ([$s[] | select(.done | not)] | last) else $l end;
[(.statusCheckRollup // [])[] | {
  name: (.name // .context // "?"),
  ts: (.startedAt // .completedAt // ""),
  cancelled: ((.conclusion // "") == "CANCELLED"),
  url: (.detailsUrl // .targetUrl // ""),
  done: ((.status == "COMPLETED") or ((.state // "") | IN("SUCCESS", "FAILURE", "ERROR"))),
  skipped: ((.conclusion // "") == "SKIPPED"),
  bad: ((.conclusion // .state // "") | IN("FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "ERROR"))
}] | group_by(.name) | map(if (.[0].name | IN($req[])) then [newest] else . end) | add // []'

reqjson=$(printf '%s\n' ${required[@]+"${required[@]}"} | jq -R . | jq -sc .)
jqn() { jq --argjson req "$reqjson" "$@"; }

# live_e2e <sha>: the e2e-kind runs on the commit that have not completed (queued, waiting, pending,
# in progress), comma-separated. A run's jobs, and so its checks, appear only once it starts.
live_e2e() {
  gh run list --commit "$1" --workflow e2e-kind.yml --limit 20 --json databaseId,status \
    --jq '[.[] | select(.status != "completed") | .databaseId | tostring] | join(",")' 2>/dev/null || true
}

last=""
emit() { # print only when the line differs from the previous one
  if [ "$1" != "$last" ]; then
    echo "$1"
    last=$1
  fi
}

start=$SECONDS
while :; do
  if json=$(gh pr view "$pr" --json state,mergeStateStatus,mergeCommit,statusCheckRollup,headRefName,headRefOid 2>/dev/null) && [ -n "$json" ]; then
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
    branch=$(jq -r '.headRefName // "<branch>"' <<<"$json")
    head=$(jq -r '.headRefOid // ""' <<<"$json")
    live=""
    failed=$(jqn -r "$norm | .[] | select(.bad) | [.name, .cancelled, ((.url | capture(\"/runs/(?<r>[0-9]+)\")? | .r) // \"\")] | @tsv" <<<"$json")
    # The only failures are cancelled runs of required checks: an e2e-kind run on the head that has
    # not created its jobs yet may still bring them (#468), so wait for it instead of failing.
    if [ -n "$failed" ] && [ -n "$head" ] &&
      jqn -e "$norm | [.[] | select(.bad)] | all(.cancelled and (.name | IN(\$req[])))" <<<"$json" >/dev/null; then
      live=$(live_e2e "$head")
    fi
    if [ -z "$live" ] && [ -n "$failed" ]; then
      while IFS=$'\t' read -r name cancelled run; do
        if [ -n "$run" ]; then echo "FAILED $name (run $run)"; else echo "FAILED $name"; fi
        for r in ${required[@]+"${required[@]}"}; do
          [ "$r" != "$name" ] || echo "REQUIRED $name failed"
          if [ "$r" = "$name" ] && [ "$cancelled" = true ]; then
            echo "only cancelled runs of $name on this head; run: gh workflow run e2e-kind.yml --ref $branch"
          fi
        done
      done <<<"$failed"
      exit 1
    fi
    if [ "$mstate" = DIRTY ]; then
      echo "needs-update"
      exit 4
    fi
    total=$(jqn "$norm | length" <<<"$json")
    pending=$(jqn -r "$norm | [.[] | select(.done | not) | .name] | sort | join(\",\")" <<<"$json")
    ndone=$(jqn "$norm | [.[] | select(.done)] | length" <<<"$json")
    if [ -n "$live" ]; then
      emit "waiting for e2e-kind run $live on ${head:0:7}"
    elif [ "$total" -gt 0 ] && [ -z "$pending" ]; then
      if [ "$mode" = checks ] && [ ${#required[@]} -gt 0 ]; then
        whys=()
        for name in "${required[@]}"; do
          why=$(jqn -r --arg n "$name" "$norm | [.[] | select(.name == \$n)] | if length == 0 then \"missing\" elif any(.[]; .skipped) then \"skipped\" else \"\" end" <<<"$json")
          [ -z "$why" ] || whys+=("$name"$'\t'"$why")
        done
        # A required check that is missing or skipped may still come from a live e2e-kind run.
        [ ${#whys[@]} -eq 0 ] || [ -z "$head" ] || live=$(live_e2e "$head")
        if [ -n "$live" ]; then
          emit "waiting for e2e-kind run $live on ${head:0:7}"
        elif [ ${#whys[@]} -gt 0 ]; then
          for w in "${whys[@]}"; do
            name=${w%%$'\t'*} why=${w#*$'\t'}
            echo "REQUIRED $name $why"
            if [ "$why" = skipped ]; then
              echo "only skipped runs of $name on this head; run: gh workflow run e2e-kind.yml --ref $branch"
            fi
          done
          exit 1
        fi
      fi
      if [ -z "$live" ] && [ "$mode" = checks ]; then
        echo "checks $total/$total green"
        exit 0
      fi
      [ -n "$live" ] || emit "checks $total/$total green"
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
