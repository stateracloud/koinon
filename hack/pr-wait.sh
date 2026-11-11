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
#              in progress (even at the same startedAt): it keeps waiting for the live run.
#              The e2e is not a merge gate on the head (ADR 0105): the merge queue's `quickstart` is the proof. A PR's
#              own `quickstart` is advisory; a red one prints `advisory quickstart failed` and does not decide.
#              Every required check is also read from the head commit's check runs (`gh api
#              commits/<sha>/check-runs`), not only from the PR rollup, which lists just one run per name: a
#              dispatched run that passed counts next to a skipped pull_request run. A success on the head
#              wins over a newer skipped or cancelled run of the same check, but a newer failure stays failed.
#              With --checks, "all green" also needs every required status check context of the base
#              branch's rules (gh api repos/{owner}/{repo}/rules/branches/<base>) to exist on the head:
#              a workflow run that is still queued has no jobs yet, so its required checks are missing
#              from the rollup (`waiting for required checks not on the head yet: ci,...`, old issue 562).
#              A draft PR is never green (old issue 647): with --checks, `isDraft` exits 5 with `DRAFT` (a draft runs
#              nothing heavy; its `go`/`web` report success with "draft: not run", which is NOT evidence). A PR that is not a draft ignores the check
#              runs of its draft-era `ci` runs (#218, #133), in every mode: a run whose display title is `ci (draft)`
#              (the run-name of ci.yml when the event payload says draft), matched by run id. There is no time
#              window, and a by-hand re-run of a draft run is ignored too. A non-draft run is never dropped: red
#              or cancelled it still fails. It prints `waiting for the ready run` until the ready run's checks
#              exist (between `gh pr ready` and that run the draft run is the only `ci` run on the head, and its
#              vacuous green must never decide). dco, gitleaks, claim and commit statuses keep their result. When gh cannot list
#              the runs, nothing is dropped.
#              A stale PR head (old issue 661): with --checks, when `git ls-remote origin refs/heads/<branch>` names
#              another commit than the PR's headRefOid for PR_WAIT_STALE_GRACE seconds (default 120; GitHub
#              lags a push by a few seconds), it exits 6 with `STALE-HEAD` instead of judging the old head.
#              Not checked for a fork PR or when the remote cannot be read.
#              Unresolved review threads (#187): when every check is green but mergeStateStatus is BLOCKED, the
#              ruleset's required_review_thread_resolution is the usual cause (advisory bots comment inline).
#              hack/pr-threads.sh lists the unresolved threads (`UNRESOLVED path:line author`) and this exits 7
#              at once, without waiting: threads do not resolve by themselves. It never resolves one; reply
#              and resolve them as docs/dev/ci.md "Merging" says. When the threads cannot be read (after
#              three tries), it exits 2: it never reports a PR as ready on a guess.
#   --timeout  90m by default; 30s, 15m, 2h or plain seconds
#   --interval 20 seconds by default
#
# Output lines: `checks 7/9 pending: go,web`, `FAILED go (run 123)`, `MERGED abc1234`,
# `conflict: merge origin/main`, `CLOSED`, `checks 9/9 green`, `REQUIRED e2e result skipped`, `TIMEOUT`.
# Exit: 0 merged (or all checks green with --checks), 1 a check failed, 2 timeout (or, with --checks, the required contexts / review threads unreadable),
#       3 closed without merge, 4 merge conflict (mergeStateStatus DIRTY), 5 draft (with --checks),
#       6 stale PR head (with --checks), 7 unresolved review threads (BLOCKED with --checks, all green), 64 usage.
#       8 the PR was in the merge queue and left it without merging (DEQUEUED).
#       Merge queue (#225): a queued PR is OPEN until the queue build merges it; the wait prints
#       `in the merge queue (<position> <state>)` and continues to `MERGED` (exit 0). With --checks the head's
#       checks are judged as before: they gate entry to the queue; the queue build's own checks are on the
#       queue commit (gh-readonly-queue/...), not on the head.
# Test hook: PR_WAIT_THREADS replaces hack/pr-threads.sh.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

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
    [ "$2" != e2e ] || { echo "pr-wait: --require e2e is gone (ADR 0105): the merge queue's quickstart is the e2e proof" >&2; exit 64; }
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

