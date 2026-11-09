#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests for hack/pr-updater.sh: the updater only touches a ready, green pull request behind
# main (old issue 596). Nothing here talks to GitHub. CI runs this file in the `quick checks` job.
# shellcheck disable=SC2016,SC2329,SC2034
# (SC2016 jq in single quotes, SC2329 stubs are called indirectly, SC2034 variables are read by the
# sourced script.)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }
eq() { if [[ $2 == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }
unset GITHUB_ACTIONS

export PR_UPDATER_LIB_ONLY=1
# shellcheck source=/dev/null
source "$here/pr-updater.sh"
unset PR_UPDATER_LIB_ONLY
ut_ok='[{"__typename":"CheckRun","name":"a","status":"COMPLETED","conclusion":"SUCCESS"},{"__typename":"CheckRun","name":"b","status":"COMPLETED","conclusion":"SKIPPED"},{"__typename":"StatusContext","context":"c","state":"SUCCESS"}]'
ut_red='[{"__typename":"CheckRun","name":"a","status":"COMPLETED","conclusion":"FAILURE"}]'
ut_run='[{"__typename":"CheckRun","name":"a","status":"IN_PROGRESS","conclusion":null}]'
ut_pr() { # NUMBER CREATED DRAFT MERGESTATE AUTOMERGE ROLLUP
  jq -n --argjson n "$1" --arg c "$2" --argjson d "$3" --arg m "$4" --argjson a "$5" --argjson r "$6" \
    '{number: $n, createdAt: $c, isDraft: $d, mergeStateStatus: $m, autoMergeRequest: (if $a then {enabledAt: $c} else null end), statusCheckRollup: $r}'
}
ut_pr_q() { ut_pr "$@" | jq '. + {isInMergeQueue: true}'; }
ut_req='["a","b","c"]'
ut() { jq -s . | update_target "$ut_req"; }
cr() { # NAME STATUS CONCLUSION STARTED -> a CheckRun
  jq -nc --arg n "$1" --arg s "$2" --arg c "$3" --arg t "$4" '{__typename:"CheckRun",name:$n,status:$s,conclusion:(if $c=="" then null else $c end),startedAt:(if $t=="" then null else $t end)}'; }
rollup() { jq -sc . ; }
req_all="$(cr a COMPLETED SUCCESS 2026-01-01T10:00:00Z; cr b COMPLETED SUCCESS 2026-01-01T10:00:00Z; jq -nc '{__typename:"StatusContext",context:"c",state:"SUCCESS",createdAt:"2026-01-01T10:00:00Z"}')"
r_oldfail_newok="$({ cr a COMPLETED FAILURE 2026-01-01T09:00:00Z; echo "$req_all"; } | rollup)"
r_newfail="$({ cr a COMPLETED FAILURE 2026-01-01T11:00:00Z; echo "$req_all"; } | rollup)"
r_cancel_then_ok="$({ cr b COMPLETED CANCELLED 2026-01-01T08:00:00Z; echo "$req_all"; } | rollup)"
r_noise="$({ echo "$req_all"; cr flavors COMPLETED FAILURE 2026-01-01T12:00:00Z; cr updater IN_PROGRESS "" 2026-01-01T12:00:00Z; cr codecov COMPLETED FAILURE 2026-01-01T12:00:00Z; } | rollup)"
r_pending_req="$({ echo "$req_all"; cr a IN_PROGRESS "" 2026-01-01T11:00:00Z; } | rollup)"
r_queued_req="$({ echo "$req_all"; cr a QUEUED "" ""; } | rollup)"
r_missing_req="$(cr a COMPLETED SUCCESS 2026-01-01T10:00:00Z | rollup)"
eq "updater: an older failure of a name with a newer success is eligible" "$(ut_pr 5 2026-01-02 false BEHIND true "$r_oldfail_newok" | ut)" 5
eq "updater: an older cancelled run with a newer success is eligible" "$(ut_pr 5 2026-01-02 false BEHIND true "$r_cancel_then_ok" | ut)" 5
eq "updater: a newer failure is not eligible" "$(ut_pr 5 2026-01-02 false BEHIND true "$r_newfail" | ut)" ""
eq "updater: red advisory, third-party and the updater's own run do not block" "$(ut_pr 5 2026-01-02 false BEHIND true "$r_noise" | ut)" 5
eq "updater: a pending required check blocks" "$(ut_pr 5 2026-01-02 false BEHIND true "$r_pending_req" | ut)" ""
eq "updater: a queued required rerun blocks" "$(ut_pr 5 2026-01-02 false BEHIND true "$r_queued_req" | ut)" ""
eq "updater: a required check that has not reported blocks" "$(ut_pr 5 2026-01-02 false BEHIND true "$r_missing_req" | ut)" ""
eq "updater: ready green behind pull request is updated" "$(ut_pr 5 2026-01-02 false BEHIND true "$ut_ok" | ut)" 5
eq "updater: a draft is left alone" "$(ut_pr 5 2026-01-02 true BEHIND true "$ut_ok" | ut)" ""
eq "updater: a red head is left alone" "$(ut_pr 5 2026-01-02 false BEHIND true "$ut_red" | ut)" ""
eq "updater: a running head is left alone" "$(ut_pr 5 2026-01-02 false BEHIND true "$ut_run" | ut)" ""
eq "updater: no checks at all is not green" "$(ut_pr 5 2026-01-02 false BEHIND true '[]' | ut)" ""
eq "updater: up to date is left alone" "$(ut_pr 5 2026-01-02 false CLEAN true "$ut_ok" | ut)" ""
eq "updater: a conflict is left to the needs-update label" "$(ut_pr 5 2026-01-02 false DIRTY true "$ut_ok" | ut)" ""
eq "updater: a pull request in the merge queue is left alone" "$(ut_pr_q 5 2026-01-02 false BEHIND true "$ut_ok" | ut)" ""
eq "updater: a pull request outside the queue is still updated" "$(ut_pr 5 2026-01-02 false BEHIND true "$ut_ok" | jq '. + {isInMergeQueue: false}' | ut)" 5
eq "updater: without auto-merge is left alone" "$(ut_pr 5 2026-01-02 false BEHIND false "$ut_ok" | ut)" ""
eq "updater: the oldest eligible one wins, skipping a red older one" \
  "$({ ut_pr 3 2026-01-01 false BEHIND true "$ut_red"; ut_pr 9 2026-01-03 false BEHIND true "$ut_ok"; ut_pr 7 2026-01-02 false BEHIND true "$ut_ok"; } | ut)" 7
rq=$(required_contexts "$here/ci-required.txt")
eq "required_contexts: reads required names, not advisory" "$(jq -r 'IN(.[]; "claim") and IN(.[]; "go") and (IN(.[]; "flavors") | not) and IN(.[]; "e2e result")' <<<"$rq")" true
LOG=$(mktemp "${TMPDIR:-/tmp}/pr_updater_test.XXXXXX")
trap 'rm -f "$LOG"' EXIT
up_rc=0
(
  GITHUB_ACTIONS=true; REPO=acme/koinon
  gh() { case $* in *rules/branches*) echo false ;; *"/labels"*) return 1 ;; *update-branch*) echo "update-branch" >>"$LOG" ;; esac; }
  server_now() { echo 1000; }; required_contexts() { echo '[]'; }
  open_prs() { echo '[{"number":5,"autoMergeRequest":{},"mergeStateStatus":"DIRTY","labels":[]},{"number":6,"autoMergeRequest":{},"mergeStateStatus":"BEHIND","labels":[]}]'; }
  update_target() { echo 6; }
  cmd_update_oldest
) >/dev/null 2>&1 || up_rc=$?
eq "updater: a failing needs-update label does not fail the job" "$up_rc" 0
eq "updater: a failing needs-update label still reaches update-branch" "$(grep -c update-branch "$LOG")" 1

exit "$fail"
