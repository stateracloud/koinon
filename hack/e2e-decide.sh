#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The e2e-kind workflow's `decide` job: should the kind e2e run for this event? Prints
# `needed=true|false` (also appended to $GITHUB_OUTPUT) and, when it matters, a hint for the step
# summary ($GITHUB_STEP_SUMMARY). The workflow runs the BASE branch's copy of this script and of
# hack/needs-e2e.sh, so a pull request cannot change the rule that decides whether it is tested.
#
#   pull_request       needed when the PR touches the e2e's own files (needs-e2e.sh --forced-only)
#                      or carries the `e2e` label. Otherwise not needed, and the summary says so
#                      when the full rule (needs-e2e.sh) would have wanted it. The labels are the
#                      event's plus the PR's current ones: `gh pr create --label` opens the PR and
#                      then labels it (two events, #468), and the run of `opened` should already see
#                      the label. Adding the `e2e` label (ACTION=labeled) to a head that another run
#                      of this workflow already ran the e2e on (or is running it) runs nothing and
#                      prints `covered=true`: the result job mirrors that run. The workflow queues the
#                      labelled run behind the other instead of cancelling it.
#   push               (main) needed when the full rule matches anything since the last green e2e
#                      run on main (push or schedule), or when there is no such run.
#   schedule           (nightly) always: drift no commit explains (upstream images, the runner
#                      image, downloads) is what it is for.
#   workflow_dispatch  always needed.
#
# Environment: EVENT, BASE (pull request base branch), LABELS (JSON array of the PR's label names),
# ACTION (the pull_request action), PR (its number), HEAD_SHA, RUN_ID (this run), NEEDS_E2E (path
# of the rule script, default hack/needs-e2e.sh), GH (the gh command), GH_REPO.
# Any failure of the rule script other than "not needed" reads as needed.
set -euo pipefail

EVENT=${EVENT:?EVENT}
NEEDS=${NEEDS_E2E:-hack/needs-e2e.sh}
GH=${GH:-gh}
LABELS=${LABELS:-[]}

emit() { # <needed> [covered]
  echo "needed=$1"
  [[ -z ${GITHUB_OUTPUT:-} ]] || echo "needed=$1" >>"$GITHUB_OUTPUT"
  if [[ -n ${2:-} ]]; then
    echo "covered=$2"
    [[ -z ${GITHUB_OUTPUT:-} ]] || echo "covered=$2" >>"$GITHUB_OUTPUT"
  fi
}
# live_labels: the PR's labels now, as a JSON array; [] when they cannot be read.
live_labels() {
  [[ -n ${PR:-} && -n ${GH_REPO:-} ]] || { echo '[]'; return; }
  "$GH" api "repos/$GH_REPO/issues/$PR/labels" --jq '[.[].name]' 2>/dev/null || echo '[]'
}
# ran_on_head: the id of another run of this workflow on HEAD_SHA, not cancelled, whose quickstart
# job ran (or runs); empty for none.
ran_on_head() {
  [[ -n ${HEAD_SHA:-} && -n ${GH_REPO:-} ]] || return 0
  local id n
  for id in $("$GH" api "repos/$GH_REPO/actions/workflows/e2e-kind.yml/runs?head_sha=$HEAD_SHA&event=pull_request&per_page=30" \
    --jq ".workflow_runs[] | select(.id != ${RUN_ID:-0} and .conclusion != \"cancelled\") | .id" 2>/dev/null); do
    n=$("$GH" api "repos/$GH_REPO/actions/runs/$id/jobs" \
      --jq '[.jobs[] | select((.name | startswith("quickstart")) and .conclusion != "skipped" and .conclusion != "cancelled")] | length' 2>/dev/null || echo 0)
    if [[ $n -gt 0 ]]; then
      echo "$id"
      return 0
    fi
  done
}
summary() {
  [[ -z ${GITHUB_STEP_SUMMARY:-} ]] || printf '## Kind e2e\n\n%s\n' "$*" >>"$GITHUB_STEP_SUMMARY"
  printf '%s\n' "$*"
}

# rule <args...>: runs the rule script; sets $rc and $text.
rule() {
  rc=0
  text=$("$NEEDS" "$@" 2>&1) || rc=$?
}

case $EVENT in
pull_request)
  base="origin/${BASE:?BASE}"
  rule --forced-only "$base"
  if [[ $rc -ge 2 ]]; then
    echo "::warning::needs-e2e.sh exited $rc; running the e2e to be safe"
    emit true
    exit 0
  fi
  forced=$rc
  if [[ ${ACTION:-} == labeled ]] && covered=$(ran_on_head) && [[ -n $covered ]]; then
    summary "Does not run again: run $covered already ran the e2e on this commit; this result mirrors it."
    emit false true
    exit 0
  fi
  labelled=1
  if jq -e 'index("e2e")' <<<"$LABELS" >/dev/null 2>&1 || jq -e 'index("e2e")' <<<"$(live_labels)" >/dev/null 2>&1; then
    labelled=0
  fi
  if [[ $forced -eq 0 ]]; then
    summary "Runs: this pull request changes the e2e's own files."$'\n\n'"$text"
    emit true
  elif [[ $labelled -eq 0 ]]; then
    summary "Runs: the pull request has the \`e2e\` label."
    emit true
  else
    rule "$base"
    if [[ $rc -ne 1 ]]; then
      summary "Does not run. This PR is e2e-relevant per hack/needs-e2e.sh (or the rule could not be evaluated, exit $rc):"$'\n\n'"$text"$'\n\n'"Run \`hack/e2e-remote.sh\` (your own Docker), or add the \`e2e\` label to run it here."
    else
      summary "Does not run: nothing in this pull request needs the kind e2e."
    fi
    emit false
  fi
  ;;
workflow_dispatch)
  emit true
  ;;
schedule)
  summary "Runs: the nightly run always runs, to catch drift nothing in the tree explains."
  emit true
  ;;
push)
  covered=$("$GH" run list --workflow e2e-kind.yml --branch main --status success --limit 30 \
    --json headSha,event --jq '[.[] | select(.event == "push" or .event == "schedule")][0].headSha // empty' 2>/dev/null || true)
  if [[ -z $covered ]]; then
    summary "Runs: no green e2e run on main to compare with."
    emit true
    exit 0
  fi
  rule "$covered"
  case $rc in
  0) summary "Runs: changes since the last green e2e on main (${covered:0:12}):"$'\n\n'"$text"; emit true ;;
  1) summary "Does not run: nothing e2e-relevant changed since the last green e2e on main (${covered:0:12})."; emit false ;;
  *) echo "::warning::needs-e2e.sh exited $rc; running the e2e to be safe"; emit true ;;
  esac
  ;;
*)
  echo "e2e-decide: unexpected event $EVENT; running the e2e" >&2
  emit true
  ;;
esac
