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
#                       docs/, .claude/; more than 100 changed files always count as code). hack/paths.rules has no "code" entry: its `go` and `scripts` jobs
#                       are about CI jobs, not about code. Change DOCS_RE here when the docs rule changes.
#   last push           the earliest check suite of the head commit (created by GitHub when the push arrives),
#                       error a few seconds. Fallbacks, in this order: the last HeadRefForcePushedEvent, the
#                       commit's pushedDate, the committer date (source=commit_date: can be hours early).
#                       A draft that is marked ready after its last push still counts from the push. Limit: a head
#                       commit that was pushed before (a reset to an older pushed commit) gives a time too early.
#   merged              mergedAt of the pull request.
#   queue time          a queue merge is a PR whose last RemovedFromMergeQueueEvent has reason "merged"; its queue
#                       time is the AddedToMergeQueueEvent of that entry (the last one before the removal) to
#                       mergedAt. Anything else (never queued, or queued, dropped, then merged by hand) is an admin
#                       merge: reported apart (admin_*), never mixed into the queue numbers.
#   runner minutes      the sum of the durations of every non-skipped job (all attempts) of every workflow run
#                       of the pull request branch (event pull_request, any push) whose head_sha is one of the PR's
#                       first 100 commits (or whose pull_requests[] names it), and of the merge-queue runs of that
#                       pull request (event merge_group, branch gh-readonly-queue/*/pr-N-*).
#   ci runs             workflow runs named `ci` on the pull request (all pushes).
#   job medians         per workflow/job name, successful jobs only; the pull request `ci` runs, and the merge
#                       queue runs of every workflow. Top 10 by median.
# Percentiles use the nearest rank. Exit: 0 ok, 64 usage, 1 no data or a failed GitHub call (named).
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
  sed -n "4,41p" "$0" | sed "s/^# \{0,1\}//" >&2
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

# ---- fan-out: every background call writes <out>.part and moves it to <out> only on success with output; drain
# waits for each one and exits 1 naming every <out> (gql-<PR>, jobs-<run id>, ...) that is missing or empty. ------
declare -A PIDS=()
spawn() { # spawn <out> <command...>
  local out=$1
  shift
  pwait
  ( "$@" >"$out.part" && [ -s "$out.part" ] && mv "$out.part" "$out" ) &
  PIDS[$!]=$out
}
drain() {
  local pid out bad=""
  for pid in "${!PIDS[@]}"; do wait "$pid" || true; done
  for pid in "${!PIDS[@]}"; do
    out=${PIDS[$pid]}
    [ -s "$out" ] || bad+=" $(basename "$out")"
  done
  PIDS=()
  [ -z "$bad" ] || { echo "ci-timing: GitHub calls failed or returned nothing:$bad" >&2; exit 1; }
}

# A pull request is a code pull request when a changed file is not docs-only. files: the first 100 paths;
# changed: the pull request's changedFiles count (more files than listed = code).
# shellcheck disable=SC2016 # a jq program, not shell
JQ_CODE='def is_code($d; $files; $changed): (($files | map(select(test($d) | not)) | length) > 0) or ($changed > ($files | length));'

# ---- fetch: writes $tmp/raw.json = {repo, since, until, records:[raw record]} ------------------------------------
# Raw record: {number, mergedAt, headRefName, files:[path], changedFiles, commitDate, pushedDate, suites:[createdAt],
#   forcePushed:[createdAt], queued:[createdAt], removed:[{createdAt, reason}], runs:[{id, name, event,
#   jobs:[{name, conclusion, started_at, completed_at}]}]}
gql_task() {
  gh api graphql -f query="$QUERY" -f o="$owner" -f r="$name" -F n="$1" --jq '.data.repository.pullRequest'
}
runs_task() { # runs_task <PR>: the pull request runs of its branch that belong to its commits
  gh api -X GET --paginate "repos/$repo/actions/runs" -f event=pull_request -f branch="$(jq -r .headRefName "$tmp/gql-$1.json")" \
    -f per_page=100 --jq '.workflow_runs[]' |
    jq -s -c --slurpfile g "$tmp/gql-$1.json" '
      $g[0] as $g | ($g.allCommits.nodes | map(.commit.oid)) as $oids
      | [.[] | select(.conclusion != "skipped")
         | select(((.head_sha as $s | $oids | index($s)) != null)
                  or any(.pull_requests[]?; .number == $g.number)
                  or ($g.allCommits.totalCount > 100 and .created_at >= $g.createdAt and .created_at <= $g.mergedAt))
         | {id, name, event}]'
}
mg_task() { # mg_task <from> <to>: the merge-queue runs created in the window
  gh api -X GET --paginate "repos/$repo/actions/runs" -f event=merge_group -f per_page=100 -f created="$1..$2" \
    --jq '.workflow_runs[] | {id, name, event, head_branch}' | jq -s -c '.'
}
jobs_task() {
  gh api -X GET --paginate "repos/$repo/actions/runs/$1/jobs" -f per_page=100 -f filter=all --jq '.jobs[]' |
    jq -s -c '[.[] | select(.conclusion != "skipped") | {name, conclusion, started_at, completed_at}]'
}

