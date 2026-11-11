#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Measure the target of issue 572: the time from the LAST PUSH of a pull request to MERGED, over the last N
# merged code pull requests, and where that time goes (queue time, time per CI job, runner minutes). Read-only
# against GitHub (gh api GET calls only), no pager, nothing posted.
#
# Usage: hack/ci-timing.sh [--n 20] [--since ISO] [--until ISO] [--json] [--from FILE] [help]
#   --n N        count the last N merged code pull requests (default 20)
#   --since ISO  only pull requests merged after this time (2026-11-11T23:00:00Z); to compare before and after
#   --until ISO  only pull requests merged before this time
#   --json       machine output (one JSON object) instead of key=value lines and tables
#   --from FILE  read recorded raw records (the hack/testdata/ci-timing/ format) instead of calling GitHub
#   REPO=owner/name overrides the repository (default: gh repo view)
#
# Definitions (one place each):
#   code pull request   changed at least one file that does not match DOCS_RE below (docs only: Markdown,
#                       docs/, .claude/). hack/paths.rules has no "code" entry: its `go` and `scripts` jobs
#                       are about CI jobs, not about code. Change DOCS_RE here when the docs rule changes.
#   last push           the earliest check suite of the head commit (created by GitHub when the push arrives),
#                       error a few seconds. Fallbacks, in this order: the last HeadRefForcePushedEvent, the
#                       commit's pushedDate, the committer date (source=commit_date: can be hours early).
#                       A draft that is marked ready after its last push still counts from the push.
#   merged              mergedAt of the pull request.
#   queue time          the last AddedToMergeQueueEvent to mergedAt. No such event = admin merge: reported
#                       apart (admin_*), never mixed into the queue numbers.
#   runner minutes      the sum of the durations of every non-skipped job (all attempts) of every workflow run
#                       of the pull request branch (event pull_request, any push, created while the PR was open) and of the merge-queue
#                       runs of that pull request (event merge_group, branch gh-readonly-queue/*/pr-N-*).
#   ci runs             workflow runs named `ci` on the pull request (all pushes).
#   job medians         per workflow/job name, successful jobs only; the pull request `ci` runs, and the merge
#                       queue runs of every workflow. Top 10 by median.
# Percentiles use the nearest rank. Exit: 0 ok, 64 usage, 1 no data.
#
# Cost: 1 search, one GraphQL call per candidate (about 2N+10), per selected pull request 1 runs call, one
# merge-queue runs listing, and one jobs call per run; all in parallel (6 at a time). N=20: about 1 minute.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat

# A file is docs-only when it matches this (POSIX ERE, jq test()).
DOCS_RE='(\.md$|^docs/|^\.claude/)'
PAR=6

usage() {
  sed -n "4,36p" "$0" | sed "s/^# \{0,1\}//" >&2
  exit "${1:-64}"
}

n=20 since="" until="" json=0 from=""
while [ $# -gt 0 ]; do
  case $1 in
  help | -h | --help) usage 0 ;;
  --n)
    [ $# -ge 2 ] || usage
    n=$2
    shift
    ;;
  --since)
    [ $# -ge 2 ] || usage
    since=$2
    shift
    ;;
  --until)
    [ $# -ge 2 ] || usage
    until=$2
    shift
    ;;
  --from)
    [ $# -ge 2 ] || usage
    from=$2
    shift
    ;;
  --json) json=1 ;;
  *) usage ;;
  esac
  shift
done
case $n in '' | *[!0-9]* | 0) usage ;; esac

