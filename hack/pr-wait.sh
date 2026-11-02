#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Wait on a pull request without a hand-written loop. Polls `gh pr view` and prints one line per
# state change, so it is cheap to run in the background and read later. It only reports: it never
# merges, never marks ready and never enables auto-merge.
#
# Usage: hack/pr-wait.sh <PR> [--merged] [--checks [--require CHECK|e2e]...] [--timeout DURATION] [--interval SECONDS]
#   --merged   (default) wait until the PR is merged or closed
#   --checks   wait until every check has finished
#   --require  (repeatable, with --checks) a check that must be green: once the others finish, a
#              required check that is missing, skipped or not green exits 1 (`REQUIRED <name> missing|skipped|failed`).
#              Only the newest check run of that name on the head counts: a later success supersedes an
#              earlier skip or cancel, and a cancelled or skipped run never outranks one that is queued or
#              in progress (even at the same startedAt): it keeps waiting for the live run. A required check
#              that is missing, skipped or cancelled while an e2e-kind run on the head is still queued or in
#              progress (whose jobs may not exist yet, #468) is waited for too: `waiting for e2e-kind run <id>`;
#              so are failed or cancelled checks of an earlier e2e-kind run on the head (its `decide`
#              cancelled, its `e2e result` red). Checks of e2e-kind count by their newest run, like
#              required ones. All green is not final while an e2e-kind run on the head is queued or
#              in progress either (the `e2e` label dispatches one, whose `e2e result` comes last).
#              Otherwise, if the newest is skipped or cancelled, the next line suggests
#              `gh workflow run e2e-kind.yml --ref <branch>`.
#              `--require e2e` is the merge gate for a needs-e2e PR (#547): it passes when the commit status
#              `e2e-lab` (posted by hack/e2e-remote.sh --argo after a green lab run) is success on the head,
#              OR any `quickstart` check run on the head succeeded. Otherwise it behaves like
#              `--require quickstart` and prints `REQUIRED e2e missing|skipped|failed` with how to produce one.
#              Every required check is also read from the head commit's check runs (`gh api
#              commits/<sha>/check-runs`), not only from the PR rollup, which lists just one run per name: a
#              dispatched run that passed counts next to a skipped pull_request run. Any success on the head
#              wins over skipped or cancelled runs of the same check.
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
  echo "usage: hack/pr-wait.sh <PR> [--merged] [--checks] [--require CHECK|e2e] [--timeout DURATION] [--interval SECONDS]" >&2
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
required=() alias_e2e=0
while [ $# -gt 0 ]; do
  case $1 in
  --merged) mode=merged ;;
  --checks) mode=checks ;;
  --require)
    [ $# -ge 2 ] || usage
    if [ "$2" = e2e ]; then
      alias_e2e=1
      required+=(quickstart)
    else
      required+=("$2")
    fi
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
# The checks of the e2e-kind workflow count the same way: several runs on one head are normal there.
# shellcheck disable=SC2016 # $req is a jq variable, bound by jqn
norm='def ok: .done and (.bad | not) and (.skipped | not);
def newest: sort_by(.ts) | . as $s | ($s | last) as $l
  | if any($s[]; ok) then ([$s[] | select(ok)] | last) elif ($l.done and ($l.cancelled or $l.skipped)) and any($s[]; .done | not) then ([$s[] | select(.done | not)] | last) else $l end;
[(.statusCheckRollup // [])[] | {
  name: (.name // .context // "?"),
  ts: (.startedAt // .completedAt // ""),
  cancelled: ((.conclusion // "") == "CANCELLED"),
  url: (.detailsUrl // .targetUrl // ""),
  wf: (.workflowName // ""),
  done: ((.status == "COMPLETED") or ((.state // "") | IN("SUCCESS", "FAILURE", "ERROR"))),
  skipped: ((.conclusion // "") == "SKIPPED"),
  bad: ((.conclusion // .state // "") | IN("FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "ERROR"))
}] | group_by(.name) | map(if (.[0].name | IN($req[])) or all(.[]; .wf == "e2e-kind") then [newest] else . end) | add // []'

reqjson="[]"
jqn() { jq --argjson req "$reqjson" "$@"; }

# head_runs <sha>: every check run of the commit as rollup-shaped rows (a JSON array); [] when gh fails.
head_runs() {
  local rows
  rows=$(gh api "repos/{owner}/{repo}/commits/$1/check-runs?per_page=100" --paginate --jq '.check_runs[] | {
    name: .name, status: ((.status // "") | ascii_upcase), conclusion: ((.conclusion // "") | ascii_upcase),
    startedAt: (.started_at // ""), completedAt: (.completed_at // ""), detailsUrl: (.details_url // .html_url // ""), workflowName: ""}' 2>/dev/null |
    jq -sc . 2>/dev/null) || rows=""
  echo "${rows:-[]}"
}

# live_e2e <sha>: the e2e-kind runs on the commit that have not completed (queued, waiting, pending,
# in progress), comma-separated. A run's jobs, and so its checks, appear only once it starts.
live_e2e() {
  [ "$no_live" = 0 ] || return 0 # `--require e2e` is already satisfied: a live e2e-kind run does not matter
  gh run list --commit "$1" --workflow e2e-kind.yml --limit 20 --json databaseId,status \
    --jq '[.[] | select(.status != "completed") | .databaseId | tostring] | join(",")' 2>/dev/null || true
}

# shown <check>: the name to print; `--require e2e` is judged on quickstart (and e2e-lab).
shown() { if [ "$alias_e2e" = 1 ] && [ "$1" = quickstart ]; then echo e2e; else echo "$1"; fi; }

no_live=0 last=""
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
    # Required checks are judged on every check run of the head commit, not only on the PR rollup, which
    # lists one run per name (a dispatched run that passed hides behind the skipped pull_request run).
    req_now=()
    for r in ${required[@]+"${required[@]}"}; do req_now+=("$r"); done
    no_live=0
    if [ ${#req_now[@]} -gt 0 ] && [ -n "$head" ]; then
      extra=$(head_runs "$head")
      json=$(jq --argjson x "$extra" --argjson names "$(printf '%s\n' "${req_now[@]}" | jq -R . | jq -sc .)" \
        '.statusCheckRollup = ((.statusCheckRollup // []) + [$x[]? | select(.name | IN($names[]))])' <<<"$json")
    fi
    if [ "$alias_e2e" = 1 ]; then
      # `e2e` is satisfied by a green e2e-lab status or a green quickstart run on the head; then the other
      # runs of those two names (skipped, cancelled, red) no longer matter.
      if jq -e 'any((.statusCheckRollup // [])[]; ((.name // .context // "") == "e2e-lab" and (.state // "") == "SUCCESS")
          or ((.name // "") == "quickstart" and (.status // "") == "COMPLETED" and (.conclusion // "") == "SUCCESS"))' <<<"$json" >/dev/null; then
        json=$(jq '.statusCheckRollup |= map(select(((.name // .context // "") | IN("quickstart", "e2e-lab") | not)
          or (.state // "") == "SUCCESS" or (.conclusion // "") == "SUCCESS"))' <<<"$json")
        req_now=()
        for r in ${required[@]+"${required[@]}"}; do [ "$r" = quickstart ] || req_now+=("$r"); done
        no_live=1
      fi
    fi
    reqjson=$(printf '%s\n' ${req_now[@]+"${req_now[@]}"} | jq -R . | jq -sc .)
    failed=$(jqn -r "$norm | .[] | select(.bad) | [.name, .cancelled, ((.url | capture(\"/runs/(?<r>[0-9]+)\")? | .r) // \"\")] | @tsv" <<<"$json")
    # The only failures are cancelled runs of required checks, or checks of an earlier e2e-kind run:
    # an e2e-kind run on the head that is still queued or running brings newer ones (#468), so wait
    # for it instead of failing.
    if [ -n "$failed" ] && [ -n "$head" ] &&
      jqn -e "$norm | [.[] | select(.bad)] | all((.cancelled and (.name | IN(\$req[]))) or .wf == \"e2e-kind\")" <<<"$json" >/dev/null; then
      live=$(live_e2e "$head")
    fi
    if [ -z "$live" ] && [ -n "$failed" ]; then
      while IFS=$'\t' read -r name cancelled run; do
        if [ -n "$run" ]; then echo "FAILED $name (run $run)"; else echo "FAILED $name"; fi
        for r in ${req_now[@]+"${req_now[@]}"}; do
          [ "$r" != "$name" ] || echo "REQUIRED $(shown "$name") failed"
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
      # Every check has finished, but an e2e-kind run on the head that has not created its jobs yet
      # (queued; or dispatched by the `e2e` label, #468) brings newer ones: wait for it.
      [ -z "$head" ] || live=$(live_e2e "$head")
      if [ -n "$live" ]; then
        emit "waiting for e2e-kind run $live on ${head:0:7}"
      elif [ "$mode" = checks ] && [ ${#req_now[@]} -gt 0 ]; then
        whys=()
        for name in "${req_now[@]}"; do
          why=$(jqn -r --arg n "$name" "$norm | [.[] | select(.name == \$n)] | if length == 0 then \"missing\" elif any(.[]; .skipped) then \"skipped\" else \"\" end" <<<"$json")
          [ -z "$why" ] || whys+=("$name"$'\t'"$why")
        done
        if [ ${#whys[@]} -gt 0 ]; then
          for w in "${whys[@]}"; do
            name=${w%%$'\t'*} why=${w#*$'\t'}
            echo "REQUIRED $(shown "$name") $why"
            if [ "$alias_e2e" = 1 ] && [ "$name" = quickstart ]; then
              echo "no green e2e on this head ($why quickstart, no e2e-lab success); run: hack/e2e-remote.sh --argo (posts e2e-lab), or gh workflow run e2e-kind.yml --ref $branch"
            elif [ "$why" = skipped ]; then
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
