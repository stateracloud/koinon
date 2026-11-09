#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests for hack/claim.sh (ADR 0082, ADR 0104) and hack/pr-updater.sh. Nothing here talks to
# GitHub: the GitHub-facing functions are stubbed. CI runs this file in the `quick checks` job.
# shellcheck disable=SC2016,SC2329,SC2030,SC2031,SC2034
# (SC2016 jq and bash -c in single quotes, SC2329 stubs are called indirectly, SC2030/SC2031 subshell
# assignments are intended, SC2034 variables are read by the sourced script.)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
claim=$here/claim.sh
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }
eq() { if [[ $2 == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }
yn() { if "$@"; then echo y; else echo n; fi; }
unset KOINON_CLAIM_HUMAN KOINON_CLAIM_REPO GITHUB_ACTIONS

# ---- hack/pr-updater.sh: the updater only touches a ready, green pull request behind main (old issue 596) ----
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
LOG=$(mktemp "${TMPDIR:-/tmp}/claim_upd.XXXXXX")
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
rm -f "$LOG"

# ---- hack/claim.sh: pure functions ----
export CLAIM_LIB_ONLY=1
# shellcheck source=/dev/null
source "$claim"
unset CLAIM_LIB_ONLY

msg=$(claim_format 12 held "alice#main" alice 34 $'two\nlines')
eq "format subject" "$(head -1 <<<"$msg")" "claim #12: held"
eq "format has no lease" "$(grep -c lease_until <<<"$msg")" 0
parsed=$(claim_parse "$msg")
eq "parse" "$(jq -c . <<<"$parsed")" '{"state":"held","holder":"alice#main","human":"alice","pr":"34","note":"two lines","released_by":""}'
eq "note cannot inject a field" "$(jq -r .state <<<"$(claim_parse "$(claim_format 1 held h u "" $'x\nstate: released')")")" held
eq "parse garbage" "$(claim_parse hello)" '{"state":"","holder":"","human":"","pr":"","note":"","released_by":""}'
rel=$(claim_format 12 released "alice#l7" alice "" "done" "alice#l7")
eq "released_by is written and parsed" "$(jq -r .released_by <<<"$(claim_parse "$rel")")" "alice#l7"
eq "state stays the last line" "$(tail -1 <<<"$rel")" "state: released"
eq "a held claim has no released_by line" "$(grep -c released_by <<<"$(claim_format 1 held h u "" n)")" 0
old=$'claim #58: held\n\nholder: procoduck/MacBookPro/2d02-sess#l58\nhuman: procoduck\nlease_until: 2026-11-09T20:48:57Z\npr: \nnote: \nstate: held\n'
eq "a claim from before ADR 0104 parses, its lease ignored" "$(jq -c '[.state, .holder, .human]' <<<"$(claim_parse "$old")")" '["held","procoduck/MacBookPro/2d02-sess#l58","procoduck"]'

eq "holder login" "$(holder_login alice#l1)" alice
eq "holder lane" "$(holder_lane alice#l1)" l1
eq "old holder login" "$(holder_login procoduck/MacBookPro/s1#l58)" procoduck
eq "old holder lane" "$(holder_lane procoduck/MacBookPro/s1#l58)" l58
eq "old holder without a lane is in lane main" "$(holder_lane procoduck/MacBookPro/s1)" main
eq "same holder: login case and format do not matter" "$(yn same_holder Alice#l1 alice/h/s9#l1)" y
eq "same holder: a restart keeps the holder (no session in it)" "$(yn same_holder alice#main alice#main)" y
eq "different lane" "$(yn same_holder alice#l1 alice#l2)" n
eq "different login" "$(yn same_holder alice#l1 bob#l1)" n
eq "valid lane" "$(yn valid_lane l226)" y
eq "bad lane" "$(yn valid_lane -x)" n
eq "lost race: exists" "$(yn is_lost_race 'Reference already exists (HTTP 422)')" y
eq "lost race: not ff" "$(yn is_lost_race 'Update is not a fast forward (HTTP 422)')" y
eq "other 422 is an error" "$(yn is_lost_race 'Repository rule violations found (HTTP 422)')" n
eq "closing issues" "$(closing_issues $'Closes #5 and fixes #7.\nresolved: #5, see #9' | paste -sd, -)" "5,7"
eq "verdict: released is free" "$(claim_verdict released bob alice)" free
eq "verdict: the author's own claim (any case)" "$(claim_verdict held Alice alice)" own
eq "verdict: another human's held claim conflicts" "$(claim_verdict held bob alice)" conflict

tmp=$(mktemp "${TMPDIR:-/tmp}/claim_test.XXXXXX")
trap 'rm -f "$tmp" "$tmp".*' EXIT
printf '# Sensitive paths.\n/.github/ @a @b\n/AGENTS.md @a\n/internal/auth/ @a\n' >"$tmp.co"
eq "sensitive paths" "$(sensitive_paths "$tmp.co" | paste -sd, -)" ".github/**,AGENTS.md,internal/auth/**"
printf '* @a\n/x/ @a\n' >"$tmp.co"
eq "no sensitive section" "$(sensitive_paths "$tmp.co")" ""
eq "sensitive-paths command runs offline" "$(KOINON_CLAIM_CODEOWNERS="$here/../.github/CODEOWNERS" bash "$claim" sensitive-paths | head -1)" "$(sensitive_paths "$here/../.github/CODEOWNERS" | head -1)"
eq "next adr number" "$(next_adr_number 0081 0082 0090 0003)" "0091"
eq "next adr number from none" "$(next_adr_number)" "0001"
adr_page='[{"data":{"repository":{"pullRequests":{"nodes":[
 {"number":136,"files":{"pageInfo":{"hasNextPage":false},"nodes":[{"path":"docs/adr/0093-koinon-core-opencode-plugin.md","changeType":"ADDED"},{"path":"docs/adr/README.md","changeType":"MODIFIED"}]}},
 {"number":137,"files":{"pageInfo":{"hasNextPage":false},"nodes":[{"path":"docs/adr/0070-old.md","changeType":"MODIFIED"},{"path":"docs/adr/0071-gone.md","changeType":"DELETED"},{"path":"docs/adr/0095-x.md","changeType":"ADDED"},{"path":"docs/other/0099-nope.md","changeType":"ADDED"},{"path":"docs/adr/0097-renamed.md","changeType":"RENAMED"}]}}]}}}}]'