tmp=$(mktemp -d "${TMPDIR:-/tmp}/ci-timing.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

pwait() { while [ "$(jobs -rp | wc -l)" -ge "$PAR" ]; do wait -n || true; done; }

# ---- fetch: writes $tmp/raw.json = {repo, since, until, records:[raw record]} ------------------------------------
# Raw record: {number, mergedAt, headRefName, files:[path], commitDate, pushedDate, suites:[createdAt],
#   forcePushed:[createdAt], queued:[createdAt], runs:[{id, name, event, jobs:[{name, conclusion, started_at,
#   completed_at}]}]}
fetch() {
  local repo=${REPO:-} owner name
  [ -n "$repo" ] || repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
  owner=${repo%%/*} name=${repo#*/}

  local range="merged:"
  if [ -n "$since" ] && [ -n "$until" ]; then range+="$since..$until"
  elif [ -n "$since" ]; then range+=">=$since"
  elif [ -n "$until" ]; then range+="<$until"
  else range="sort:updated-desc"; fi
  local cand=$((2 * n + 10))
  gh pr list -R "$repo" --state merged --base main --search "$range" --limit 300 --json number,mergedAt,headRefName |
    jq -c --argjson c "$cand" --arg s "$since" --arg u "$until" \
      '[.[] | select(($s == "" or .mergedAt > $s) and ($u == "" or .mergedAt < $u))] | sort_by(.mergedAt) | reverse | .[:$c]' \
      >"$tmp/cands.json"

  # shellcheck disable=SC2016 # a GraphQL query, not shell
  local query='query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){
    number mergedAt createdAt headRefName
    files(first:100){nodes{path}}
    timelineItems(last:60,itemTypes:[HEAD_REF_FORCE_PUSHED_EVENT,ADDED_TO_MERGE_QUEUE_EVENT]){nodes{__typename
      ... on HeadRefForcePushedEvent{createdAt} ... on AddedToMergeQueueEvent{createdAt}}}
    commits(last:1){nodes{commit{committedDate pushedDate checkSuites(first:30){nodes{createdAt}}}}}}}}'
  local num
  for num in $(jq -r '.[].number' "$tmp/cands.json"); do
    pwait
    gh api graphql -f query="$query" -f o="$owner" -f r="$name" -F n="$num" \
      --jq '.data.repository.pullRequest' >"$tmp/gql-$num.json" &
  done
  wait

  # Select the first N code pull requests (newest first).
  local sel=""
  local count=0
  for num in $(jq -r '.[].number' "$tmp/cands.json"); do
    [ "$count" -lt "$n" ] || break
    if jq -e --arg d "$DOCS_RE" '[.files.nodes[].path | select(test($d) | not)] | length > 0' "$tmp/gql-$num.json" >/dev/null; then
      sel+="$num "
      count=$((count + 1))
    fi
  done
  [ -n "$sel" ] || { echo "ci-timing: no merged code pull request found" >&2; exit 1; }
  [ "$count" -ge "$n" ] || echo "ci-timing: only $count code pull requests in range (wanted $n)" >&2

  # Workflow runs: the pull request runs per branch (GitHub empties pull_requests[] once the branch is deleted, so
  # the runs of the branch created while the pull request was open count), the merge-queue runs in one window.
  for num in $sel; do
    pwait
    (
      br=$(jq -r '.headRefName' "$tmp/gql-$num.json")
      gh api -X GET --paginate "repos/$repo/actions/runs" -f event=pull_request -f branch="$br" -f per_page=100 \
        --jq '.workflow_runs[]' |
        jq -s -c --arg a "$(jq -r .createdAt "$tmp/gql-$num.json")" --arg b "$(jq -r .mergedAt "$tmp/gql-$num.json")" \
          '[.[] | select(.created_at >= $a and .created_at <= $b) | select(.conclusion != "skipped") | {id, name, event}]' \
          >"$tmp/prruns-$num.json"
    ) &
  done
  wait
  local minm
  minm=$(jq -r --arg s "$sel" '[.[] | select(.number as $x | ($s | split(" ") | index($x | tostring)))] | map(.mergedAt) | min' "$tmp/cands.json")
  gh api -X GET --paginate "repos/$repo/actions/runs" -f event=merge_group -f per_page=100 \
    -f created=">=$(date -u -d "$minm - 1 day" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -j -v-1d -f %Y-%m-%dT%H:%M:%SZ "$minm" +%Y-%m-%dT%H:%M:%SZ)" \
    --jq '.workflow_runs[] | {id, name, event, head_branch, conclusion}' | jq -s -c '.' >"$tmp/mgruns.json"

  # Jobs of every run, deduplicated.
  local ids id
  ids=$(
    {
      for num in $sel; do jq -r '.[].id' "$tmp/prruns-$num.json"; done
      for num in $sel; do jq -r --arg p "/pr-$num-" '.[] | select(.head_branch | contains($p)) | .id' "$tmp/mgruns.json"; done
    } | sort -u
  )
  for id in $ids; do
    pwait
    gh api -X GET "repos/$repo/actions/runs/$id/jobs" -f per_page=100 -f filter=all \
      --jq '[.jobs[] | select(.conclusion != "skipped") | {name, conclusion, started_at, completed_at}]' >"$tmp/jobs-$id.json" &
  done
  wait

  # Assemble.
  for num in $sel; do
    jq -c -n --slurpfile g "$tmp/gql-$num.json" --slurpfile p "$tmp/prruns-$num.json" --slurpfile m "$tmp/mgruns.json" \
      --arg pat "/pr-$num-" '
      $g[0] as $g
      | ($g.commits.nodes[0].commit) as $c
      | {number: $g.number, mergedAt: $g.mergedAt, headRefName: $g.headRefName,
         files: [$g.files.nodes[].path], commitDate: $c.committedDate, pushedDate: $c.pushedDate,
         suites: [$c.checkSuites.nodes[].createdAt],
         forcePushed: [$g.timelineItems.nodes[] | select(.__typename == "HeadRefForcePushedEvent") | .createdAt],
         queued: [$g.timelineItems.nodes[] | select(.__typename == "AddedToMergeQueueEvent") | .createdAt],
         runs: ($p[0] + [$m[0][] | select(.head_branch | contains($pat)) | {id, name, event}])}' >"$tmp/rec-$num.json"
    # jobs per run
    jq -c '.runs[].id' "$tmp/rec-$num.json" | while read -r id; do
      jq -c --argjson id "$id" -n --slurpfile j "$tmp/jobs-$id.json" '{key: ($id | tostring), value: $j[0]}'
    done | jq -s -c 'from_entries' >"$tmp/jmap-$num.json"
    jq -c --slurpfile j "$tmp/jmap-$num.json" '.runs |= map(. + {jobs: ($j[0][.id | tostring] // [])})' "$tmp/rec-$num.json" >"$tmp/recj-$num.json"
  done
  local -a recs=()
  for num in $sel; do recs+=("$tmp/recj-$num.json"); done
  jq -s -c --arg repo "$repo" --arg s "$since" --arg u "$until" '{repo: $repo, since: $s, until: $u, records: .}' \
    "${recs[@]}" >"$tmp/raw.json"
}

