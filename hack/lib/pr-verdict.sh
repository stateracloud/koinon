#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The one place that decides what a pull request's checks say (#133). hack/pr-wait.sh and hack/pr-merge.sh
# both source this file; neither keeps its own ordering of check rows. Source it, do not run it.
#
# The rule (docs/dev/ci.md "One verdict per workflow run"):
#   1. Judge per WORKFLOW RUN, not per check name. For each workflow file (`path`) on the head take the
#      newest run that counts: by `created_at`, then `run_attempt`. A run titled `ci (draft)` does not
#      count once the PR is ready. The check runs of every other run of that workflow are dropped, so an
#      older green can never outvote a newer run that was cancelled, skipped or is still queued.
#   2. Inside the chosen run, a check name is judged by its newest row: a row that has not finished
#      (a live re-run attempt) outranks finished ones; otherwise the newest by start time (completion time
#      for a row that never started, which the GraphQL rollup reports with a null startedAt), then job id.
#   3. Commit statuses and check runs without a workflow run (e2e-lab, external apps) have no run to
#      choose: the newest row of that name decides.
#   4. Exception, names passed as `anyok` (quickstart: the e2e gate, old issue 547) and every check of the e2e-kind
#      workflow (`e2e result` and its jobs): any green row on the head counts, from any non-draft run, unless a newer
#      row failed. A newer cancelled or unfinished row does not hide an older green; a newer red does.
#   5. A completed newest run with no check run at all (startup_failure) is a failure (prv_empty), never a wait.
#   6. When the runs cannot be listed nothing is dropped (fail towards "not green": a draft run's red shows
#      as red, never as green).
# A verdict word per context: success skipped pending failure cancelled missing.
#
# Tests replay recorded API answers (hack/testdata/pr-verdict/) through `gh` stubs: no live GitHub.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

