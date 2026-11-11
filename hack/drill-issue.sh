#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The nightly restore drill (the `drill` job of e2e-kind.yml, ADR 0109) was red. Record it in ONE issue:
# when an open `drill-red` issue exists, comment on it; otherwise open one. The drill is advisory
# (continue-on-error, never a merge check), so without this a red night is visible only in the Actions tab (#572).
#
#   hack/drill-issue.sh [--dry-run]
#
# Environment: GH (the gh command, default gh), GH_REPO, GITHUB_RUN_URL, DRILL_COMMIT (the commit the drill
# tested; default GITHUB_SHA), GITHUB_EVENT_NAME,
# GITHUB_REF_NAME. --dry-run calls no GitHub API and prints what it would do.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

dry=0
while [[ $# -gt 0 ]]; do
  case $1 in
  --dry-run) dry=1 ;;
  *) echo "drill-issue: unknown argument $1" >&2; exit 2 ;;
  esac
  shift
done
GH=${GH:-gh}
label=drill-red
run_url=${GITHUB_RUN_URL:-unknown}
sha=${DRILL_COMMIT:-${GITHUB_SHA:-unknown}}
event=${GITHUB_EVENT_NAME:-unknown}
ref=${GITHUB_REF_NAME:-unknown}

body="The restore drill was red on \`${ref}\` (${sha:0:9}, event \`${event}\`).

Run: ${run_url}

Read the \`drill-logs\` artifact of that run: \`drill-result.json\` has one verdict per check, and the job summary lists them. A check that fails only on a declared known gap (label \`known-gap\`, message \`known gap: #N\`) does not make the drill red. The drill is advisory: it does not block merges, but a restore that no longer works is a release blocker (ADR 0109).

This issue stays open while the drill is red. Close it when a nightly run is green; a later red run comments here."

if [[ $dry == 1 ]]; then
  echo "would look for an open issue with label $label"
  echo "--- comment on the open issue, or open one (label $label) ---"
  echo "$body"
  exit 0
fi

open_issue=$("$GH" issue list --label "$label" --state open --limit 1 --json number --jq '.[0].number // empty')
if [[ -n $open_issue ]]; then
  "$GH" issue comment "$open_issue" --body "$body" >/dev/null
  echo "commented on #$open_issue"
else
  "$GH" label create "$label" --color B60205 --description "The nightly restore drill is red" >/dev/null 2>&1 || true
  issue=$("$GH" issue create --title "Restore drill red on ${ref} (${sha:0:9})" --body "$body" --label "$label")
  echo "opened $issue"
fi
