#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The kind e2e failed on main. Find which merge broke it and open one issue naming it.
#
#   hack/e2e-bisect.sh [--dry-run] [--green SHA] [--head SHA] [--results SHA=pass|fail,...]
#
# 1. The merges (first-parent commits) since the last green e2e run on main (--green; by default
#    asked of GitHub), up to --head (default HEAD).
# 2. Candidates: those whose changed files hack/needs-e2e.sh --stdin says need the e2e (usually 1-3).
# 3. With two or more candidates, dispatches the e2e-kind workflow on each (input `commit`), in
#    parallel, and waits. The culprit is the earliest candidate whose run failed. With one candidate
#    it is the culprit without a run (the batched run already tested exactly it plus docs).
# 4. Opens one issue, labelled e2e-regression, with the fix-forward-or-revert policy.
#
# --dry-run calls no GitHub API: it prints what it would dispatch and the issue, and takes the
# dispatched runs' outcomes from --results (default pass). Environment: GH (the gh command),
# GH_REPO (owner/name), GITHUB_RUN_URL (linked from the issue).
set -euo pipefail

dry=0 green="" head=HEAD results=""
while [[ $# -gt 0 ]]; do
  case $1 in
  --dry-run) dry=1 ;;
  --green) green=$2; shift ;;
  --head) head=$2; shift ;;
  --results) results=$2; shift ;;
  *) echo "e2e-bisect: unknown argument $1" >&2; exit 2 ;;
  esac
  shift
done
GH=${GH:-gh}
cd "$(git rev-parse --show-toplevel)"
head=$(git rev-parse "$head")

if [[ -z $green ]]; then
  green=$("$GH" run list --workflow e2e-kind.yml --branch main --status success --limit 30 \
    --json headSha,event --jq '[.[] | select(.event == "push" or .event == "schedule")][0].headSha // empty')
fi
if [[ -n $green ]]; then range="$green..$head"; else range="-n 10 $head"; fi
# shellcheck disable=SC2086 # range is intentionally two words in the fallback
merges=$(git log --first-parent --reverse --format=%H $range)

candidates=()
while IFS= read -r c; do
  [[ -n $c ]] || continue
  if git diff --name-only --no-renames "$c^1" "$c" 2>/dev/null | hack/needs-e2e.sh --stdin >/dev/null; then
    candidates+=("$c")
  fi
done <<<"$merges"
echo "merges since ${green:-?}: $(grep -c . <<<"$merges"); e2e-relevant candidates: ${#candidates[@]}"

# outcome <sha>: pass|fail for a dispatched run.
outcome() {
  local sha=$1 r
  if [[ $dry == 1 ]]; then
    for r in ${results//,/ }; do [[ $r == "${sha:0:7}"*=* || $r == "$sha"=* ]] && { echo "${r#*=}"; return; }; done
    echo pass
    return
  fi
  local id=""
  for _ in $(seq 60); do
    id=$("$GH" run list --workflow e2e-kind.yml --event workflow_dispatch --limit 30 --json databaseId,displayTitle \
      --jq ".[] | select(.displayTitle | contains(\"${sha:0:12}\")) | .databaseId" | head -1)
    [[ -n $id ]] && break
    sleep 10
  done
  [[ -n $id ]] || { echo fail; return; }
  if "$GH" run watch "$id" --exit-status >/dev/null 2>&1; then echo pass; else echo fail; fi
}

culprit="" note=""
case ${#candidates[@]} in
0) note="No merge since the last green run changed anything the e2e depends on: suspect a flake or the environment (runner, an upstream image or download)." ;;
1) culprit=${candidates[0]}; note="It is the only e2e-relevant merge since the last green run, so no separate runs were needed." ;;
*)
  for c in "${candidates[@]}"; do
    if [[ $dry == 1 ]]; then echo "would dispatch: gh workflow run e2e-kind.yml -f commit=$c"; else
      "$GH" workflow run e2e-kind.yml --ref main -f "commit=$c"
    fi
  done
  # Wait in parallel; collect through files so the subshells can report.
  wd=$(mktemp -d)
  for c in "${candidates[@]}"; do
    (outcome "$c" >"$wd/$c") &
  done
  wait
  for c in "${candidates[@]}"; do
    res=$(cat "$wd/$c")
    echo "candidate ${c:0:12}: $res"
    if [[ -z $culprit && $res == fail ]]; then culprit=$c; fi
  done
  rm -rf "$wd"
  [[ -n $culprit ]] || note="No candidate failed on its own: a flake, or an interaction between merges. Re-run the e2e on main before reverting anything."
  ;;
esac

title="e2e regression on main"
if [[ -n $culprit ]]; then
  title="e2e regression on main: $(git log -1 --format=%s "$culprit")"
fi
body="The kind e2e failed on main"
[[ -z ${GITHUB_RUN_URL:-} ]] || body+=" ([failed run](${GITHUB_RUN_URL}))"
body+=$'.\n\n'
if [[ -n $culprit ]]; then
  body+="Culprit: $(git log -1 --format='%h %s (%an)' "$culprit")"$'\n\n'"$note"$'\n\n'
else
  body+="$note"$'\n\n'
fi
body+="Candidates since the last green run (oldest first):"$'\n'
for c in ${candidates[@]+"${candidates[@]}"}; do body+="- $(git log -1 --format='%h %s' "$c")"$'\n'; done
[[ ${#candidates[@]} -gt 0 ]] || body+="- none"$'\n'
body+=$'\n''**Policy: fix forward within 2 hours of this issue opening, otherwise revert the merge.** Main stays releasable: release candidate tags are only cut from a green kind e2e run.'

if [[ $dry == 1 ]]; then
  echo "--- issue (label e2e-regression) ---"
  echo "$title"
  echo
  echo "$body"
  exit 0
fi
"$GH" label create e2e-regression --color B60205 --description "The kind e2e is red on main" >/dev/null 2>&1 || true
"$GH" issue create --title "$title" --body "$body" --label e2e-regression
