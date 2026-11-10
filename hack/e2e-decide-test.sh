#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/e2e-decide.sh with a stub rule script and a stub gh.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."
tmp=$(mktemp -d "${TMPDIR:-/tmp}/e2e-decide-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# Stub rule: STUB_FORCED / STUB_FULL are the exit codes for --forced-only and the full rule.
cat >"$tmp/needs" <<'SH'
#!/usr/bin/env bash
if [[ $1 == --forced-only ]]; then echo "forced stub"; exit "${STUB_FORCED:-1}"; fi
echo "full stub $*"; exit "${STUB_FULL:-1}"
SH
# Stub gh: answers from fixture JSON and applies the script's own --jq with the real jq, so a typo in a
# filter fails a test. `run list` also honours --event and --status the way gh does. Fixtures come from
# env: STUB_LABELS (the PR's label names; served as the API's label objects), STUB_COVERED (head sha of the last green push run on main),
# STUB_MG_RUNS ("<run id> <sha>": a green merge_group run), STUB_MG_FAILED_RUN (a merge_group run that
# failed overall although its quickstart job passed), STUB_MG_JOB (conclusion of run <id>'s quickstart job;
# empty: that job does not exist), STUB_TREE_PUSH / STUB_TREE_MG (tree sha of the pushed / any other commit).
cat >"$tmp/gh" <<'SH'
#!/usr/bin/env bash
args=("$@")
expr=. event= status=
for ((i = 0; i < ${#args[@]}; i++)); do
  case ${args[i]} in
  --jq) expr=${args[i + 1]} ;;
  --event) event=${args[i + 1]} ;;
  --status) status=${args[i + 1]} ;;
  esac
done
case "$*" in
*/issues/*/labels*) [[ -z ${STUB_LABELS_FAIL:-} ]] || exit 1; printf '%s\n' "${STUB_LABELS:-[]}" | jq -c '[.[] | {id: 1, name: .}]' | jq -c "$expr" ;;
*git/commits/pushsha*) [[ -n ${STUB_TREE_PUSH:-} ]] && jq -n --arg t "$STUB_TREE_PUSH" '{sha: "pushsha", tree: {sha: $t}}' | jq -r "$expr" ;;
*git/commits/*) [[ -n ${STUB_TREE_MG:-} ]] && jq -n --arg t "$STUB_TREE_MG" '{sha: "x", tree: {sha: $t}}' | jq -r "$expr" ;;
*"run list"*)
  jq -n --arg cov "${STUB_COVERED:-}" --arg mg "${STUB_MG_RUNS:-}" --arg bad "${STUB_MG_FAILED_RUN:-}" '
    [ {databaseId: 90, headSha: "prsha", event: "pull_request", status: "success"},
      {databaseId: 91, headSha: "oldsha", event: "workflow_dispatch", status: "success"} ]
    + (if $bad != "" then [{databaseId: ($bad | tonumber), headSha: "mgbad", event: "merge_group", status: "failure"}] else [] end)
    + (if $cov != "" then [{databaseId: 78, headSha: $cov, event: "push", status: "success"}] else [] end)
    + (if $mg != "" then [{databaseId: ($mg | split(" ")[0] | tonumber), headSha: ($mg | split(" ")[1]), event: "merge_group", status: "success"}] else [] end)' |
    jq --arg e "$event" --arg s "$status" '[.[] | select(($e == "" or .event == $e) and ($s == "" or .status == $s))]' | jq -r "$expr"
  ;;
*actions/runs/*/jobs*)
  jq -n --arg j "${STUB_MG_JOB:-}" '{jobs: ([{id: 1, name: "decide", conclusion: "success"}, {id: 2, name: "e2e result", conclusion: "success"}]
    + (if $j != "" then [{id: 9, name: "quickstart", conclusion: $j}] else [] end))}' | jq -r "$expr" ;;
*) echo "gh stub: unexpected $*" >&2; exit 1 ;;
esac
SH
chmod +x "$tmp/needs" "$tmp/gh"
export NEEDS_E2E="$tmp/needs" GH="$tmp/gh" BASE=main

fail=0
# want <true|false> <description> [VAR=value...]: runs the decision with those variables.
want() {
  local expect=$1 desc=$2 got
  shift 2
  export GITHUB_STEP_SUMMARY="$tmp/summary"
  : >"$GITHUB_STEP_SUMMARY"
  got=$(env "$@" hack/e2e-decide.sh | sed -n 's/^needed=//p' | tail -1)
  if [[ $got == "$expect" ]]; then echo "ok: $desc"; else echo "FAIL: $desc (got '$got', want $expect)" >&2; fail=1; fi
}
summary_has() { grep -q -- "$1" "$tmp/summary" || { echo "FAIL: summary lacks '$1'" >&2; fail=1; }; }