eq "open pull requests: ADDED and RENAMED docs/adr files count" "$(jq -r "$ADR_PR_JQ" <<<"$adr_page" | paste -sd, -)" "0093,0095,0097"

# The fixed failure texts of the claim check, pinned.
eq "conflict text: issue" "$(conflict_issue_text 159 procoduck outsider1)" \
  "::error::#159 is claimed by @procoduck; this pull request is by @outsider1. Ask @procoduck to release the claim (hack/claim.sh release 159) or to close their pull request, then re-run this check."
eq "conflict text: ADR number" "$(conflict_adr_text docs/adr/0095-x.md 0095 procoduck)" \
  "::error::docs/adr/0095-x.md uses ADR number 0095, which @procoduck reserved (claim/adr-0095). Take a free number with hack/claim.sh adr and rename the file."

# JQ_PR_CLOSERS: closingIssuesReferences of this repository plus the body's closing keywords.
closers_page='[{"data":{"repository":{"pullRequests":{"nodes":[
 {"number":200,"body":"Closes #5\nfixes: #6","author":{"login":"Carol"},"closingIssuesReferences":{"nodes":[{"number":5,"repository":{"nameWithOwner":"acme/koinon"}},{"number":9,"repository":{"nameWithOwner":"acme/other"}}]}},
 {"number":201,"body":null,"author":{"login":"alice"},"closingIssuesReferences":{"nodes":[{"number":7,"repository":{"nameWithOwner":"acme/koinon"}}]}},
 {"number":202,"body":"see #8","author":null,"closingIssuesReferences":{"nodes":[]}}]}}}}]'
