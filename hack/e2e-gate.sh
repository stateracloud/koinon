#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The e2e-kind workflow's `e2e result` job (the required check): did the e2e pass, and when this
# pull request needs one (hack/e2e-decide.sh says `required=true`), is there evidence on its head?
# Without this, a needs-e2e change merged on a green `e2e result` because the e2e was skipped (#562).
#
# Evidence, any one of (all on the PR's head commit):
#   1. this run's `quickstart` job succeeded;
#   2. another e2e-kind run on the head (a `gh workflow run e2e-kind.yml --ref <branch>` dispatch,
#      the `e2e` label's dispatch) has a `quickstart` job that succeeded;
#   3. the commit status `e2e-lab` is success (posted by hack/e2e-remote.sh --argo after a green lab
#      run, hack/e2e-status.sh). Its newest status counts.
#
# A status event does not start a workflow, so a later `e2e-lab` (or a dispatched run) does not
# re-evaluate this job by itself: hack/e2e-status.sh and the dispatched run's own `e2e result` job
# re-run the failed `e2e result` of the pull request run (hack/e2e-rerun-result.sh); by hand it is
# "Re-run failed jobs" on the pull request's e2e-kind run.
#
# Environment: DECIDE and QUICKSTART (the `needs.<job>.result` values), REQUIRED (true|false), SHA
# (the pull request head), BRANCH (its head branch, for the hint), GH (the gh command), GH_REPO,
# GITHUB_RUN_ID (this run, skipped when looking at other runs), GITHUB_STEP_SUMMARY.
# Exit 0 pass, 1 fail.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
export GH_PAGER=cat PAGER=cat

DECIDE=${DECIDE:-} QUICKSTART=${QUICKSTART:-} REQUIRED=${REQUIRED:-false}
SHA=${SHA:-} BRANCH=${BRANCH:-<branch>} GH=${GH:-gh} GH_REPO=${GH_REPO:-}

echo "decide: $DECIDE, quickstart: $QUICKSTART, e2e required: ${REQUIRED:-false}"
for r in "$DECIDE" "$QUICKSTART"; do
  case $r in success | skipped) ;; *) echo "::error::the e2e did not pass ($r)"; exit 1 ;; esac
done
[[ $REQUIRED == true ]] || exit 0

pass() {
  echo "e2e required and present: $1"
  [[ -z ${GITHUB_STEP_SUMMARY:-} ]] || printf '## e2e result\n\nA passing e2e is required for this pull request and it has one: %s\n' "$1" >>"$GITHUB_STEP_SUMMARY"
  exit 0
}

[[ $QUICKSTART != success ]] || pass "this run's quickstart job succeeded"

if [[ -n $SHA && -n $GH_REPO ]]; then
  # A green quickstart job of another e2e-kind run on the head.
  runs=$("$GH" run list --workflow e2e-kind.yml --commit "$SHA" --status success --limit 20 \
    --json databaseId --jq '.[].databaseId' 2>/dev/null || true)
  for id in $runs; do
    [[ $id != "${GITHUB_RUN_ID:-}" ]] || continue
    if [[ -n $("$GH" api "repos/$GH_REPO/actions/runs/$id/jobs?per_page=100" \
      --jq '.jobs[] | select(.name == "quickstart" and .conclusion == "success") | .id' 2>/dev/null || true) ]]; then
      pass "quickstart succeeded in e2e-kind run $id on ${SHA:0:12}"
    fi
  done
  # The lab's status (the newest e2e-lab status is first).
  lab=$("$GH" api "repos/$GH_REPO/commits/$SHA/statuses?per_page=100" \
    --jq '[.[] | select(.context == "e2e-lab")][0].state // ""' 2>/dev/null || true)
  [[ $lab != success ]] || pass "the commit status e2e-lab is success on ${SHA:0:12}"
else
  lab=""
fi

msg="This change needs the kind e2e (hack/needs-e2e.sh, or the e2e label) and the head ${SHA:0:12} has no passing one: no successful quickstart job in this or another e2e-kind run${lab:+, e2e-lab is $lab}."
how="Produce one: run \`hack/e2e-remote.sh --argo\` (the lab posts the status e2e-lab), or run \`gh workflow run e2e-kind.yml --ref $BRANCH\` on GitHub. Then re-run this job: \"Re-run failed jobs\" on this e2e-kind run (hack/e2e-status.sh and a dispatched run's result job do it for you)."
echo "::error::$msg"
echo "$how"
[[ -z ${GITHUB_STEP_SUMMARY:-} ]] || printf '## e2e result\n\n%s\n\n%s\n' "$msg" "$how" >>"$GITHUB_STEP_SUMMARY"
exit 1
