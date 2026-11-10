#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The e2e-kind workflow's `decide` job: should the kind e2e run for this event? Prints
# `needed=true|false` (also appended to $GITHUB_OUTPUT) and, when it matters, a hint for the step
# summary ($GITHUB_STEP_SUMMARY). The workflow runs the BASE branch's copy of this script and of
# hack/needs-e2e.sh, so a pull request cannot change the rule that decides whether it is tested.
#
#   pull_request       advisory (ADR 0105): needed when the full rule (needs-e2e.sh) says the change is
#                      e2e-relevant, when the PR touches the e2e's own files (--forced-only) or when it carries
#                      the `e2e` label. A fork PR (HEAD_REPO != BASE_REPO) runs it only with the label: the
#                      flavor images on GHCR are private until v0.3. The labels are the
#                      event's plus the PR's current ones: `gh pr create --label` opens the PR and
#                      then labels it (two events, old issue 468), and the run of `opened` should already see
#                      the label. Label events do not start the workflow.
#   merge_group        (the merge queue, #225, #444, ADR 0102, ADR 0105) needed when the pull request named by
#                      the queue ref (QUEUE_REF, gh-readonly-queue/main/pr-<N>-<sha>) carries the `e2e` label,
#                      or the entry's own diff against MG_BASE (merge_group.base_sha) matches the full rule or
#                      touches the e2e's own files: the quickstart runs on the queue commit (main plus the PR),
#                      and that run is the one merge evidence. A label read that fails, a ref that cannot be
#                      parsed, or a failing rule script other than "not needed" reads as needed.
#   push               (main) needed when the full rule matches anything since the last green e2e
#                      run on main (push or schedule), or when there is no such run. Not needed when a
#                      green merge_group quickstart ran on a commit with the pushed commit's tree
#                      (PUSH_SHA): the queue already tested exactly this tree (#444, ADR 0102).
#   schedule           (nightly) always: drift no commit explains (upstream images, the runner
#                      image, downloads) is what it is for.
#   workflow_dispatch  always needed.
#
# Environment: EVENT, BASE (pull request base branch), MG_BASE (merge group base sha), QUEUE_REF (the merge group
# ref), LABELS (JSON array of the PR's label names), PR (its number), HEAD_REPO and BASE_REPO (pull request, to
# tell a fork), PUSH_SHA (the pushed commit, push events), NEEDS_E2E (path of the rule script, default
# hack/needs-e2e.sh), GH (the gh command), GH_REPO.
# Any failure of the rule script other than "not needed" reads as needed.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

EVENT=${EVENT:?EVENT}
NEEDS=${NEEDS_E2E:-hack/needs-e2e.sh}
GH=${GH:-gh}
LABELS=${LABELS:-[]}