# One normalised row per check, judged by hack/lib/pr-verdict.sh (#133): the same rows and the same ordering as
# hack/pr-merge.sh. A name is judged on its newest row of the workflow run that counts; see that file's header.
# shellcheck source=/dev/null
. "$(cd "$(dirname "$0")" && pwd)/lib/pr-verdict.sh"
# shellcheck disable=SC2016 # $req is a jq variable, bound by jqn
norm="$PRV_JQ"'[(.statusCheckRollup // [])[] | prv_row] | prv_collapse'

jqn() { jq "$@"; }

# head_runs <sha>: every check run of the commit as rollup-shaped rows (a JSON array); [] when gh fails.
head_runs() {
  local rows
  rows=$(prv_check_rows "$1") || rows=""
  echo "${rows:-[]}"
}

# load_contexts <base branch>: the required status check contexts of the base branch's rules (the
# ruleset), one per line, into $ctxs. Read until it succeeds once; a failed read leaves it empty.
# A workflow run that is queued has no jobs yet, so its required checks are absent from the head's
# rollup: "everything in the rollup is green" is not "every required check is green".
ctx_ok=0 ctx_tried=0 ctxs=""
load_contexts() {
  [ "$ctx_ok" = 0 ] || return 0
  local out try
  local -a sleeps
  read -r -a sleeps <<<"${PR_THREADS_RETRY_SLEEPS:-2 5 15}" # the same retries (and test override) as pr-threads.sh
  for ((try = 0; try <= ${#sleeps[@]}; try++)); do
    if out=$(gh api "repos/{owner}/{repo}/rules/branches/$1" \
      --jq '[.[] | select(.type == "required_status_checks") | .parameters.required_status_checks[].context] | unique | .[]' 2>/dev/null); then
      ctxs=$out ctx_ok=1
      return 0
    fi
    [ "$try" -lt "${#sleeps[@]}" ] && sleep "${sleeps[$try]}"
  done
  return 0
}

queue_state() { prv_queue_state "$1"; }

last="" was_queued=0 dq_polls=0
emit() { # print only when the line differs from the previous one
  if [ "$1" != "$last" ]; then
    echo "$1"
    last=$1
  fi
}

threads_sh=${PR_WAIT_THREADS:-$(cd "$(dirname "$0")" && pwd)/pr-threads.sh}
stale_grace=${PR_WAIT_STALE_GRACE:-120} stale_since=""
start=$SECONDS
while :; do
  if json=$(gh pr view "$pr" --json state,isDraft,mergeStateStatus,mergeCommit,statusCheckRollup,headRefName,headRefOid,baseRefName,isCrossRepository 2>/dev/null) && [ -n "$json" ]; then
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
    # The merge queue (#225): an enqueued PR stays OPEN until the queue build merges it, so
    # --merged keeps waiting and says where it stands. A PR that was in the queue and is OPEN and out of it
    # again was removed (a failed queue build, a conflict with the entry ahead, or by hand): exit 8, with
    # the checks of the queue build named by `hack/ci-fail.sh`/the PR page. Never re-enqueues.
    # Only in the merged mode: --checks judges the head's checks as before, queued or not (pr-merge --admin).
    qs=""
    [ "$mode" != merged ] || qs=$(queue_state "$pr")
    case $qs in
    queued*)
      was_queued=1 dq_polls=0
      emit "in the merge queue (${qs#queued })"
      if [ $((SECONDS - start)) -ge "$timeout" ]; then echo "TIMEOUT"; exit 2; fi
      [ "$interval" -eq 0 ] || sleep "$interval"
      continue
      ;;
    unknown)
      # The queue state could not be read (rate limit, outage). Once the PR was queued that says nothing about
      # whether it left: keep waiting. It neither counts toward dq_polls nor resets it, so DEQUEUED still needs
      # two real "not queued" answers in a row, however many unreadable ones sit between them. Before anything
      # was queued it is read as "not queued", as it always was.
      if [ "$was_queued" = 1 ]; then
        emit "merge queue state unreadable; waiting"
        if [ $((SECONDS - start)) -ge "$timeout" ]; then echo "TIMEOUT"; exit 2; fi
        [ "$interval" -eq 0 ] || sleep "$interval"
        continue
      fi
      ;;
    *)
      if [ "$was_queued" = 1 ]; then
        # The queue pushes main first and the PR reads MERGED a moment later, and the PR state is read before the
        # queue state: one poll of "OPEN and not queued" can be a merge in progress. Look again (the next pass
        # reads the PR first) and report DEQUEUED only on the second consecutive poll.
        dq_polls=$((dq_polls + 1))
        if [ "$dq_polls" -lt 2 ]; then
          if [ $((SECONDS - start)) -ge "$timeout" ]; then echo "TIMEOUT"; exit 2; fi
          [ "$interval" -eq 0 ] || sleep "$interval"
          continue
        fi
        echo "DEQUEUED: PR $pr left the merge queue without merging (the queue build failed, it conflicted with an entry ahead, or it was removed); read the queue build's failures on the PR page, fix, and enqueue again with hack/pr-merge.sh"
        exit 8
      fi
      ;;
    esac
    ready_dropped=0
    if [ "$mode" = checks ]; then
      if [ "$(jq -r '.isDraft // false' <<<"$json")" = true ]; then
        echo "DRAFT: not tested, mark ready for review (hack/ship.sh --ready, gh pr ready)"
        exit 5
      fi
      # A stale PR head: the push is on origin but GitHub's headRefOid still names the old commit (old issue 661).
      sb=$(jq -r '.headRefName // ""' <<<"$json")
      sh=$(jq -r '.headRefOid // ""' <<<"$json")
      rem=""
      if [ -n "$sb" ] && [ -n "$sh" ] && [ "$(jq -r '.isCrossRepository // false' <<<"$json")" != true ]; then
        rem=$(git ls-remote origin "refs/heads/$sb" 2>/dev/null | awk 'NR == 1 { print $1 }') || rem=""
      fi
      if [ -n "$rem" ] && [ "$rem" != "$sh" ]; then
        stale_since=${stale_since:-$SECONDS}
        if [ $((SECONDS - stale_since)) -ge "$stale_grace" ]; then
          echo "STALE-HEAD: PR $pr head is ${sh:0:7} but origin/$sb is ${rem:0:7}; GitHub has not picked up the push. Re-sync: gh api -X PATCH repos/{owner}/{repo}/pulls/$pr -f base=$(jq -r '.baseRefName // "main"' <<<"$json")"
          exit 6
        fi
        emit "waiting: PR head ${sh:0:7} is not origin/$sb ${rem:0:7} yet"
        if [ $((SECONDS - start)) -ge "$timeout" ]; then echo "TIMEOUT"; exit 2; fi
        [ "$interval" -eq 0 ] || sleep "$interval"
        continue
      fi
      stale_since=""
    fi
    head=$(jq -r '.headRefOid // ""' <<<"$json")
    live=""
    # Required checks are judged on every check run of the head commit, not only on the PR rollup, which
    # lists one run per name (a dispatched run that passed hides behind the skipped pull_request run).
    req_now=()
    for r in ${required[@]+"${required[@]}"}; do req_now+=("$r"); done
    if [ ${#req_now[@]} -gt 0 ] && [ -n "$head" ]; then
      extra=$(head_runs "$head")
      json=$(jq --argjson x "$extra" --argjson names "$(printf '%s\n' "${req_now[@]}" | jq -R . | jq -sc .)" \
        '.statusCheckRollup = ((.statusCheckRollup // []) + [$x[]? | select(.name | IN($names[]))])' <<<"$json")
    fi
    # One verdict per workflow run (#133, hack/lib/pr-verdict.sh): only the newest counting run of each workflow
    # on the head is read, by created_at then run_attempt. A run titled `ci (draft)` does not count once the PR is
    # ready (#218), whatever its age: no time window, and a by-hand re-run of it replays draft=true. A red or
    # cancelled newest run is never dropped, so it still fails; when the newest run has no checks yet the missing
    # required contexts make this wait for it. When gh cannot list the runs nothing is dropped (a draft run's red
    # then shows as FAILED, never as green).
    ready_dropped=0 superseded=0
    if [ -n "$head" ]; then
      wfruns=$(prv_runs "$head")
      if [ -n "$wfruns" ]; then
        is_draft=$(jq -r '.isDraft // false' <<<"$json")
        # Required contexts scope prv_empty (#399): load them before judging empty runs when we can
        # (do not set ctx_tried here; the block below still owns the fail-closed messaging).
        if [ "$ctx_tried" = 0 ]; then
          load_contexts "$(jq -r '.baseRefName // "main"' <<<"$json")"
        fi
        want_json=$(printf '%s\n' $ctxs ${required[@]+"${required[@]}"} ${req_now[@]+"${req_now[@]}"} | awk 'NF' | jq -R . | jq -sc .)
        # A completed newest run with no jobs at all (startup_failure) never gets checks: a failure, not a wait.
        # Only workflows that carry a required context (or every workflow when none are known, fail closed).
        empty=$(jq -r --argjson runs "$wfruns" --argjson draft "$is_draft" --argjson want "$want_json" \
          "$PRV_JQ"'(.statusCheckRollup // []) | prv_empty($runs; $draft; $want)[] | "\(.name)\t\(.id)\t\(.conclusion)"' <<<"$json")
        if [ -n "$empty" ]; then
          while IFS=$'\t' read -r ename eid econ; do
            echo "FAILED $ename: run $eid ended ($econ) with no jobs"
          done <<<"$empty"
          exit 1
        fi
        before=$(jq -r '(.statusCheckRollup // []) | length' <<<"$json")
        json=$(jq --argjson runs "$wfruns" --argjson draft "$is_draft" "$PRV_JQ"'.statusCheckRollup |= prv_drop($runs; $draft)' <<<"$json")
        if [ "$(jq -r '(.statusCheckRollup // []) | length' <<<"$json")" -lt "$before" ]; then
          if jq -e 'any(.[]; .display_title == "ci (draft)")' <<<"$wfruns" >/dev/null; then ready_dropped=1; else superseded=1; fi
        fi
      fi
    fi
    # Only required checks decide (AGENTS.md: advisory and third-party checks, DeepSource, CodeRabbit, Socket,
    # do not count): the base branch's required contexts, the explicit --require names. Any other failed or pending check is printed and dropped.
    # Fail closed: when the required contexts cannot be read, nothing is judged (never "every check").
    # With --merged (#201) the read is best effort: it is tried once (with its retries), and when it fails
    # the wait goes on to the merge or the close with no check judged (every check is printed as advisory);
    # the merge itself is what --merged waits for, and GitHub applies the rules there.
    if [ "$mode" = checks ] || [ "$ctx_tried" = 0 ]; then
      load_contexts "$(jq -r '.baseRefName // "main"' <<<"$json")"
      if [ "$ctx_ok" != 1 ] && [ "$mode" = checks ]; then
        echo "pr-wait: cannot read the required status checks of the base branch; not judging checks" >&2
        exit 2
      fi
      if [ "$ctx_ok" != 1 ] && [ "$ctx_tried" = 0 ]; then
        echo "pr-wait: cannot read the required status checks of the base branch; waiting for the merge without judging checks" >&2
      fi
      ctx_tried=1
    fi
    keep_names=("$ctxs" ${required[@]+"${required[@]}"} ${req_now[@]+"${req_now[@]}"})
    keep=$(printf '%s\n' "${keep_names[@]}" | jq -R . | jq -sc 'map(select(. != ""))')
    adv_json=$(jq --argjson keep "$keep" '.statusCheckRollup |= map(select((.name // .context // "?") as $n | ($keep | index($n)) == null))' <<<"$json")
    adv=$(jq -r "$norm"' | .[] | select((.done | not) or .bad) | "advisory \(.name) \(if .done then "failed" else "pending" end)"' <<<"$adv_json" | sort -u)
    json=$(jq --argjson keep "$keep" '.statusCheckRollup |= map(select((.name // .context // "?") as $n | ($keep | index($n)) != null))' <<<"$json")
    if [ -n "$adv" ] && [ "$adv" != "${last_adv:-}" ]; then
      echo "$adv"
      last_adv=$adv
    fi
    failed=$(jqn -r "$norm | .[] | select(.bad) | [.name, .cancelled, ((.url | capture(\"/runs/(?<r>[0-9]+)\")? | .r) // \"\")] | @tsv" <<<"$json")
    if [ -n "$failed" ]; then
      while IFS=$'\t' read -r name cancelled run; do
        if [ -n "$run" ]; then echo "FAILED $name (run $run)"; else echo "FAILED $name"; fi
        for r in ${req_now[@]+"${req_now[@]}"}; do
          [ "$r" != "$name" ] || echo "REQUIRED $name failed"
          if [ "$r" = "$name" ] && [ "$cancelled" = true ]; then
            echo "only cancelled runs of $name on this head; re-run the workflow's failed jobs, or push"
          fi
        done
      done <<<"$failed"
      exit 1
    fi
    if [ "$mstate" = DIRTY ]; then
      echo "conflict: merge origin/main into the branch, resolve it and push"
      exit 4
    fi
    # Required contexts of the ruleset that have no check on the head yet (a queued workflow run).
    missing_ctx=""
    if [ "$mode" = checks ]; then
      while IFS= read -r c; do
        [ -n "$c" ] || continue
        jq -e --arg c "$c" 'any((.statusCheckRollup // [])[]; (.name // .context // "") == $c)' <<<"$json" >/dev/null ||
          missing_ctx+="${missing_ctx:+,}$c"
      done <<<"$ctxs"
    fi
    total=$(jqn "$norm | length" <<<"$json")
    pending=$(jqn -r "$norm | [.[] | select(.done | not) | .name] | sort | join(\",\")" <<<"$json")
    ndone=$(jqn "$norm | [.[] | select(.done)] | length" <<<"$json")
    if [ "$total" -gt 0 ] && [ -z "$pending" ]; then
      # Every check has finished; a workflow run that has not created its jobs yet (queued) shows as a missing
      # required context, so wait for it.
      if [ -n "$missing_ctx" ] && [ "$ready_dropped" = 1 ]; then
        emit "waiting for the ready run"
        live=$missing_ctx
      elif [ -n "$missing_ctx" ] && [ "$superseded" = 1 ]; then
        emit "waiting for the newest run of each workflow (checks missing: $missing_ctx)"
        live=$missing_ctx
      elif [ -n "$missing_ctx" ]; then
        emit "checks $ndone/$total done; waiting for required checks not on the head yet: $missing_ctx"
        live=$missing_ctx
      elif [ "$mode" = checks ] && [ ${#req_now[@]} -gt 0 ]; then
        whys=()
        for name in "${req_now[@]}"; do
          why=$(jqn -r --arg n "$name" "$norm | [.[] | select(.name == \$n)] | if length == 0 then \"missing\" elif any(.[]; .skipped) then \"skipped\" else \"\" end" <<<"$json")
          [ -z "$why" ] || whys+=("$name"$'\t'"$why")
        done
        if [ ${#whys[@]} -gt 0 ]; then
          for w in "${whys[@]}"; do
            name=${w%%$'\t'*} why=${w#*$'\t'}
            echo "REQUIRED $name $why"
            if [ "$why" = skipped ]; then
              echo "only skipped runs of $name on this head"
            fi
          done
          exit 1
        fi
      fi
      if [ -z "$live" ] && [ "$mode" = checks ]; then
        echo "checks $total/$total green"
        # BLOCKED with every check green: the ruleset's required_review_thread_resolution (#187). Threads do
        # not resolve themselves, so list them and stop instead of waiting; never resolve them from here.
        if [ "$mstate" = BLOCKED ]; then
          trc=0
          threads=$("$threads_sh" "$pr" 2>&1) || trc=$?
          case $trc in
          0) ;;
          7)
            echo "$threads"
            echo "BLOCKED: PR $pr has unresolved review threads; reply and resolve each one (docs/dev/ci.md \"Merging\"), then run this again"
            exit 7
            ;;
          *)
            echo "pr-wait: PR $pr is BLOCKED and its review threads could not be read: $threads" >&2
            echo "pr-wait: cannot read review threads; not ready" >&2
            exit 2
            ;;
          esac
        fi
        exit 0
      fi
      [ -n "$live" ] || emit "checks $total/$total green"
    elif [ -n "$pending" ]; then
      emit "checks $ndone/$total pending: $pending"
    elif [ "$ready_dropped" = 1 ]; then
      emit "waiting for the ready run"
    elif [ "$superseded" = 1 ]; then
      emit "waiting for the newest run of each workflow"
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