fetch() {
  repo=${REPO:-}
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
  QUERY='query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){
    number mergedAt createdAt headRefName changedFiles
    files(first:100){nodes{path}}
    timelineItems(last:100,itemTypes:[HEAD_REF_FORCE_PUSHED_EVENT,ADDED_TO_MERGE_QUEUE_EVENT,REMOVED_FROM_MERGE_QUEUE_EVENT]){nodes{__typename
      ... on HeadRefForcePushedEvent{createdAt} ... on AddedToMergeQueueEvent{createdAt}
      ... on RemovedFromMergeQueueEvent{createdAt reason}}}
    headCommit: commits(last:1){nodes{commit{committedDate pushedDate checkSuites(first:30){nodes{createdAt}}}}}
    allCommits: commits(first:100){totalCount nodes{commit{oid}}}}}}'
  local num
  for num in $(jq -r '.[].number' "$tmp/cands.json"); do spawn "$tmp/gql-$num.json" gql_task "$num"; done
  drain
  for num in $(jq -r '.[].number' "$tmp/cands.json"); do
    jq -e '.number != null' "$tmp/gql-$num.json" >/dev/null || { echo "ci-timing: no pull request data for #$num (gql-$num.json)" >&2; exit 1; }
  done

  # Select the first N code pull requests (newest first).
  local sel="" count=0
  for num in $(jq -r '.[].number' "$tmp/cands.json"); do
    [ "$count" -lt "$n" ] || break
    if jq -e --arg d "$DOCS_RE" "$JQ_CODE"' is_code($d; [.files.nodes[].path]; .changedFiles)' "$tmp/gql-$num.json" >/dev/null; then
      sel+="$num "
      count=$((count + 1))
    fi
  done
  [ -n "$sel" ] || { echo "ci-timing: no merged code pull request found" >&2; exit 1; }
  [ "$count" -ge "$n" ] || echo "ci-timing: only $count code pull requests in range (wanted $n)" >&2

  # Workflow runs: the pull request runs per branch (kept when the head_sha is one of the pull request's commits,
  # or pull_requests[] names it; GitHub empties that list once the branch is deleted), and the merge-queue runs
  # created between a day before the oldest and the latest merge.
  for num in $sel; do spawn "$tmp/prruns-$num.json" runs_task "$num"; done
  local -a gfiles=()
  for num in $sel; do gfiles+=("$tmp/gql-$num.json"); done
  local from_t to_t
  from_t=$(jq -s -r '[.[].mergedAt] | min | fromdateiso8601 - 86400 | todate' "${gfiles[@]}")
  to_t=$(jq -s -r '[.[].mergedAt] | max' "${gfiles[@]}")
  spawn "$tmp/mgruns.json" mg_task "$from_t" "$to_t"
  drain

  # Jobs of every run, deduplicated.
  local ids id
  ids=$(
    {
      for num in $sel; do jq -r '.[].id' "$tmp/prruns-$num.json"; done
      for num in $sel; do jq -r --arg p "/pr-$num-" '.[] | select(.head_branch | contains($p)) | .id' "$tmp/mgruns.json"; done
    } | sort -u
  )
  for id in $ids; do spawn "$tmp/jobs-$id.json" jobs_task "$id"; done
  drain

  # Assemble.
  for num in $sel; do
    jq -c -n --slurpfile g "$tmp/gql-$num.json" --slurpfile p "$tmp/prruns-$num.json" --slurpfile m "$tmp/mgruns.json" \
      --arg pat "/pr-$num-" '
      $g[0] as $g
      | ($g.headCommit.nodes[0].commit) as $c
      | {number: $g.number, mergedAt: $g.mergedAt, headRefName: $g.headRefName,
         files: [$g.files.nodes[].path], changedFiles: $g.changedFiles,
         commitDate: $c.committedDate, pushedDate: $c.pushedDate,
         suites: [$c.checkSuites.nodes[].createdAt],
         forcePushed: [$g.timelineItems.nodes[] | select(.__typename == "HeadRefForcePushedEvent") | .createdAt],
         queued: [$g.timelineItems.nodes[] | select(.__typename == "AddedToMergeQueueEvent") | .createdAt],
         removed: [$g.timelineItems.nodes[] | select(.__typename == "RemovedFromMergeQueueEvent") | {createdAt, reason}],
         runs: ($p[0] + [$m[0][] | select(.head_branch | contains($pat)) | {id, name, event}])}' >"$tmp/rec-$num.json"
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
    | is_code($docs; ($r.files // []); ($r.changedFiles // 0)) as $code
    | (if (($r.suites // []) | length) > 0 then {t: ($r.suites | min), s: "suite"}
       elif (($r.forcePushed // []) | length) > 0 then {t: ($r.forcePushed | max), s: "force_push"}
       elif $r.pushedDate != null then {t: $r.pushedDate, s: "pushed_date"}
       else {t: $r.commitDate, s: "commit_date"} end) as $p
    | (($r.removed // []) | sort_by(.createdAt) | last) as $rm
    | (if $rm != null and $rm.reason == "merged"
       then ([($r.queued // [])[] | select(. <= $rm.createdAt)] | max) else null end) as $q
    | ($r.runs // []) as $runs
    | {number: $r.number, code: $code, merged_at: $r.mergedAt, push_at: $p.t, push_source: $p.s,
       admin: ($q == null),
       push_to_merged_min: ((($r.mergedAt | ts) - ($p.t | ts)) / 60),
       queue_min: (if $q == null then null else ((($r.mergedAt | ts) - ($q | ts)) / 60) end),
       runs: ($runs | length),
       ci_runs: ([$runs[] | select(.name == "ci" and .event == "pull_request")] | length),
       runner_min: (([$runs[].jobs[]? | jobdur] | add // 0) / 60),
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
  runner_min_per_pr: ($all | map(.runner_min) | stats),
  ci_runs_per_pr_median: ($all | map(.ci_runs) | med),
  runs_per_pr_median: ($all | map(.runs) | med),
  job_median_pr_ci: jobtable("pull_request"),
  job_median_merge_group: jobtable("merge_group"),
  prs: ($all | map(del(.jobs) | .push_to_merged_min |= r1 | .queue_min |= r1 | .runner_min |= r1))
}
JQ

result=$(jq -c --arg docs "$DOCS_RE" "$JQ_CODE $ANALYSE" "$tmp/raw.json")
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
  kv("runner_min_per_pr"; .runner_min_per_pr),
  "ci_runs_per_pr_median=\(.ci_runs_per_pr_median)",
  "runs_per_pr_median=\(.runs_per_pr_median)"' <<<"$result"
# Aligns tab-separated rows into columns, like `column -t -s TAB` (which not every image ships).
align() {
  awk -F '\t' '{ for (i = 1; i <= NF; i++) { c[NR, i] = $i; if (length($i) > w[i]) w[i] = length($i) }
                  if (NF > nf) nf = NF; n = NR }
    END { for (r = 1; r <= n; r++) { line = ""
            for (i = 1; i <= nf; i++) line = line (i < nf ? sprintf("%-" w[i] "s  ", c[r, i]) : c[r, i])
            sub(/ +$/, "", line); print line } }'
}
echo
echo "job medians, pull request ci runs (top 10)"
{
  printf 'job\tn\tmedian_min\n'
  jq -r '.job_median_pr_ci[] | [.job, .n, .median_min] | @tsv' <<<"$result"
} | align
echo
echo "job medians, merge queue runs (top 10)"
{
  printf 'job\tn\tmedian_min\n'
  jq -r '.job_median_merge_group[] | [.job, .n, .median_min] | @tsv' <<<"$result"
} | align
echo
echo "pull requests"
{
  printf 'pr\tmerge\tpush->merged\tqueue\trunner_min\truns\tci_runs\tpush_source\n'
  jq -r '.prs[] | [.number, (if .admin then "admin" else "queue" end), .push_to_merged_min, (.queue_min // "-"), .runner_min, .runs, .ci_runs, .push_source] | @tsv' <<<"$result"
} | align