if [ -n "$from" ]; then
  [ -r "$from" ] || { echo "ci-timing: cannot read $from" >&2; exit 64; }
  cp "$from" "$tmp/raw.json"
else
  fetch
fi

# ---- analyse ------------------------------------------------------------------------------------------------------
read -r -d '' ANALYSE <<'JQ' || true
def ts: fromdateiso8601;
def med: sort as $s | ($s | length) as $k
  | if $k == 0 then null elif $k % 2 == 1 then $s[($k - 1) / 2] else ($s[$k / 2 - 1] + $s[$k / 2]) / 2 end;
def pct($p): sort as $s | ($s | length) as $k
  | if $k == 0 then null else $s[([(($p * $k) | ceil) - 1, 0] | max)] end;
def r1: if . == null then null else (. * 10 | round) / 10 end;
def stats: if length == 0 then {n: 0, median: null, p75: null, worst: null}
  else {n: length, median: (med | r1), p75: (pct(0.75) | r1), worst: (max | r1)} end;
def jobdur: select(.started_at != null and .completed_at != null) | ((.completed_at | ts) - (.started_at | ts));

(.records | map(select(.mergedAt != null))
  | map(. as $r
    | (($r.files // []) | map(select(test($docs) | not)) | length > 0) as $code
    | (if (($r.suites // []) | length) > 0 then {t: ($r.suites | min), s: "suite"}
       elif (($r.forcePushed // []) | length) > 0 then {t: ($r.forcePushed | max), s: "force_push"}
       elif $r.pushedDate != null then {t: $r.pushedDate, s: "pushed_date"}
       else {t: $r.commitDate, s: "commit_date"} end) as $p
    | ([($r.queued // [])[] | select(. <= $r.mergedAt)] | max) as $q
    | ($r.runs // []) as $runs
    | {number: $r.number, code: $code, merged_at: $r.mergedAt, push_at: $p.t, push_source: $p.s,
       admin: ($q == null),
       push_to_merged_min: ((($r.mergedAt | ts) - ($p.t | ts)) / 60),
       queue_min: (if $q == null then null else ((($r.mergedAt | ts) - ($q | ts)) / 60) end),
       runs: ($runs | length),
       ci_runs: ([$runs[] | select(.name == "ci" and .event == "pull_request")] | length),
       runner_min: ([$runs[].jobs[]? | jobdur] | add // 0) / 60,
       jobs: [$runs[] | . as $run | .jobs[]? | select(.conclusion == "success")
              | {wf: $run.name, event: $run.event, name: .name, sec: jobdur}]})
  | map(select(.code))) as $all
| ($all | map(select(.admin | not))) as $qd
| ($all | map(select(.admin))) as $ad
| def jobtable($ev):
    [$all[].jobs[] | select(.event == $ev and (.wf == "ci" or $ev == "merge_group"))]
    | group_by(.wf + "/" + .name)
    | map({job: (.[0].wf + "/" + .[0].name), n: length, median_min: (([.[].sec] | med) / 60 | r1)})
    | sort_by(-.median_min) | .[:10];
{ repo: .repo, since: .since, until: .until,
  code_prs: ($all | length), queue_prs: ($qd | length), admin_prs: ($ad | length),
  push_source: ($all | group_by(.push_source) | map({key: .[0].push_source, value: length}) | from_entries),
  push_to_merged_min: ($qd | map(.push_to_merged_min) | stats),
  queue_min: ($qd | map(.queue_min) | stats),
  admin_push_to_merged_min: ($ad | map(.push_to_merged_min) | stats),
  runner_min_per_pr: ($all | map(.runner_min) | {median: (med | r1), worst: (max | r1)}),
  ci_runs_per_pr_median: ($all | map(.ci_runs) | med),
  runs_per_pr_median: ($all | map(.runs) | med),
  job_median_pr_ci: jobtable("pull_request"),
  job_median_merge_group: jobtable("merge_group"),
  prs: ($all | map(del(.jobs) | .push_to_merged_min |= r1 | .queue_min |= r1 | .runner_min |= r1))
}
JQ

result=$(jq -c --arg docs "$DOCS_RE" "$ANALYSE" "$tmp/raw.json")
if [ "$(jq '.code_prs' <<<"$result")" -eq 0 ]; then
  echo "ci-timing: no code pull request in the input" >&2
  exit 1
fi
if [ "$json" -eq 1 ]; then
  jq . <<<"$result"
  exit 0
fi

jq -r '
  def kv($p; $o): $o | to_entries[] | "\($p)_\(.key)=\(.value)";
  "repo=\(.repo)", "since=\(.since)", "until=\(.until)",
  "code_prs=\(.code_prs)", "queue_prs=\(.queue_prs)", "admin_prs=\(.admin_prs)",
  "push_source=\(.push_source | to_entries | map("\(.key):\(.value)") | join(","))",
  kv("push_to_merged_min"; .push_to_merged_min),
  kv("queue_min"; .queue_min),
  kv("admin_push_to_merged_min"; .admin_push_to_merged_min),
  "runner_min_per_pr_median=\(.runner_min_per_pr.median)",
  "runner_min_per_pr_worst=\(.runner_min_per_pr.worst)",
  "ci_runs_per_pr_median=\(.ci_runs_per_pr_median)",
  "runs_per_pr_median=\(.runs_per_pr_median)"' <<<"$result"
tab=$(printf '\t')
echo
echo "job medians, pull request ci runs (top 10)"
{
  printf 'job\tn\tmedian_min\n'
  jq -r '.job_median_pr_ci[] | [.job, .n, .median_min] | @tsv' <<<"$result"
} | column -t -s "$tab"
echo
echo "job medians, merge queue runs (top 10)"
{
  printf 'job\tn\tmedian_min\n'
  jq -r '.job_median_merge_group[] | [.job, .n, .median_min] | @tsv' <<<"$result"
} | column -t -s "$tab"
echo
echo "pull requests"
{
  printf 'pr\tmerge\tpush->merged\tqueue\trunner_min\truns\tci_runs\tpush_source\n'
  jq -r '.prs[] | [.number, (if .admin then "admin" else "queue" end), .push_to_merged_min, (.queue_min // "-"), .runner_min, .runs, .ci_runs, .push_source] | @tsv' <<<"$result"
} | column -t -s "$tab"