# The jq definitions. Rows are "rollup shaped": {name|context, status, conclusion|state, startedAt,
# completedAt, detailsUrl|targetUrl, workflowName}, as `gh pr view --json statusCheckRollup` prints them.
read -r -d '' PRV_JQ <<'JQ' || true
def prv_rid: ((.detailsUrl // .targetUrl // "") | capture("/runs/(?<r>[0-9]+)")? | .r) // null;
def prv_jobid: (((.detailsUrl // "") | capture("/job/(?<j>[0-9]+)")? | .j | tonumber) // 0);
def prv_name: (.name // .context // "?");
def prv_row: {
  name: prv_name,
  ts: (.startedAt // .completedAt // ""),
  job: prv_jobid,
  run: prv_rid,
  cancelled: ((.conclusion // "") == "CANCELLED"),
  url: (.detailsUrl // .targetUrl // ""),
  wf: (.workflowName // ""),
  done: ((.status == "COMPLETED") or ((.state // "") | IN("SUCCESS", "FAILURE", "ERROR"))),
  skipped: ((.conclusion // "") == "SKIPPED"),
  bad: ((.conclusion // .state // "") | IN("FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "ERROR"))
};
def prv_ok: .done and (.bad | not) and (.skipped | not);
# The rows of one check name: its verdict row. A cancelled or skipped row never outranks one that is queued or in
# progress (old issue 431); a success outranks a skip or cancel but not a newer failure (old issue 392, 547).
# $any is true only for the names that count any green on purpose (rule 4, e2e-kind): there an unfinished row does not hide a
# green one. Everywhere else a row that has not finished outranks every finished one, a green included (rule 2).
def prv_newest($any): sort_by([.ts, .job]) | . as $s | ($s | last) as $l
  | ([$s[] | select(prv_ok)] | last) as $g
  | ([$s[] | select(.done and .bad and (.cancelled | not))] | last) as $f
  | if ($any | not) and any($s[]; .done | not) then ([$s[] | select(.done | not)] | last)
    elif $g != null and $f != null and $f.ts > $g.ts then $f
    elif $g != null then $g
    elif ($l.done and ($l.cancelled or $l.skipped)) and any($s[]; .done | not) then ([$s[] | select(.done | not)] | last)
    else $l end;
# Rule 2 for the prv_row rows of a list: a name is collapsed within one run (statuses have no run: one group), and across
# runs for the names in $anyok and for the checks of e2e-kind, whose several runs on one head are normal. When the
# runs could not be listed, two runs of one other name both stay, so a draft run's red is never outvoted by a newer green.
def prv_collapse($anyok): group_by(.name)
  | map(if (.[0].name | IN($anyok[])) or all(.[]; .wf == "e2e-kind") then [prv_newest(true)] else (group_by(.run) | map(prv_newest(false))) end)
  | add // [];
# Rules 1, 3, 4, 5: drop the rows of runs that do not count. $runs is the array of prv_runs, or null when unreadable.
# Rows that carry no workflowName (check runs read by REST, prv_check_rows) get it from their run, so an e2e-kind row is
# recognised by its run, never by a name the source row may lack.
def prv_stamp($runs):
  map(prv_rid as $id
    | if (.workflowName // "") == "" and $id != null then
        (first($runs[] | select((.id | tostring) == $id)) // null) as $r
        | if $r == null then . else .workflowName = (if ($r.path | endswith("/e2e-kind.yml")) then "e2e-kind" else ($r.name // "") end) end
      else . end);
def prv_live($runs; $draft): [$runs[] | select($draft or (.display_title != "ci (draft)"))];
def prv_cur($live): $live | group_by(.path) | map(if (.[0].path | endswith("/e2e-kind.yml")) then (.[] | .id | tostring) else (max_by([.created_at, .run_attempt]) | .id | tostring) end);
def prv_drop($runs; $draft; $anyok):
  if $runs == null then . else
    ($runs | map(.id | tostring)) as $known
    | prv_live($runs; $draft) as $live
    | ($live | map(.id | tostring)) as $liveids
    | prv_cur($live) as $cur
    | map(prv_rid as $id | prv_name as $n
        | select($id == null or ($known | index($id)) == null or ($cur | index($id)) != null
            or (($n | IN($anyok[])) and ($liveids | index($id)) != null)))
    | prv_stamp($runs)
  end;
# The newest run of a workflow (e2e-kind has several by design and is left out) that completed and has no check run at all
# (startup_failure, a workflow file that failed to parse): nothing would ever arrive, so it is a failure, not a wait.
# Applies to the raw rows (before prv_drop). Prints {id, name, conclusion} objects; none when the runs are unreadable.
def prv_empty($runs; $draft):
  if $runs == null then [] else
    (map(prv_rid) | map(select(. != null))) as $have
    | prv_live($runs; $draft) as $live
    | prv_cur($live) as $cur
    | [$live[] | select((.id | tostring) as $i | ($cur | index($i)) != null and (.path | startswith(".github/workflows/")) and (.path | endswith("/e2e-kind.yml") | not)
        and .status == "completed" and ($have | index($i)) == null)
      | {id: (.id | tostring), name: (.name // "workflow"), conclusion: (.conclusion // "unknown")}]
  end;
def prv_word: if .done | not then "pending" elif .skipped then "skipped" elif .cancelled then "cancelled" elif .bad then "failure" else "success" end;
JQ

# prv_runs <sha>: the workflow runs of the head commit as a JSON array; prints nothing when gh cannot list them.
prv_runs() {
  local out
  out=$(gh api "repos/{owner}/{repo}/actions/runs?head_sha=$1&per_page=100" --paginate --jq '.workflow_runs[] | {
    id: .id, path: .path, name: .name, event: .event, display_title: .display_title, status: .status,
    conclusion: .conclusion, created_at: .created_at, run_attempt: (.run_attempt // 1)}' 2>/dev/null | jq -sc . 2>/dev/null) || out=""
  printf '%s' "$out"
}

# prv_check_rows <sha>: every check run of the commit as rollup-shaped rows (a JSON array). Fails when gh does.
prv_check_rows() {
  gh api "repos/{owner}/{repo}/commits/$1/check-runs?per_page=100" --paginate --jq '.check_runs[] | {
    name: .name, status: ((.status // "") | ascii_upcase), conclusion: ((.conclusion // "") | ascii_upcase),
    startedAt: (.started_at // ""), completedAt: (.completed_at // ""), detailsUrl: (.details_url // .html_url // ""), workflowName: ""}' 2>/dev/null |
    jq -sc . 2>/dev/null
}

# prv_status_rows <sha>: the commit statuses (dco-style apps, e2e-lab) as rollup-shaped rows. Fails when gh does.
prv_status_rows() {
  gh api "repos/{owner}/{repo}/commits/$1/status" --jq '.statuses[] | {
    name: .context, state: ((.state // "") | ascii_upcase), startedAt: (.created_at // ""), targetUrl: (.target_url // "")}' 2>/dev/null |
    jq -sc . 2>/dev/null
}

# prv_judge <rows json> <runs json|""> <draft true|false> <anyok names, space separated> <context>...
# One line per context: `<name>\t<success|skipped|pending|failure|cancelled|missing>\t<run id>`.
prv_judge() {
  local rows=$1 runs=${2:-null} draft=$3 want ok
  local -a anyok
  read -r -a anyok <<<"$4"
  shift 4
  want=$(printf '%s\n' "$@" | jq -R . | jq -sc 'map(select(. != ""))')
  ok=$(printf '%s\n' "${anyok[@]}" | jq -R . | jq -sc 'map(select(. != ""))')
  jq -nr --argjson rows "$rows" --argjson runs "$runs" --argjson draft "$draft" --argjson want "$want" \
    --argjson anyok "$ok" "$PRV_JQ"'
    ($rows | prv_drop($runs; $draft; $anyok) | map(prv_row) | prv_collapse($anyok)) as $r
    | (($want[] | . as $n
      | [$r[] | select(.name == $n)] as $c
      | if ($c | length) == 0 then [$n, "missing", ""] else (([$c[] | select(prv_ok | not)] | first) // $c[0]) as $x | [$n, ($x | prv_word), ($x.run // "")] end),
      ($rows | prv_empty($runs; $draft)[] | ["\(.name): run has no jobs (\(.conclusion))", "failure", .id]))
    | @tsv'
}

# prv_queue_state <PR>: `queued <position> <state>` while the PR is in the base branch's merge queue (#225), `no`
# when the answer says it is not, `unknown` when it cannot be read. Callers must not take `unknown` for "left".
# shellcheck disable=SC2016 # $owner, $repo, $number are GraphQL variables
prv_queue_query='query($owner: String!, $repo: String!, $number: Int!) { repository(owner: $owner, name: $repo) { pullRequest(number: $number) { isInMergeQueue mergeQueueEntry { position state } } } }'
prv_queue_state() {
  local raw
  raw=$(gh api graphql -F owner='{owner}' -F repo='{repo}' -F number="$1" -f query="$prv_queue_query" 2>/dev/null) || { echo unknown; return 0; }
  jq -r '.data.repository.pullRequest | if type != "object" then "unknown" elif .isInMergeQueue == true then "queued \(.mergeQueueEntry.position // "?") \(.mergeQueueEntry.state // "")" else "no" end' <<<"$raw" 2>/dev/null || echo unknown
}

# prv_merge_outcome <mode --auto|--admin> <queue state from prv_queue_state> <gh pr view json: state, mergeStateStatus,
# reviewDecision, autoMergeRequest>: an honest one-line verb for what `gh pr merge` achieved. `gh pr merge --auto` succeeds
# when GitHub accepted the request, not when the PR is queued or mergeable: a PR still blocked by a required
# review (required_reviewers) has auto-merge armed but is NOT in the merge queue and will not merge until that is met.
prv_merge_outcome() {
  local mode=$1 qs=$2 view=$3 st ms rd am
  st=$(jq -r '.state // ""' <<<"$view" 2>/dev/null) || st=""
  ms=$(jq -r '.mergeStateStatus // ""' <<<"$view" 2>/dev/null) || ms=""
  rd=$(jq -r '.reviewDecision // ""' <<<"$view" 2>/dev/null) || rd=""
  am=$(jq -r 'if (.autoMergeRequest // null) == null then "no" else "yes" end' <<<"$view" 2>/dev/null) || am=no
  if [ "$mode" = --admin ]; then
    if [ "$st" = MERGED ]; then echo "merged (--admin)"; else echo "merge requested (--admin); GitHub shows the PR as ${st:-unknown}"; fi
    return 0
  fi
  case $st in MERGED) echo "merged"; return 0 ;; esac
  case $qs in
  queued*) echo "added to the merge queue (merged by GitHub when the queue build is green)"; return 0 ;;
  esac
  if [ "$rd" = REVIEW_REQUIRED ] || [ "$rd" = CHANGES_REQUESTED ]; then
    if [ "$am" = yes ]; then
      echo "auto-merge armed but NOT queued: the PR waits for a required review ($rd); it merges only after that is met"
    else
      echo "auto-merge is NOT armed and the PR is NOT queued: a review is required ($rd); get it, then arm auto-merge again"
    fi
  elif [ "$am" = yes ]; then
    case $ms in
    BLOCKED) echo "auto-merge armed but NOT queued: GitHub reports the PR as BLOCKED by a rule that is not a failed check (a required review, or an unresolved thread)" ;;
    *) echo "auto-merge enabled (merged by GitHub when its rules are met; merge state $ms)" ;;
    esac
  else
    echo "merge requested; GitHub has not armed auto-merge or queued the PR (state ${st:-unknown}, merge state ${ms:-unknown}); check the PR page"
  fi
}
