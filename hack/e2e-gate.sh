#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The e2e-kind workflow's `e2e result` job (the required check): did the e2e pass, and when this
# pull request needs one (hack/e2e-decide.sh says `required=true`), is there evidence on its head?
# Without this, a needs-e2e change merged on a green `e2e result` because the e2e was skipped (koinon-private#562).
#
# Evidence, any one of (all on the PR's head commit):
#   1. this run's `quickstart` job succeeded;
#   2. another e2e-kind run on the head (a `gh workflow run e2e-kind.yml --ref <branch>` dispatch,
#      the `e2e` label's dispatch) has a go-suite `quickstart` job that succeeded (not a bisect
#      dispatch of another commit, not the web suite);
#   3. the commit status `e2e-lab` is success (an external e2e run that passed on this commit, posted with
#      hack/e2e-status.sh). Its newest status counts.
#
# A status event does not start a workflow, so a later `e2e-lab` (or a dispatched run) does not
# re-evaluate this job by itself: hack/e2e-status.sh and the dispatched run's own `e2e result` job
# re-run the `e2e result` job of the pull request run (hack/e2e-rerun-result.sh; not while that run
# is still in progress); by hand it is "Re-run failed jobs" on the pull request's e2e-kind run.
#
# Trust limit: `result` runs from the pull request's own e2e-kind.yml, so a PR can edit the YAML to
# drop this gate. The guard is the ruleset's required review on `.github/**` (not CODEOWNERS:
# require_code_owner_review is false). `e2e result` is not pinned to an app and `e2e-lab` can be
# posted by anyone with write access: a guardrail against mistakes, not against a malicious member.
# The follow-up is a pinned integration_id (koinon-private#568).
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
case $DECIDE in success | skipped) ;; *) echo "::error::the e2e did not pass (decide: $DECIDE)"; exit 1 ;; esac
case $QUICKSTART in
success | skipped) ;;
# This run's own quickstart failed or was cancelled (a flake, say). When the e2e is required, the
# an external run stays an accelerator: fall through to the evidence checks (another run's quickstart, e2e-lab)
# instead of failing here; with none, the gate still fails below.
failure | cancelled) [[ $REQUIRED == true ]] || { echo "::error::the e2e did not pass (quickstart: $QUICKSTART)"; exit 1; } ;;
*) echo "::error::the e2e did not pass (quickstart: $QUICKSTART)"; exit 1 ;;
esac
[[ $REQUIRED == true ]] || exit 0

pass() {
  echo "e2e required and present: $1"
  [[ -z ${GITHUB_STEP_SUMMARY:-} ]] || printf '## e2e result\n\nA passing e2e is required for this pull request and it has one: %s\n' "$1" >>"$GITHUB_STEP_SUMMARY"
  exit 0
}

[[ $QUICKSTART != success ]] || pass "this run's quickstart job succeeded"

if [[ -n $SHA && -n $GH_REPO ]]; then
  # A green go-suite quickstart job of another e2e-kind run on the head: a pull_request run, or a
  # plain dispatch (display title "e2e-kind", as hack/e2e-label.sh; a bisect dispatch with a commit
  # input tests another commit). The job's own conclusion is judged, not the run's: the dispatched run
  # that re-runs this one is still in progress. Only a quickstart job whose "Go suite" marker step
  # succeeded counts (the web suite, or any future suite, only runs something else).
  runs=$("$GH" run list --workflow e2e-kind.yml --commit "$SHA" --limit 30 --json databaseId,event,displayTitle \
    --jq '.[] | select(.event == "pull_request" or (.event == "workflow_dispatch" and .displayTitle == "e2e-kind")) | .databaseId' 2>/dev/null || true)
  for id in $runs; do
    [[ $id != "${GITHUB_RUN_ID:-}" ]] || continue
    if [[ -n $("$GH" api "repos/$GH_REPO/actions/runs/$id/jobs?per_page=100" \
      --jq '.jobs[] | select(.name == "quickstart" and .conclusion == "success" and any(.steps[]?; .name == "Go suite (marker for the e2e gate)" and .conclusion == "success")) | .id' 2>/dev/null || true) ]]; then
      pass "quickstart succeeded in e2e-kind run $id on ${SHA:0:12}"
    fi
  done
  # The e2e-lab status (the newest e2e-lab status is first). An API error (a 403 when the job lacks
  # `statuses: read`, koinon-private#588) or a body that is not JSON is reported as such, not as "no e2e-lab";
  # the gate still fails closed.
  lab="" laberr=""
  errf=$(mktemp "${TMPDIR:-/tmp}/e2e-gate.XXXXXX")
  rc=0
  resp=$("$GH" api "repos/$GH_REPO/commits/$SHA/statuses?per_page=100" 2>"$errf") || rc=$?
  if [[ $rc -ne 0 ]]; then
    laberr="$(tr '\n' ' ' <"$errf")"
    laberr=${laberr:-exit $rc}
  elif ! lab=$(jq -r '[.[] | select(.context == "e2e-lab")][0].state // ""' <<<"$resp" 2>/dev/null); then
    lab="" laberr="invalid JSON in the response"
  fi
  rm -f "$errf"
  [[ -z $laberr ]] || echo "::error::could not read e2e-lab: $laberr (check the job's statuses: read permission)"
  [[ $lab != success ]] || pass "the commit status e2e-lab is success on ${SHA:0:12}"
else
  lab="" laberr=""
fi

msg="This change needs the kind e2e (hack/needs-e2e.sh, or the e2e label) and the head ${SHA:0:12} has no passing one: no successful quickstart job in this${QUICKSTART:+ ($QUICKSTART)} or another e2e-kind run${lab:+, e2e-lab is $lab}${laberr:+, and e2e-lab could not be read ($laberr; the job needs statuses: read)}."
how="Produce one: run \`gh workflow run e2e-kind.yml --ref $BRANCH\` on GitHub (or post the commit status e2e-lab from an external e2e run that passed on this commit). Then re-run this job: \"Re-run failed jobs\" on this e2e-kind run (hack/e2e-status.sh and a dispatched run's result job do it for you)."
echo "::error::$msg"
echo "$how"
[[ -z ${GITHUB_STEP_SUMMARY:-} ]] || printf '## e2e result\n\n%s\n\n%s\n' "$msg" "$how" >>"$GITHUB_STEP_SUMMARY"
exit 1