# emit <needed>: run the quickstart in this workflow run.
emit() {
  echo "needed=$1"
  [[ -z ${GITHUB_OUTPUT:-} ]] || printf 'needed=%s\n' "$1" >>"$GITHUB_OUTPUT"
}
# live_labels: the PR's labels now, as a JSON array; [] when they cannot be read.
live_labels() {
  [[ -n ${PR:-} && -n ${GH_REPO:-} ]] || { echo '[]'; return; }
  "$GH" api "repos/$GH_REPO/issues/$PR/labels" --jq '[.[].name]' 2>/dev/null || echo '[]'
}
# queued_labels <number>: like live_labels for the queued PR, but a read that fails answers ["e2e"] (fail closed).
queued_labels() {
  [[ -n ${GH_REPO:-} ]] || { echo '["e2e"]'; return; }
  "$GH" api "repos/$GH_REPO/issues/$1/labels" --jq '[.[].name]' 2>/dev/null || echo '["e2e"]'
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
  labelled=1
  if jq -e 'index("e2e")' <<<"$LABELS" >/dev/null 2>&1 || jq -e 'index("e2e")' <<<"$(live_labels)" >/dev/null 2>&1; then
    labelled=0
  fi
  if [[ -n ${HEAD_REPO:-} && -n ${BASE_REPO:-} && $HEAD_REPO != "$BASE_REPO" ]]; then
    if [[ $labelled -eq 0 ]]; then
      summary "Runs (advisory): a maintainer added the \`e2e\` label to this fork pull request."
      emit true
    else
      summary "Does not run: a fork pull request runs the kind e2e only with the \`e2e\` label (the flavor images are private until v0.3). The merge queue runs it."
      emit false
    fi
    exit 0
  fi
  rule --forced-only "$base"
  if [[ $rc -ge 2 ]]; then
    echo "::warning::needs-e2e.sh exited $rc; running the e2e to be safe"
    emit true
    exit 0
  fi
  forced=$rc
  if [[ $forced -eq 0 ]]; then
    summary "Runs (advisory): this pull request changes the e2e's own files."$'\n\n'"$text"
    emit true
  elif [[ $labelled -eq 0 ]]; then
    summary "Runs (advisory): the pull request has the \`e2e\` label."
    emit true
  else
    rule "$base"
    if [[ $rc -ne 1 ]]; then
      summary "Runs (advisory): this pull request is e2e-relevant per hack/needs-e2e.sh (or the rule could not be evaluated, exit $rc):"$'\n\n'"$text"
      emit true
    else
      summary "Does not run: nothing in this pull request needs the kind e2e."
      emit false
    fi
  fi
  ;;
merge_group)
  # #444 (ADR 0102, ADR 0105): the queue commit is main plus the pull request, so the quickstart runs on it when
  # the pull request carries the `e2e` label or the entry's own diff against MG_BASE is e2e-relevant, and that run
  # is the one merge evidence. A queue entry is never cancelled. The label is read now, when the queue builds
  # the entry; one added later does not rebuild it.
  if [[ ${QUEUE_REF:-} =~ /pr-([0-9]+)-[0-9a-f]+$ ]]; then
    if jq -e 'index("e2e")' <<<"$(queued_labels "${BASH_REMATCH[1]}")" >/dev/null 2>&1; then
      summary "Runs: pull request #${BASH_REMATCH[1]} has the \`e2e\` label (or its labels could not be read)."
      emit true
      exit 0
    fi
  else
    echo "::warning::cannot read a pull request number from the queue ref '${QUEUE_REF:-}'; running the e2e to be safe"
    emit true
    exit 0
  fi
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
  # #444 (ADR 0102): a queue merge puts on main the tree the queue's quickstart already tested. When a
  # green merge_group quickstart ran on a commit with the pushed commit's tree, this push needs no second run.
  if [[ -n ${PUSH_SHA:-} && -n ${GH_REPO:-} ]]; then
    pushed_tree=$("$GH" api "repos/$GH_REPO/git/commits/$PUSH_SHA" --jq .tree.sha 2>/dev/null || true)
    if [[ -n $pushed_tree ]]; then
      while read -r id sha; do
        [[ -n ${id:-} ]] || continue
        [[ $("$GH" api "repos/$GH_REPO/git/commits/$sha" --jq .tree.sha 2>/dev/null || true) == "$pushed_tree" ]] || continue
        if [[ -n $("$GH" api "repos/$GH_REPO/actions/runs/$id/jobs?per_page=100" \
          --jq '.jobs[] | select(.name == "quickstart" and .conclusion == "success") | .id' 2>/dev/null || true) ]]; then
          summary "Does not run: the merge queue's e2e run $id passed on a commit with this commit's tree (${pushed_tree:0:12})."
          emit false
          exit 0
        fi
      done < <("$GH" run list --workflow e2e-kind.yml --event merge_group --status success --limit 20 \
        --json databaseId,headSha --jq '.[] | "\(.databaseId) \(.headSha)"' 2>/dev/null || true)
    fi
  fi
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