CL=$(jq -c --arg repo acme/koinon "$JQ_PR_CLOSERS" <<<"$closers_page")
eq "closers: references and body keywords, this repository only" "$(jq -c '[.[] | [.number, .author, .issues]]' <<<"$CL")" '[[200,"Carol",[5,6]],[201,"alice",[7]],[202,"",[]]]'
eq "pr conflicts: another author" "$(pr_conflicts "$CL" 5 alice)" "#200 by @Carol"
eq "pr conflicts: the caller's own pull request is none" "$(pr_conflicts "$CL" 7 Alice)" ""
eq "own pr: linked" "$(own_pr "$CL" 7 alice)" 201
eq "own pr: never another author's" "$(own_pr "$CL" 5 alice)" ""
eq "assignees: others" "$(other_assignees '{"assignees":["bob","Alice"]}' alice)" "@bob"
eq "assignees: none" "$(other_assignees '{}' alice)" ""

# JQ_NEXT: skip held claims, open closing pull requests and other assignees; In Progress / In Review
# with nothing holding the issue count as Ready.
item() { # N STATUS ASSIGNEES_JSON [LABELS_JSON]
  jq -nc --argjson n "$1" --arg s "$2" --argjson a "$3" --argjson l "${4:-[\"agent-ok\"]}" \
    '{status:{name:$s},priority:{name:"P1"},component:null,content:{number:$n,title:"t\($n)",state:"OPEN",createdAt:"2026-01-0\($n % 9 + 1)T00:00:00Z",repository:{nameWithOwner:"acme/koinon"},milestone:{title:"m1"},labels:{nodes:($l|map({name:.}))},assignees:{nodes:($a|map({login:.}))}}}'
}
board=$(jq -sc '[{data:{organization:{projectV2:{items:{nodes:.}}}}}]' < <(
  item 1 Ready '[]'; item 2 Ready '[]'; item 3 Ready '[]'; item 4 Ready '["bob"]'; item 5 Ready '["Alice"]'
  item 6 "In Progress" '[]'; item 7 "In Review" '[]'; item 8 Backlog '[]'; item 9 Ready '[]' '[]'))
nx() { jq -r --arg repo acme/koinon --arg ms m1 --arg label "" --argjson held '[2]' --argjson prs '[3]' --arg human alice "$JQ_RANK$JQ_NEXT"' | map(.n) | join(",")' <<<"$board"; }
eq "next: skips held, PR-closed, other-assigned, Backlog and non-agent-ok issues" "$(nx)" "1,5,6,7"
ranked=$(jq -nc "$JQ_RANK"' [{p:"P2",at:"1"},{p:"",at:"0"},{p:"P0",at:"9"},{p:"P2",at:"0"}] | sort_by([(.p|rank),.at]) | map(.p+.at) | join(",")')
eq "priority then age" "$ranked" '"P09,P20,P21,0"'

# ---- the surface: the header lists exactly the commands main dispatches ----
header=$(bash "$claim" help | grep -oE '^  claim\.sh [a-z-]+' | awk '{print $2}'; bash "$claim" help | grep -oE '\| mine' | sed 's/| //')
dispatch=$(sed -n '/^main() {/,/^}/p' "$claim" | grep -oE '^    [a-z-]+\)' | tr -d ' )')
eq "header commands = dispatched commands (plus help)" "$(sort -u <<<"$header" | paste -sd, -)" "$({ echo help; echo "$dispatch"; } | sort -u | paste -sd, -)"
for c in beat adopt review sweep comment limits wait-release update-oldest; do
  rc=0; bash "$claim" "$c" >/dev/null 2>&1 || rc=$?
  eq "removed command $c is gone (exit 1)" "$rc" 1
