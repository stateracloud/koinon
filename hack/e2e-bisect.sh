#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Maintainer script (hack/README.md): runs in the upstream CI (opens an issue with the workflow's token).
#
# The kind e2e failed on main. Find which merge broke it and record it in one issue.
#
#   hack/e2e-bisect.sh [--dry-run] [--green SHA] [--head SHA] [--results SHA=pass|fail|unknown,...]
#                      [--prev success|failure] [--open-issue N]
#
# Only on a green-to-red transition: if the previous completed push/schedule run on main also
# failed, it stops (main is already red, an issue exists or someone is on it). If an open
# `e2e-regression` issue exists it comments there and stops; it never dispatches twice.
#
# 1. The merges (first-parent commits) since the last green run on main (--green; by default asked
#    of GitHub), up to --head. Without a green baseline it reports that and tests HEAD alone.
# 2. Candidates: those whose changed files hack/needs-e2e.sh --stdin says need the e2e. At most
#    MAX_CANDIDATES (5), the newest; older ones are listed as untested.
# 3. Opens the issue first, listing the candidates, so a timeout still leaves a record.
# 4. Dispatches the e2e-kind workflow on every candidate (inputs `commit`, `bisect_id`, `backup_restore`, and `stories` only when STORIES_RED=1; this
#    run's id, which the dispatched run's name carries), in parallel, and polls each once a minute
#    (never `gh run watch`: its polling spends the 1000 requests/hour token budget). An outcome is
#    pass, fail, or unknown (cancelled, timed out, not found).
# 5. Updates the issue. Only a `fail` can name a culprit; with one candidate that is a confirming
#    re-run of exactly it, and anything but fail leaves it "suspected: re-run before reverting".
#
# --dry-run calls no GitHub API: it prints what it would do and takes outcomes from --results
# (default pass). Environment: GH (the gh command), GH_REPO, GITHUB_RUN_ID, GITHUB_RUN_URL.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

MAX_CANDIDATES=${MAX_CANDIDATES:-5}
POLL_SECONDS=${POLL_SECONDS:-60}
CANDIDATE_TIMEOUT=${CANDIDATE_TIMEOUT:-5100} # 85 min
# STORIES_RED=1 (the bisect job sets it when the story suite was red and the quickstart passed): the baseline is
# the newest nightly whose stories passed, and the candidates run with stories=true.
STORIES_RED=${STORIES_RED:-0}
dry=0 green="" head=HEAD results="" prev="" open_issue=""
while [[ $# -gt 0 ]]; do
  case $1 in
  --dry-run) dry=1 ;;
  --green) green=$2; shift ;;
  --head) head=$2; shift ;;
  --results) results=$2; shift ;;
  --prev) prev=$2; shift ;;
  --open-issue) open_issue=$2; shift ;;
  *) echo "e2e-bisect: unknown argument $1" >&2; exit 2 ;;
  esac
  shift
done
GH=${GH:-gh}
RUN_ID=${GITHUB_RUN_ID:-0}
cd "$(git rev-parse --show-toplevel)"
head=$(git rev-parse "$head")

# The newest completed (success or failure) push/schedule runs on main, newest first, without this one.
if [[ $dry == 0 ]]; then
  runs=$("$GH" run list --workflow e2e-kind.yml --branch main --limit 50 --json databaseId,headSha,event,conclusion \
    --jq "[.[] | select((.event == \"push\" or .event == \"schedule\") and (.conclusion == \"success\" or .conclusion == \"failure\") and .databaseId != $RUN_ID)]")
  [[ -n $prev ]] || prev=$(jq -r '.[0].conclusion // ""' <<<"$runs")
  if [[ -n $green ]]; then
    :
  elif [[ ${STORIES_RED:-0} == 1 ]]; then
    # Only a nightly runs the stories (a push never does) and an advisory red stories pass still concludes
    # success, so the baseline is the newest schedule run that left the artifact `stories-green` (run.sh writes
    # the marker when the stories passed; the workflow uploads it).
    for id in $(jq -r '[.[] | select(.event == "schedule" and .conclusion == "success")][].databaseId' <<<"$runs"); do
      # An API error counts as "no stories-green artifact": the search moves to an older run, it does not abort.
      n=$("$GH" api "repos/$GH_REPO/actions/runs/$id/artifacts" --jq '[.artifacts[] | select(.name == "stories-green" and (.expired | not))] | length') || {
        echo "e2e-bisect: warning: cannot list the artifacts of run $id; no stories-green artifact for this run" >&2
        n=0
      }
      if [[ $n -gt 0 ]]; then
        green=$(jq -r ".[] | select(.databaseId == $id) | .headSha" <<<"$runs")
        break
      fi
    done
  else
    green=$(jq -r '[.[] | select(.conclusion == "success")][0].headSha // ""' <<<"$runs")
  fi
  [[ -n $open_issue ]] || open_issue=$("$GH" issue list --label e2e-regression --state open --limit 1 --json number --jq '.[0].number // empty')
