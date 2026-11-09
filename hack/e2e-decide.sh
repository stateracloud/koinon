#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The e2e-kind workflow's `decide` job: should the kind e2e run for this event? Prints
# `needed=true|false` (also appended to $GITHUB_OUTPUT) and, when it matters, a hint for the step
# summary ($GITHUB_STEP_SUMMARY). The workflow runs the BASE branch's copy of this script and of
# hack/needs-e2e.sh, so a pull request cannot change the rule that decides whether it is tested.
#
#   pull_request       needed when the full rule (needs-e2e.sh) says the change is e2e-relevant, when
#                      the PR touches the e2e's own files (--forced-only) or when it carries the
#                      `e2e` label. Otherwise not needed. A fork PR cannot label, dispatch or post
#                      `e2e-lab`, so relevance alone must run the quickstart (issue #148); the
#                      label and an external run are accelerators. The labels are the
#                      event's plus the PR's current ones: `gh pr create --label` opens the PR and
#                      then labels it (two events, old issue 468), and the run of `opened` should already see
#                      the label. Label events do not start the workflow; adding `e2e` later
#                      dispatches it (hack/e2e-label.sh).
#   merge_group        (the merge queue, #225, #444) needed when the entry's own diff against MG_BASE
#                      (merge_group.base_sha) matches the full rule or touches the e2e's own files:
#                      the quickstart runs on the queue commit (main plus the PR), and that run is the
#                      merge evidence. `required` is always false (no PR-head lookup). A failing rule
#                      script other than "not needed" reads as needed.
#   push               (main) needed when the full rule matches anything since the last green e2e
#                      run on main (push or schedule), or when there is no such run.
#   schedule           (nightly) always: drift no commit explains (upstream images, the runner
#                      image, downloads) is what it is for.
#   workflow_dispatch  always needed.
#
# `required=true` (pull requests only) is what `e2e result` enforces (hack/e2e-gate.sh, old issue 562): the
# change needs an e2e (the full rule, the e2e's own files or the label), so the job fails unless the
# head has a green quickstart or the `e2e-lab` status. On a pull request required implies
# needed. Every other event: `required=false`.
#
# Environment: EVENT, BASE (pull request base branch), MG_BASE (merge group base sha), LABELS (JSON array of the PR's label names),
# PR (its number), NEEDS_E2E (path of the rule script, default hack/needs-e2e.sh), GH (the gh
# command), GH_REPO.
# Any failure of the rule script other than "not needed" reads as needed.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

EVENT=${EVENT:?EVENT}
NEEDS=${NEEDS_E2E:-hack/needs-e2e.sh}
GH=${GH:-gh}
LABELS=${LABELS:-[]}

# emit <needed> [required]: needed = run the quickstart in this workflow run; required = the pull
# request must have a passing e2e on its head before `e2e result` goes green (hack/e2e-gate.sh).
emit() {
  local req=${2:-false}
  echo "needed=$1"
  echo "required=$req"
  [[ -z ${GITHUB_OUTPUT:-} ]] || printf 'needed=%s\nrequired=%s\n' "$1" "$req" >>"$GITHUB_OUTPUT"
}
# live_labels: the PR's labels now, as a JSON array; [] when they cannot be read.
live_labels() {
  [[ -n ${PR:-} && -n ${GH_REPO:-} ]] || { echo '[]'; return; }
  "$GH" api "repos/$GH_REPO/issues/$PR/labels" --jq '[.[].name]' 2>/dev/null || echo '[]'
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
    emit true true
    exit 0
  fi
  forced=$rc
  labelled=1
  if jq -e 'index("e2e")' <<<"$LABELS" >/dev/null 2>&1 || jq -e 'index("e2e")' <<<"$(live_labels)" >/dev/null 2>&1; then
    labelled=0
  fi
  if [[ $forced -eq 0 ]]; then
    summary "Runs: this pull request changes the e2e's own files."$'\n\n'"$text"
    emit true true
  elif [[ $labelled -eq 0 ]]; then
    summary "Runs: the pull request has the \`e2e\` label."
    emit true true
  else
    rule "$base"
    if [[ $rc -ne 1 ]]; then
      # E2e-relevant by the full rule (or the rule could not be evaluated): run the quickstart in this
      # run. A fork pull request cannot label, dispatch or post `e2e-lab`, so "required but not run
      # here" could never go green for it (issue #148). The label and an external run stay accelerators: the
      # label only matters for the nothing-relevant case above, a green `e2e-lab` also satisfies
      # `e2e result` (hack/e2e-gate.sh) and may land first.
      summary "Runs: this pull request is e2e-relevant per hack/needs-e2e.sh (or the rule could not be evaluated, exit $rc):"$'\n\n'"$text"$'\n\n'"The \`e2e-lab\` commit status (an external e2e run that passed on this commit) also satisfies \`e2e result\`."
      emit true true
    else
      summary "Does not run: nothing in this pull request needs the kind e2e."
      emit false false
    fi
  fi
  ;;
merge_group)
  # #444 (amends ADR 0094 decision 4): the queue commit is main plus the pull request, so the quickstart
  # runs on it when the entry's own diff against MG_BASE is e2e-relevant, and that run is the merge
  # evidence. `required` stays false: the evidence is this run's own quickstart, not the PR head's (the
  # PR-head gate already ran before the entry could be queued). A queue entry is never cancelled.
  mg=${MG_BASE:-}
  if [[ -z $mg ]]; then
    echo "::warning::no merge group base sha; running the e2e to be safe"
    emit true
    exit 0
  fi
  rule --forced-only "$mg"
  forced=$rc
  if [[ $forced -eq 0 ]]; then
    summary "Runs: this merge queue entry changes the e2e's own files."$'\n\n'"$text"
    emit true
  elif [[ $forced -ge 2 ]]; then
    echo "::warning::needs-e2e.sh exited $forced; running the e2e to be safe"
    emit true
  else
    rule "$mg"
    case $rc in
    1) summary "Does not run: nothing in this merge queue entry needs the kind e2e."; emit false ;;
    0) summary "Runs: this merge queue entry is e2e-relevant per hack/needs-e2e.sh:"$'\n\n'"$text"; emit true ;;
    *) echo "::warning::needs-e2e.sh exited $rc; running the e2e to be safe"; emit true ;;
    esac
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