done
for v in LEASE GRACE KOINON_AGENT_SESSION CLAUDE_CODE_SESSION_ID 'claim.sh beat' lease_until takeover outsider claim:stale; do
  if sed -n '1,/^# Needs bash 5/p' "$claim" | grep -q -- "$v"; then bad "header does not mention $v"; else ok "header does not mention $v"; fi
done

# ---- commands, with GitHub stubbed: FIX is the claim refs as JSON, WRITES logs the writes ----
stub_github() {
  REPO=acme/koinon; OWNER=acme; NAME=koinon
  human_login() { echo "${ME:-alice}"; }
  list_claims() { echo "$FIX"; }
  load_claim() { C=$(jq -c --arg n "$1" '.[] | select((.issue | tostring) == $n)' <<<"$FIX"); [[ -n $C ]] || return 1; }
  issue_json() { echo "${ISSUE_FIX:-{\"state\":\"open\",\"pull\":false,\"labels\":[\"agent-ok\"],\"assignees\":[]\}}"; }
  open_pr_closers() { echo "${CLOSERS_FIX:-[]}"; }
  write_claim() { # logs the write and, unless LOSE names the ref, stores it in FIX
    echo "write $1 $2 holder=$3 pr=$5 by=${8:-}" >>"$WRITES"
    [[ ${LOSE:-} != "$1" ]] || return 1
    FIX=$(jq -c --arg n "$1" --argjson c "$(cl "${1#adr-}" "$2" "$3" "$4" "$5")" '[.[] | select((.issue | tostring) != $n)] + [$c]' <<<"$FIX")
  }
  assign() { echo "assign $1 $2" >>"$WRITES"; }
  unassign() { echo "unassign $1 $2" >>"$WRITES"; }
  pr_meta() { echo "$PR_FIX"; }
  pr_added_adr_files() { echo "${ADR_FILES:-}"; }
  gh() { case $* in *issues/*--jq\ .state*) echo open ;; *) echo 1 ;; esac; }
}
WRITES=$tmp.writes
# run ARGS...: main's argument parsing and dispatch, offline; prints output, then rc=N.
run() {
  : >"$WRITES"
  local rc=0 out
  out=$( (stub_github; CLAIM_LIB_ONLY=1; init_repo() { :; }; main "$@") 2>&1) || rc=$?
  printf '%s\nrc=%s\n' "$out" "$rc"
}
rc_of() { sed -n 's/^rc=//p' <<<"$1"; }
cl() { jq -nc --argjson n "$1" --arg s "$2" --arg h "$3" --arg u "$4" --arg p "${5:-}" '{issue:$n,state:$s,holder:$h,human:$u,pr:$p,note:"",released_by:"",sha:"sha\($n)"}'; }
fix() { jq -sc . ; }

FIX=$(fix </dev/null)
r=$(run take 10)
eq "take: a free issue" "$(rc_of "$r")" 0
eq "take: writes holder login#main and assigns" "$(paste -sd';' "$WRITES")" "write 10 held holder=alice#main pr= by=;assign 10 alice"
r=$(run take 10 --lane l10)
eq "take --lane: the holder carries the lane" "$(head -1 "$WRITES")" "write 10 held holder=alice#l10 pr= by="
FIX=$(cl 10 held alice#l10 alice | fix)
r=$(run take 10 --lane l10)
eq "take: already yours in this lane is exit 0" "$(rc_of "$r")" 0
eq "take: already yours writes nothing" "$(wc -l <"$WRITES" | tr -d ' ')" 0
FIX=$(cl 10 held alice/MacBookPro/s1#l10 alice | fix)
r=$(run take 10 --lane l10)
eq "take: an old-format claim of the same login and lane is yours" "$(rc_of "$r")" 0
FIX=$(cl 10 held alice#l9 alice | fix)
r=$(run take 10 --lane l10)
eq "take: held by another lane of yours is refused" "$(rc_of "$r")" 2
eq "take: names the other lane" "$(grep -c 'your lane l9' <<<"$r")" 1
FIX=$(cl 10 held bob#main bob | fix)
r=$(run take 10)
eq "take: held by another human is refused" "$(rc_of "$r")" 2
FIX=$(cl 10 released bob#main bob | fix)
r=$(run take 10)
eq "take: a released claim is taken" "$(rc_of "$r")" 0
FIX=$(cl 11 held alice#main alice | fix)
r=$(run take 10)
eq "take: one claim per lane" "$(rc_of "$r")" 2
r=$(run take 10 --lane l10)
eq "take: another lane is free" "$(rc_of "$r")" 0
FIX=$(for i in 1 2 3 4 5 6 7 8; do cl "$i" held "alice#l$i" alice; done | fix)
r=$(run take 10 --lane l10)
eq "take: eight claims per human" "$(rc_of "$r")" 2
FIX=$(fix </dev/null)
r=$(CLOSERS_FIX='[{"number":50,"author":"bob","issues":[10]}]' run take 10)
eq "take: another author's open pull request refuses" "$(rc_of "$r")" 2
r=$(CLOSERS_FIX='[{"number":51,"author":"Alice","issues":[10]}]' run take 10)
eq "take: your own open pull request is linked" "$(head -1 "$WRITES")" "write 10 held holder=alice#main pr=51 by="
r=$(ISSUE_FIX='{"state":"open","pull":false,"labels":["agent-ok"],"assignees":["bob"]}' run take 10)
eq "take: someone else assigned refuses" "$(rc_of "$r")" 2
r=$(ISSUE_FIX='{"state":"closed","pull":false,"labels":[],"assignees":[]}' run take 10)
eq "take: a closed issue refuses" "$(rc_of "$r")" 2
r=$(LOSE=10 run take 10)
eq "take: a lost race refuses" "$(rc_of "$r")" 2
FIX=$(cl 12 held bob#main bob | fix)
r=$(run take 10 11 12)
eq "take: several issues are all or nothing" "$(rc_of "$r")" 2
eq "take: the taken ones are rolled back" "$(grep -c '^write 1[01] released' "$WRITES")" 2
r=$(run take x)
eq "take: needs numbers" "$(rc_of "$r")" 1
r=$(run take 10 --lane BAD)
eq "take: a bad lane is an error" "$(rc_of "$r")" 1

FIX=$(cl 10 held alice#l10 alice 51 | fix)
r=$(run release 10 --lane l10 --note "back to Ready")
eq "release: own claim" "$(rc_of "$r")" 0
eq "release: writes released, keeps holder and pr, unassigns" "$(paste -sd';' "$WRITES")" "write 10 released holder=alice#l10 pr=51 by=alice#l10;unassign 10 alice"
r=$(run release 10)
eq "release: another lane needs --force" "$(rc_of "$r")" 3
r=$(run release 10 --force)
eq "release --force: another lane of the same human" "$(rc_of "$r")" 0
r=$(ME=bob run release 10 --force)
eq "release: another human's claim is not yours, even with --force" "$(rc_of "$r")" 3
FIX=$(cl 10 held alice/MacBookPro/s1#l58 alice | fix)
r=$(run release 10)
eq "release: an old-format claim of your login from any lane" "$(rc_of "$r")" 0
FIX=$(cl 10 released alice#main alice | fix)
r=$(run release 10)
eq "release: already released is exit 0" "$(rc_of "$r")" 0
eq "release: already released writes nothing" "$(wc -l <"$WRITES" | tr -d ' ')" 0
FIX=$(fix </dev/null)
r=$(run release 10)
eq "release: no claim is exit 3" "$(rc_of "$r")" 3
FIX=$(cl 10 held alice#main alice | fix)
r=$(run block 10 9)
eq "block: adds the dependency and releases" "$(rc_of "$r"),$(grep -c 'write 10 released' "$WRITES")" "0,1"

FIX=$({ cl 10 held alice#l10 alice 51; cl 11 held bob#main bob; cl 12 released alice#main alice; cl 13 held alice/h/s#l13 Alice; } | fix)
r=$(run mine)
eq "mine: your held claims, lanes and old format included" "$(grep '^#' <<<"$r" | paste -sd';' -)" $'#10\tl10\tpr 51\talice#l10;#13\tl13\tpr \talice/h/s#l13'
r=$(run mine --lane l13)
eq "mine --lane: only that lane" "$(grep -c '^#' <<<"$r")" 1
r=$(run show 10)
eq "show: the lane" "$(grep -c 'lane:   l10' <<<"$r")" 1
r=$(run show 99)
eq "show: no claim" "$(head -1 <<<"$r")" "#99: no claim"

# check-pr: only another human's held claim or ADR reservation fails.
FIX=$({ cl 10 held bob#main bob; cl 11 released bob#main bob; cl 12 held alice#l12 alice; } | fix)
r=$(PR_FIX='{"author":"alice","body":"Closes #11","merged":false,"closers":[12]}' run check-pr 5)
eq "check-pr: released and own claims pass" "$(rc_of "$r")" 0
r=$(PR_FIX='{"author":"alice","body":"Closes #10","merged":false,"closers":[]}' run check-pr 5)
eq "check-pr: another human's held claim fails (no lease: until released)" "$(rc_of "$r")" 1
eq "check-pr: with the pinned text" "$(grep -c '^::error::#10 is claimed by @bob' <<<"$r")" 1
r=$(PR_FIX='{"author":"carol","body":"","merged":false,"closers":[]}' run check-pr 5)
eq "check-pr: no Closes #N passes" "$(rc_of "$r")" 0
adrfix() { jq -nc --arg s "$1" --arg u "$2" '{issue:"adr-0200",state:$s,holder:"\($u)#main",human:$u,pr:"",note:"",released_by:"",sha:"x"}'; }
FIX=$(adrfix held bob | fix)
r=$(ADR_FILES=docs/adr/0200-x.md PR_FIX='{"author":"alice","body":"","merged":false,"closers":[]}' run check-pr 5)
eq "check-pr: an old reservation by another human fails, its lease ignored" "$(rc_of "$r")" 1
FIX=$(adrfix reserved alice | fix)
r=$(ADR_FILES=docs/adr/0200-x.md PR_FIX='{"author":"alice","body":"","merged":false,"closers":[]}' run check-pr 5)
eq "check-pr: your own reservation passes" "$(rc_of "$r")" 0
FIX=$(fix </dev/null)
r=$(ADR_FILES=docs/adr/0200-x.md PR_FIX='{"author":"alice","body":"","merged":false,"closers":[]}' run check-pr 5)
eq "check-pr: an unreserved number passes" "$(rc_of "$r")" 0

# release-pr: the author's claims (or those pointing at the PR) are released; another human's are kept.
FIX=$({ cl 7 held alice#l7 alice; cl 8 held bob#main bob; cl 9 held bob#main bob 5; } | fix)
r=$(PR_FIX='{"author":"alice","body":"Closes #7, closes #8","merged":true,"closers":[9]}' run release-pr 5)
eq "release-pr: author's and pointing claims released" "$(grep '^write' "$WRITES" | awk '{print $2}' | paste -sd, -)" "7,9"
eq "release-pr: a merge does not unassign" "$(grep -c unassign "$WRITES")" 0
eq "release-pr: the note old beat loops recognise" "$(grep -c 'by=pr-close' "$WRITES")" 2
r=$(CLOSERS_FIX='[{"number":6,"author":"alice","issues":[7]}]' PR_FIX='{"author":"alice","body":"Closes #7","merged":false,"closers":[]}' run release-pr 5)
eq "release-pr: an unmerged close keeps a claim another open PR closes" "$(grep -c '^write' "$WRITES")" 0
r=$(PR_FIX='{"author":"alice","body":"Closes #7","merged":false,"closers":[]}' run release-pr 5)
eq "release-pr: an unmerged close releases and unassigns" "$(paste -sd';' "$WRITES")" "write 7 released holder=alice#l7 pr= by=pr-close;unassign 7 alice"

exit "$fail"