fi

if [[ $prev == failure ]]; then
  echo "main was already red at the previous completed run: not a new regression, not bisecting."
  exit 0
fi
if [[ -n $open_issue ]]; then
  msg="main is red again at ${head:0:12}${GITHUB_RUN_URL:+ ([run]($GITHUB_RUN_URL))}. Not bisecting again while this issue is open."
  if [[ $dry == 1 ]]; then echo "would comment on #$open_issue: $msg"; else "$GH" issue comment "$open_issue" --body "$msg"; fi
  exit 0
fi

commit_title() { # a merge commit's subject is "Merge pull request #N from ...": use the PR title in its body
  local s
  s=$(git log -1 --format=%s "$1")
  if [[ $s == "Merge pull request"* ]]; then
    s=$(git log -1 --format=%b "$1" | sed -n '/./{p;q;}')
  fi
  echo "$s"
}

baseline_note=""
candidates=() untested=()
if [[ -z $green ]]; then
  baseline_note="There is no green baseline (no earlier green run on main), so only HEAD was re-run."
  candidates=("$head")
else
  merges=$(git log --first-parent --reverse --format=%H "$green..$head")
  relevant=()
  while IFS= read -r c; do
    [[ -n $c ]] || continue
    # Exit 1 is "not needed"; 0 is needed and anything else (2: the rule could not be evaluated) is
    # treated as needed too, as e2e-decide.sh does: a broken rule must not drop a candidate.
    rc=0
    git diff --name-only --no-renames "$c^1" "$c" 2>/dev/null | hack/needs-e2e.sh --stdin >/dev/null || rc=$?
    if [[ $rc -ne 1 ]]; then
      relevant+=("$c")
    fi
  done <<<"$merges"
  n=${#relevant[@]}
  if ((n > MAX_CANDIDATES)); then
    untested=("${relevant[@]:0:n-MAX_CANDIDATES}")
    candidates=("${relevant[@]:n-MAX_CANDIDATES}")
  elif ((n > 0)); then
    candidates=("${relevant[@]}")
  fi
fi
echo "baseline ${green:-none}; e2e-relevant candidates: ${#candidates[@]}; untested older: ${#untested[@]}"

list() { # a bullet per commit
  local c
  for c in "$@"; do echo "- ${c:0:9} $(commit_title "$c") ($(git log -1 --format=%an "$c"))"; done
}

policy='**Policy: fix forward within 2 hours of this issue opening, otherwise revert the merge.** Release candidate tags are only cut from a commit with a green kind e2e run.'
run_link=""
[[ -z ${GITHUB_RUN_URL:-} ]] || run_link=" ([failed run](${GITHUB_RUN_URL}))"

# render <status paragraph>: the issue body.
render() {
  local b="The kind e2e failed on main at ${head:0:12}${run_link}."$'\n\n'"$1"$'\n\n'
  if ((${#candidates[@]} > 0)); then
    b+="Candidates tested (oldest first):"$'\n'"$(list "${candidates[@]}")"$'\n\n'
  fi
  if ((${#untested[@]} > 0)); then
    b+="Older e2e-relevant merges, not tested (more than $MAX_CANDIDATES):"$'\n'"$(list "${untested[@]}")"$'\n\n'
  fi
  [[ -z $baseline_note ]] || b+="$baseline_note"$'\n\n'
  echo "$b$policy"
}

issue=""
title="e2e regression on main (${head:0:9})"
body=$(render "Bisecting: the e2e is being re-run on each candidate. This issue is updated with the result.")
if [[ $dry == 1 ]]; then
  echo "--- issue opened (label e2e-regression) ---"
  echo "$title"
  echo "$body"
else
  "$GH" label create e2e-regression --color B60205 --description "The kind e2e is red on main" >/dev/null 2>&1 || true
  issue=$("$GH" issue create --title "$title" --body "$body" --label e2e-regression)
fi

# outcome <sha>: pass|fail|unknown for the run dispatched on it.
outcome() {
  local sha=$1 r
  if [[ $dry == 1 ]]; then
    for r in ${results//,/ }; do
      if [[ $r == "${sha:0:7}"*=* ]]; then echo "${r#*=}"; return; fi
    done
    echo pass
    return
  fi
  local id="" waited=0 st
  # The dispatched run's name carries the commit and this run's id.
  while ((waited < CANDIDATE_TIMEOUT)); do
    id=$("$GH" run list --workflow e2e-kind.yml --event workflow_dispatch --limit 30 --json databaseId,displayTitle \
      --jq ".[] | select(.displayTitle | contains(\"$sha\") and contains(\"bisect $RUN_ID\")) | .databaseId" | head -1)
    [[ -n $id ]] && break
    sleep "$POLL_SECONDS"
    waited=$((waited + POLL_SECONDS))
  done
  [[ -n $id ]] || { echo unknown; return; }
  while ((waited < CANDIDATE_TIMEOUT)); do
    st=$("$GH" run view "$id" --json status,conclusion --jq '.status + " " + .conclusion')
    case $st in
    "completed success") echo pass; return ;;
    "completed failure") echo fail; return ;;
    "completed "*) echo unknown; return ;;
    esac
    sleep "$POLL_SECONDS"
    waited=$((waited + POLL_SECONDS))
  done
  echo unknown
}

status="" final_title="$title"
if ((${#candidates[@]} == 0)); then
  status="No merge since the last green run changed anything the e2e depends on: suspect a flake or the environment (runner, an upstream image or a download). Re-run the e2e on main."
else
  wd=$(mktemp -d "${TMPDIR:-/tmp}/e2e-bisect.XXXXXX")
  stories_args=()
  [[ $STORIES_RED != 1 ]] || stories_args=(-f stories=true)
  for c in "${candidates[@]}"; do
    if [[ $dry == 1 ]]; then
      echo "would dispatch: gh workflow run e2e-kind.yml -f commit=$c -f bisect_id=$RUN_ID -f backup_restore=true${stories_args[*]:+ ${stories_args[*]}}"
    else
      # The bisect runs only for push and schedule, so the backup-restore step (#90) is always in
      # scope: without it a nightly that went red there could never reproduce on a candidate.
      "$GH" workflow run e2e-kind.yml --ref main -f "commit=$c" -f "bisect_id=$RUN_ID" -f backup_restore=true "${stories_args[@]}"
    fi
  done
  for c in "${candidates[@]}"; do (outcome "$c" >"$wd/$c") & done
  wait
  culprit="" unknowns=()
  for c in "${candidates[@]}"; do
    res=$(cat "$wd/$c")
    echo "candidate ${c:0:12}: $res"
    if [[ $res == fail && -z $culprit ]]; then culprit=$c; fi
    if [[ $res == unknown ]]; then unknowns+=("$c"); fi
  done
  rm -rf "$wd"
  if [[ -n $culprit ]]; then
    final_title="e2e regression on main: $(commit_title "$culprit")"
    status="Culprit: $(git log -1 --format='%h' "$culprit") $(commit_title "$culprit") ($(git log -1 --format=%an "$culprit")). The e2e fails on that commit by itself."
    ((${#untested[@]} == 0)) || status+=" Older untested merges are listed below, so it may be an earlier one."
  elif ((${#candidates[@]} == 1)); then
    status="Suspected: $(git log -1 --format='%h' "${candidates[0]}") $(commit_title "${candidates[0]}"). Its confirming re-run did not fail (${res}), so this may be a flake or the environment. Re-run before reverting."
  else
    status="No candidate failed on its own: a flake, or an interaction between merges. Re-run the e2e on main before reverting anything."
  fi
  if ((${#unknowns[@]} > 0)); then
    status+=" Outcome unknown (cancelled, timed out or not found) for: $(for u in "${unknowns[@]}"; do printf '%s ' "${u:0:9}"; done)."
  fi
fi

body=$(render "$status")
if [[ $dry == 1 ]]; then
  echo "--- issue updated ---"
  echo "$final_title"
  echo "$body"
else
  "$GH" issue edit "$issue" --title "$final_title" --body "$body"
fi