want true "PR touching the e2e's files runs" EVENT=pull_request STUB_FORCED=0
want true "PR with the e2e label runs" EVENT=pull_request 'LABELS=["bug","e2e"]'
want false "PR with other labels and nothing relevant does not run" EVENT=pull_request 'LABELS=["bug"]'
want true "PR that is e2e-relevant but unlabelled runs (fork PRs cannot label, issue #148)" EVENT=pull_request STUB_FULL=0
summary_has "e2e-relevant"
want true "PR: full-rule failure runs the e2e" EVENT=pull_request STUB_FULL=2
want true "PR: a rule failure fails closed" EVENT=pull_request STUB_FORCED=2
want false "PR: nothing relevant says so" EVENT=pull_request
summary_has "nothing in this pull request"
# old issue 468: `gh pr create --label e2e` fires `opened` without the label, then `labeled`.
PRV=(PR=524 GH_REPO=o/r)
want true "opened: the label added after the event counts" EVENT=pull_request ACTION=opened "${PRV[@]}" 'STUB_LABELS=["e2e"]'
want false "opened: no label now either" EVENT=pull_request ACTION=opened "${PRV[@]}" 'STUB_LABELS=["bug"]'
want true "dispatch always runs" EVENT=workflow_dispatch
want true "push with no green run to compare with runs" EVENT=push
want true "push with e2e-relevant changes since the last green run" EVENT=push STUB_COVERED=abc123 STUB_FULL=0
want false "push without them does not" EVENT=push STUB_COVERED=abc123 STUB_FULL=1
want true "push: a rule failure fails closed" EVENT=push STUB_COVERED=abc123 STUB_FULL=2
# #444: a push whose tree a green merge_group quickstart already tested does not run again.
PQ=(EVENT=push PUSH_SHA=pushsha GH_REPO=o/r STUB_COVERED=abc123 STUB_FULL=0 'STUB_MG_RUNS=77 mgsha' STUB_TREE_PUSH=tree1 STUB_TREE_MG=tree1 STUB_MG_JOB=success)
want false "push: same tree as a green queue quickstart does not run" "${PQ[@]}"
summary_has "merge queue"
want true "push: a different tree runs" "${PQ[@]}" STUB_TREE_MG=tree2
want true "push: the queue run's quickstart was skipped runs" "${PQ[@]}" STUB_MG_JOB=skipped
want true "push: the queue run's quickstart was cancelled runs" "${PQ[@]}" STUB_MG_JOB=cancelled
want true "push: the queue run has no quickstart job runs" "${PQ[@]}" STUB_MG_JOB=
want true "push: only a failed queue run (its quickstart passed) runs" "${PQ[@]}" STUB_MG_RUNS= STUB_MG_FAILED_RUN=70
want false "push: a failed queue run next to a green one still skips on the green one" "${PQ[@]}" STUB_MG_FAILED_RUN=70
want true "push: no queue run runs" "${PQ[@]}" STUB_MG_RUNS=
want true "push: unreadable pushed tree runs" "${PQ[@]}" STUB_TREE_PUSH=
want true "push: without PUSH_SHA the queue is not consulted" EVENT=push GH_REPO=o/r STUB_COVERED=abc123 STUB_FULL=0 'STUB_MG_RUNS=77 mgsha' STUB_TREE_PUSH=tree1 STUB_TREE_MG=tree1 STUB_MG_JOB=success
# #444, ADR 0105: the queue commit (main plus the PR) runs the quickstart when the entry is e2e-relevant or the
# pull request carries the `e2e` label (read from the queue ref's PR number).
MG=(EVENT=merge_group MG_BASE=abc123 GH_REPO=o/r QUEUE_REF=refs/heads/gh-readonly-queue/main/pr-12-0a1b2c)
want true "queue entry touching the e2e's files runs" "${MG[@]}" STUB_FORCED=0
want true "queue entry that is e2e-relevant runs" "${MG[@]}" STUB_FULL=0
want false "queue entry with nothing relevant does not run" "${MG[@]}"
want true "queue entry: a forced-rule failure fails closed" "${MG[@]}" STUB_FORCED=2
want true "queue entry: a full-rule failure fails closed" "${MG[@]}" STUB_FULL=2
want true "queue entry without a base sha runs" "${MG[@]}" MG_BASE=
want true "queue entry with the e2e label runs although nothing is relevant" "${MG[@]}" 'STUB_LABELS=["bug","e2e"]'
summary_has "e2e"
want false "queue entry with other labels and nothing relevant does not run" "${MG[@]}" 'STUB_LABELS=["bug"]'
want false "queue entry with no labels and nothing relevant does not run" "${MG[@]}" 'STUB_LABELS=[]'
want true "queue entry: a label read error fails closed (needed)" "${MG[@]}" STUB_LABELS_FAIL=1
want true "queue entry: an unparsable ref fails closed (needed)" "${MG[@]}" QUEUE_REF=refs/heads/gh-readonly-queue/main/pr-x-0a1b2c
want true "queue entry: a missing ref fails closed (needed)" "${MG[@]}" QUEUE_REF=
# The output has no `required` line any more (ADR 0105).
out=$(env "${MG[@]}" hack/e2e-decide.sh)
if grep -q '^required=' <<<"$out"; then echo "FAIL: decide still prints required=" >&2; fail=1; else echo "ok: no required= output"; fi
want true "nightly always runs (drift detection)" EVENT=schedule STUB_COVERED=abc123 STUB_FULL=1

# Fork pull requests (ADR 0105): the flavor images are private until v0.3, so the advisory run needs the label.
FORK=(EVENT=pull_request HEAD_REPO=fork/r BASE_REPO=o/r)
want false "fork PR that is e2e-relevant does not run unlabelled" "${FORK[@]}" STUB_FULL=0
want false "fork PR touching the e2e's files does not run unlabelled" "${FORK[@]}" STUB_FORCED=0
want true "fork PR with the e2e label runs" "${FORK[@]}" 'LABELS=["e2e"]'
want true "same-repo PR (head repo = base repo) still runs unlabelled when relevant" EVENT=pull_request HEAD_REPO=o/r BASE_REPO=o/r STUB_FULL=0
want true "queue entry in the middle of the fork logic is unaffected" "${MG[@]}" STUB_FULL=0

[[ $fail -eq 0 ]] && echo "e2e-decide tests passed"
exit "$fail"
