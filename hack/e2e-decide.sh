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
#                      when the full rule (needs-e2e.sh) would have wanted it.
#   push               (main) needed when the full rule matches anything since the last green e2e
#                      run on main (push or schedule), or when there is no such run.
#   schedule           (nightly) always: drift no commit explains (upstream images, the runner
#                      image, downloads) is what it is for.
#   workflow_dispatch  always needed.
#
# Environment: EVENT, BASE (pull request base branch), LABELS (JSON array of the PR's label names),
# NEEDS_E2E (path of the rule script, default hack/needs-e2e.sh), GH (the gh command), GH_REPO.
# Any failure of the rule script other than "not needed" reads as needed.
set -euo pipefail

EVENT=${EVENT:?EVENT}
NEEDS=${NEEDS_E2E:-hack/needs-e2e.sh}
GH=${GH:-gh}
LABELS=${LABELS:-[]}

emit() {
  echo "needed=$1"
  [[ -z ${GITHUB_OUTPUT:-} ]] || echo "needed=$1" >>"$GITHUB_OUTPUT"
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
  labelled=1
  if jq -e 'index("e2e")' <<<"$LABELS" >/dev/null 2>&1; then labelled=0; fi
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
