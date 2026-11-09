#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests for hack/claim.sh (ADR 0082).
#
#   hack/claim_test.sh            offline unit tests of the parsing and formatting functions (no
#                                 network); this is the part CI may run.
#   KOINON_CLAIM_TEST_REPO=owner/scratch hack/claim_test.sh
#                                 also drives the real commands against a SCRATCH repository you own:
#                                 it opens and closes issues and creates claim refs there (claim refs
#                                 are never deleted). Never point it at stateracloud/koinon. Not run
#                                 in CI: CI must not mutate GitHub.
# The lane tests stub functions of the sourced script and run commands in subshells:
# SC2016 single quotes for bash -c, SC2329 stubs are called indirectly, SC2030/SC2031 subshell
# assignments are intended, SC2034 caps are read by the sourced script.
# shellcheck disable=SC2016,SC2329,SC2030,SC2031,SC2034

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
claim=$here/claim.sh
fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }
eq() { if [[ $2 == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }

# ---- offline unit tests ----
export CLAIM_LIB_ONLY=1
# shellcheck source=/dev/null
source "$claim"
unset CLAIM_LIB_ONLY

# update_target: the updater only touches a ready, green pull request that is behind main (old issue 596).
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
rq=$(required_contexts "$(dirname "$claim")/ci-required.txt")
eq "required_contexts: reads required names, not advisory" "$(jq -r 'IN(.[]; "claim") and IN(.[]; "go") and (IN(.[]; "flavors") | not) and IN(.[]; "e2e result")' <<<"$rq")" true

msg=$(claim_format 12 held "alice/mac/s1" alice 2026-10-30T16:00:00Z 34 $'two\nlines')
eq "format subject" "$(head -1 <<<"$msg")" "claim #12: held"
parsed=$(claim_parse "$msg")
eq "parse holder" "$(jq -r .holder <<<"$parsed")" "alice/mac/s1"
eq "parse human" "$(jq -r .human <<<"$parsed")" "alice"
eq "parse lease" "$(jq -r .lease_until <<<"$parsed")" "2026-10-30T16:00:00Z"
eq "parse pr" "$(jq -r .pr <<<"$parsed")" "34"
eq "parse note joins lines" "$(jq -r .note <<<"$parsed")" "two lines"
eq "parse state" "$(jq -r .state <<<"$parsed")" "held"
inj=$(claim_format 1 held h u 2026-10-30T16:00:00Z "" $'x\nstate: released')
eq "note cannot inject a field" "$(jq -r .state <<<"$(claim_parse "$inj")")" "held"
eq "parse garbage" "$(claim_parse 'hello')" '{"state":"","holder":"","human":"","lease_until":"","pr":"","note":"","released_by":""}'
rel=$(claim_format 12 released "alice/mac/s1" alice 2026-10-30T16:00:00Z "" "done")
eq "parse released" "$(jq -r .state <<<"$(claim_parse "$rel")")" "released"
eq "a release from before released_by parses with an empty field" "$(jq -r .released_by <<<"$(claim_parse "$rel")")" ""
rel2=$(claim_format 12 released "alice/mac/s1" alice 2026-10-30T16:00:00Z "" "done" "alice/mac/s1#x")
eq "released_by is written and parsed" "$(jq -r .released_by <<<"$(claim_parse "$rel2")")" "alice/mac/s1#x"
eq "released_by keeps state last" "$(tail -1 <<<"$rel2")" "state: released"
eq "released_by cannot inject a field" "$(jq -r .state <<<"$(claim_parse "$(claim_format 1 released h u t "" n $'x\nstate: held')")")" "released"
eq "a held claim has no released_by line" "$(grep -c released_by <<<"$(claim_format 1 held h u t "" n)")" "0"

e=$(iso_to_epoch 2026-10-30T16:00:00Z)
eq "iso round trip" "$(epoch_to_iso "$e")" "2026-10-30T16:00:00Z"
eq "http date" "$(http_date_to_epoch 'Fri, 30 Oct 2026 16:00:00 GMT')" "$e"

GRACE=900 # read by claim_status
export GRACE
eq "status active" "$(claim_status held 1000 900)" active
eq "status grace" "$(claim_status held 1000 1500)" grace
eq "status takeover" "$(claim_status held 1000 1901)" takeover
eq "status released" "$(claim_status released 1000 0)" released

tmp=$(mktemp "${TMPDIR:-/tmp}/claim_test.XXXXXX")
trap 'rm -f "$tmp"' EXIT
printf '# Sensitive paths.\n/.github/ @a @b\n/AGENTS.md @a\n/internal/auth/ @a\n' >"$tmp.co"
eq "sensitive paths" "$(sensitive_paths "$tmp.co" | paste -sd, -)" ".github/**,AGENTS.md,internal/auth/**"
printf '* @a\n/x/ @a\n' >"$tmp.co"
eq "no sensitive section" "$(sensitive_paths "$tmp.co")" ""
rm -f "$tmp.co"
eq "next adr number" "$(next_adr_number 0081 0082 0090 0003)" "0091"
eq "next adr number from none" "$(next_adr_number)" "0001"
# ADR 0094 decision 6: numbers that open pull requests ADD count, nothing else does.
adr_page='[{"data":{"repository":{"pullRequests":{"nodes":[
 {"number":136,"files":{"pageInfo":{"hasNextPage":false},"nodes":[{"path":"docs/adr/0093-koinon-core-opencode-plugin.md","changeType":"ADDED"},{"path":"docs/adr/README.md","changeType":"MODIFIED"}]}},
 {"number":137,"files":{"pageInfo":{"hasNextPage":false},"nodes":[{"path":"docs/adr/0070-old.md","changeType":"MODIFIED"},{"path":"docs/adr/0071-gone.md","changeType":"DELETED"},{"path":"docs/adr/0095-x.md","changeType":"ADDED"},{"path":"docs/other/0099-nope.md","changeType":"ADDED"},{"path":"docs/adr/0097-renamed.md","changeType":"RENAMED"}]}}]}}}}]'
eq "open pull requests: ADDED and RENAMED docs/adr files count" "$(jq -r "$ADR_PR_JQ" <<<"$adr_page" | paste -sd, -)" "0093,0095,0097"
# shellcheck disable=SC2046
eq "adr number counts an open pull request's file" "$(next_adr_number 0092 $(jq -r "$ADR_PR_JQ" <<<"$adr_page"))" "0098"
eq "adr number: no open pull request adds one" "$(jq -r "$ADR_PR_JQ" <<<'[{"data":{"repository":{"pullRequests":{"nodes":[]}}}}]' | wc -l | tr -d ' ')" "0"

# ADR 0094 decision 2: the exemptions are retired (the check fails only on a conflict).
for f in title_exempt is_dependabot pr_touches_code humans_list unclaimed_ok_by_human; do
  if declare -F "$f" >/dev/null; then bad "retired exemption $f is gone"; else ok "retired exemption $f is gone"; fi
done

# The fixed failure texts of the claim check, pinned: outsiders and agents match on them.
eq "conflict text: issue with a pull request" \
  "$(conflict_issue_text 159 procoduck 2026-11-07T10:00:00Z 160 outsider1 2026-11-07T12:00:00Z)" \
  "::error::#159 is claimed by @procoduck until 2026-11-07T10:00:00Z (pull request #160); this pull request is by @outsider1. Ask @procoduck on #159 to comment /release, or ask a maintainer. If the claim goes stale the sweeper frees it at 2026-11-07T12:00:00Z, and this check runs again when it does."
eq "conflict text: issue without a pull request" \
  "$(conflict_issue_text 7 ovirandunu 2026-11-07T10:00:00Z "" bob 2026-11-07T12:00:00Z)" \
  "::error::#7 is claimed by @ovirandunu until 2026-11-07T10:00:00Z (no pull request yet); this pull request is by @bob. Ask @ovirandunu on #7 to comment /release, or ask a maintainer. If the claim goes stale the sweeper frees it at 2026-11-07T12:00:00Z, and this check runs again when it does."
eq "conflict text: ADR number" \
  "$(conflict_adr_text docs/adr/0095-x.md 0095 procoduck 2026-11-08T10:00:00Z)" \
  "::error::docs/adr/0095-x.md uses ADR number 0095, which @procoduck reserved (claim/adr-0095) until 2026-11-08T10:00:00Z. Take a free number with hack/claim.sh adr (a maintainer can do it for you) and rename the file."

# claim_verdict: only someone else's live claim conflicts.
RELEASE_AFTER=7200
eq "verdict: released is free" "$(claim_verdict released bob alice 1000 900 none)" free
eq "verdict: the author's own claim (any case)" "$(claim_verdict held Alice alice 1000 900 none)" own
eq "verdict: someone else's live claim" "$(claim_verdict held bob alice 1000 900 none)" conflict
eq "verdict: within the release delay still conflicts" "$(claim_verdict held bob alice 8000 900 draft)" conflict
eq "verdict: past liveness + release delay is stale" "$(claim_verdict held bob alice 8101 900 draft)" stale
eq "verdict: a ready pull request is never swept" "$(claim_verdict held bob alice 99999 900 ready)" conflict
eq "verdict: a closed pull request releases the claim" "$(claim_verdict held bob alice 1000 900 closed)" stale

# Claim classes (ADR 0094 decision 1).
OUTSIDER_LEASE=1209600 HUMAN_LEASE=259200 LEASE=7200 GRACE=900
eq "class: agent" "$(holder_class alice/mac/s1#x)" agent
eq "class: take --human" "$(holder_class alice/mac/human)" person
eq "class: /claim by a maintainer" "$(holder_class alice/github/human)" person
eq "class: outsider" "$(holder_class carol/github/outsider)" outsider
eq "lease: outsider 14 days" "$(lease_for_holder carol/github/outsider)" 1209600
eq "lease: person by comment 3 days" "$(lease_for_holder alice/github/human)" 259200
eq "stale: outsiders at liveness" "$(stale_after carol/github/outsider)" 0
eq "stale: agents after the grace" "$(stale_after alice/mac/s1)" 900
eq "comment holder: person" "$(comment_holder alice person)" alice/github/human
eq "comment holder: outsider" "$(comment_holder carol outsider)" carol/github/outsider
for p in admin maintain write; do eq "permission $p is a person" "$(perm_class "$p")" person; done
for p in triage read none ""; do eq "permission '$p' is an outsider" "$(perm_class "$p")" outsider; done
eq "a session cannot be named outsider" "$(env -u CLAUDE_CODE_SESSION_ID KOINON_AGENT_SESSION=outsider KOINON_CLAIM_HUMAN=alice bash -c 'CLAIM_LIB_ONLY=1; source "$1"; my_holder' _ "$claim" 2>/dev/null || echo refused)" refused

# comment_command: only a first line of exactly /claim or /release.
eq "command: /claim" "$(comment_command '/claim')" claim
eq "command: /release with trailing space and CR" "$(comment_command $'/release  \r\nthanks')" release
eq "command: /claim then more lines" "$(comment_command $'/claim\nI will do it')" claim
eq "command: not on the first line" "$(comment_command $'hi\n/claim')" ""
eq "command: words after it" "$(comment_command '/claim this')" ""
eq "command: leading space" "$(comment_command ' /claim')" ""
eq "command: another command" "$(comment_command '/assign')" ""
eq "denied: 403" "$(is_denied 'Resource not accessible by integration (HTTP 403)' && echo y || echo n)" y
eq "denied: 401" "$(is_denied 'Bad credentials (HTTP 401)' && echo y || echo n)" y
eq "denied: a lost race is not" "$(is_denied 'Reference already exists (HTTP 422)' && echo y || echo n)" n
eq "denied: a ruleset refusal is not" "$(is_denied 'Repository rule violations found: you do not have permission to update this ref (HTTP 422)' && echo y || echo n)" n
eq "denied: a word in a title is not" "$(is_denied 'missing permission label' && echo y || echo n)" n
eq "fallback text" "$(text_fallback carol 42)" "@carol comment claims are not available for outside contributors yet; open a pull request that says Closes #42 instead. An open pull request already makes agents skip this issue."
eq "unassignable note" "$(text_unassignable carol 42)" 'GitHub did not allow assigning @carol (only collaborators, org members with read access and people who commented here can be assigned); the claim is recorded in the ref `claim/42`.'

# JQ_PR_CLOSERS: closingIssuesReferences of this repository plus the body's closing keywords.
closers_page='[{"data":{"repository":{"pullRequests":{"nodes":[
 {"number":200,"body":"Closes #5\nfixes: #6","author":{"login":"Carol"},"closingIssuesReferences":{"nodes":[{"number":5,"repository":{"nameWithOwner":"acme/koinon"}},{"number":9,"repository":{"nameWithOwner":"acme/other"}}]}},
 {"number":201,"body":null,"author":{"login":"alice"},"closingIssuesReferences":{"nodes":[{"number":7,"repository":{"nameWithOwner":"acme/koinon"}}]}},
 {"number":202,"body":"see #8","author":null,"closingIssuesReferences":{"nodes":[]}}]}}}}]'
CL=$(jq -c --arg repo acme/koinon "$JQ_PR_CLOSERS" <<<"$closers_page")
eq "closers: references and body keywords, this repository only" "$(jq -c '[.[] | [.number, .author, .issues]]' <<<"$CL")" '[[200,"Carol",[5,6]],[201,"alice",[7]],[202,"",[]]]'
eq "pr conflicts: another author" "$(pr_conflicts "$CL" 5 alice)" "#200 by @Carol"
eq "pr conflicts: the caller's own pull request is none" "$(pr_conflicts "$CL" 7 Alice)" ""
eq "pr conflicts: a takeover excuses the dead claim's human" "$(pr_conflicts "$CL" 5 alice carol)" ""
eq "own pr: linked" "$(own_pr "$CL" 7 alice)" 201
eq "own pr: never another author's" "$(own_pr "$CL" 5 alice)" ""
eq "assignees: others" "$(other_assignees '{"assignees":["bob","Alice"]}' alice)" "@bob"
eq "assignees: only the caller's human" "$(other_assignees '{"assignees":["ALICE"]}' alice)" ""
eq "assignees: a takeover excuses the dead claim's human" "$(other_assignees '{"assignees":["bob"]}' alice bob)" ""
eq "assignees: none" "$(other_assignees '{}' alice)" ""

# JQ_NEXT (ADR 0094 decision 5): skip held claims, open closing pull requests and other assignees;
# In Progress / In Review with nothing holding the issue count as Ready (decision 3).
item() { # N STATUS ASSIGNEES_JSON [LABELS_JSON]
  jq -nc --argjson n "$1" --arg s "$2" --argjson a "$3" --argjson l "${4:-[\"agent-ok\"]}" \
    '{status:{name:$s},priority:{name:"P1"},component:null,content:{number:$n,title:"t\($n)",state:"OPEN",createdAt:"2026-01-0\($n % 9 + 1)T00:00:00Z",repository:{nameWithOwner:"acme/koinon"},milestone:{title:"m1"},labels:{nodes:($l|map({name:.}))},assignees:{nodes:($a|map({login:.}))}}}'
}
board=$(jq -sc '[{data:{organization:{projectV2:{items:{nodes:.}}}}}]' < <(
  item 1 Ready '[]'; item 2 Ready '[]'; item 3 Ready '[]'; item 4 Ready '["bob"]'; item 5 Ready '["Alice"]'
  item 6 "In Progress" '[]'; item 7 "In Review" '[]'; item 8 Backlog '[]'; item 9 Ready '[]' '[]'))
nx() { jq -r --arg repo acme/koinon --arg ms m1 --arg label "" --argjson held '[2]' --argjson prs '[3]' --arg human alice "$JQ_RANK$JQ_NEXT"' | map(.n) | join(",")' <<<"$board"; }
eq "next: skips held, PR-closed, other-assigned, Backlog and non-agent-ok issues" "$(nx)" "1,5,6,7"

eq "same holder ignores the host" "$(same_holder a/h1/s1 a/h2/s1 && echo y || echo n)" y
eq "different session" "$(same_holder a/h1/s1 a/h1/s2 && echo y || echo n)" n
eq "different login" "$(same_holder a/h1/s1 b/h1/s1 && echo y || echo n)" n
eq "lease parse ok" "$(parse_lease 2026-10-30T16:00:00Z)" "$(iso_to_epoch 2026-10-30T16:00:00Z)"
eq "lease parse rejects junk" "$(parse_lease 'tomorrow' || echo bad)" bad
export LEASE=7200 HUMAN_LEASE=259200
eq "agent lease" "$(lease_for_holder a/h/s1)" 7200
eq "human lease" "$(lease_for_holder a/h/human)" 259200
eq "lost race: exists" "$(is_lost_race 'Reference already exists (HTTP 422)' && echo y || echo n)" y
eq "lost race: not ff" "$(is_lost_race 'Update is not a fast forward (HTTP 422)' && echo y || echo n)" y
eq "lost race: cannot be updated" "$(is_lost_race 'Reference cannot be updated (HTTP 422)' && echo y || echo n)" y
eq "other 422 is an error" "$(is_lost_race 'Repository rule violations found (HTTP 422)' && echo y || echo n)" n
eq "closing issues" "$(closing_issues $'Closes #5 and fixes #7.\nresolved: #5, see #9' | paste -sd, -)" "5,7"

ranked=$(jq -nc "$JQ_RANK"' [{p:"P2",at:"1"},{p:"",at:"0"},{p:"P0",at:"9"},{p:"P2",at:"0"}] | sort_by([(.p|rank),.at]) | map(.p+.at) | join(",")')
eq "priority then age" "$ranked" '"P09,P20,P21,0"'

holder=alice/host.local/sess-1
eq "holder login" "$(holder_login "$holder")" alice
eq "holder session" "$(holder_session "$holder")" sess-1

# beat_ended (ADR 0094 decision 7): who ended a tracked claim.
be() { # STATE HOLDER NOTE RELEASED_BY
  beat_ended "$(jq -nc --arg s "$1" --arg h "$2" --arg n "$3" --arg b "$4" '{issue:1,state:$s,holder:$h,note:$n,released_by:$b}')" alice s1; }
eq "beat_ended: released by this session" "$(be released alice/mac/s1 x alice/mac/s1)" drop
eq "beat_ended: released by this session on another host name" "$(be released alice/mac/s1 x alice/other/s1)" drop
eq "beat_ended: the parent releasing a lane's claim" "$(be released alice/mac/s1#x x alice/mac/s1)" drop
eq "beat_ended: released by a lane of this session" "$(be released alice/mac/s1 x alice/mac/s1#y)" drop
eq "beat_ended: pr-close" "$(be released alice/mac/s1 x pr-close)" drop
eq "beat_ended: the sweeper" "$(be released alice/mac/s1 x sweeper)" "lost released by sweeper"
eq "beat_ended: another session" "$(be released alice/mac/s1 x alice/mac/s2)" "lost released by alice/mac/s2"
eq "beat_ended: another person" "$(be released alice/mac/s1 x comment:bob)" "lost released by comment:bob"
eq "beat_ended: a session id that only starts with ours" "$(be released alice/mac/s1 x alice/mac/s10)" "lost released by alice/mac/s10"
eq "beat_ended: a takeover names the new holder" "$(be held alice/mac/s2 '' '')" "lost taken over by alice/mac/s2"
eq "beat_ended: no released_by, pr close note keeps dropping" "$(be released alice/mac/s1 'pull request #7 closed' '')" drop
if [[ $(be released alice/mac/s1 'released by the sweeper: x' '') == lost* && $(be released alice/mac/s1 handing '') == lost* ]]; then ok "beat_ended: no released_by, other notes stay lost"; else bad "beat_ended old rule"; fi
eq "beat_ended: a claim that is gone" "$(beat_ended '' alice s1)" "lost it is gone"

# ---- lanes (ADR 0082 amendment), offline: gh and the GitHub-facing functions are stubbed ----
eq "holder login with a lane" "$(holder_login alice/h/s1#x)" alice
eq "holder session drops the lane" "$(holder_session alice/h/s1#x)" s1
eq "holder session without a lane" "$(holder_session alice/h/s1)" s1
eq "holder lane" "$(holder_lane alice/h/s1#x)" x
eq "holder lane when none" "$(holder_lane alice/h/s1)" ""
eq "holder key with a lane" "$(holder_key alice/h/s1#x)" "s1#x"
eq "holder key without a lane" "$(holder_key alice/h/s1)" "s1"
eq "same holder, same lane, other host" "$(same_holder a/h1/s1#x a/h2/s1#x && echo y || echo n)" y
eq "same session, other lane" "$(same_holder a/h/s1#x a/h/s1#y && echo y || echo n)" n
eq "laned holder is not the no-lane holder" "$(same_holder a/h/s1#x a/h/s1 && echo y || echo n)" n
eq "no-lane holder is not a laned holder" "$(same_holder a/h/s1 a/h/s1#x && echo y || echo n)" n
eq "agent lease with a lane" "$(lease_for_holder a/h/s1#x)" 7200
eq "human lease" "$(lease_for_holder a/h/human)" 259200
for l in a x1 0 a-b abcdefghijklmnop; do
  if valid_lane "$l"; then ok "valid lane: $l"; else bad "valid lane: $l"; fi
done
for l in "" -a A a_b abcdefghijklmnopq "a b" 'a#b' 'a/b'; do
  if valid_lane "$l"; then bad "invalid lane: '$l'"; else ok "invalid lane: '$l'"; fi
done
LANE=
split_globals take 5 --lane foo 6 --human
eq "split_globals takes the lane out" "${REST[*]}" "take 5 6"
eq "split_globals sets the lane" "$LANE" foo
eq "split_globals sets human mode" "$HUMAN_MODE" 1
HUMAN_MODE=0
LANE=
eq "split_globals rejects a bad lane" "$(split_globals take 5 --lane Bad 2>/dev/null && echo y || echo n)" n
LANE=
eq "split_globals rejects a missing lane" "$(split_globals take 5 --lane 2>/dev/null && echo y || echo n)" n
LANE=

# The environment variable equals the flag, and the flag wins over the variable.
holder_env() { env -u CLAUDE_CODE_SESSION_ID KOINON_AGENT_SESSION=s1 KOINON_CLAIM_HUMAN=alice "$@" bash -c 'CLAIM_LIB_ONLY=1; source "$1"; shift; "$@"' _ "$claim" my_holder; }
h_env=$(holder_env KOINON_CLAIM_LANE=zed)
h_flag=$(env -u CLAUDE_CODE_SESSION_ID -u KOINON_CLAIM_LANE KOINON_AGENT_SESSION=s1 KOINON_CLAIM_HUMAN=alice bash -c 'CLAIM_LIB_ONLY=1; source "$1"; split_globals --lane zed; my_holder' _ "$claim")
h_both=$(env -u CLAUDE_CODE_SESSION_ID KOINON_CLAIM_LANE=other KOINON_AGENT_SESSION=s1 KOINON_CLAIM_HUMAN=alice bash -c 'CLAIM_LIB_ONLY=1; source "$1"; split_globals --lane zed; my_holder' _ "$claim")
h_none=$(env -u CLAUDE_CODE_SESSION_ID -u KOINON_CLAIM_LANE KOINON_AGENT_SESSION=s1 KOINON_CLAIM_HUMAN=alice bash -c 'CLAIM_LIB_ONLY=1; source "$1"; my_holder' _ "$claim")
h_human=$(env -u CLAUDE_CODE_SESSION_ID KOINON_CLAIM_LANE=zed KOINON_AGENT_SESSION=s1 KOINON_CLAIM_HUMAN=alice bash -c 'CLAIM_LIB_ONLY=1; source "$1"; split_globals --human; my_holder' _ "$claim")
eq "env lane equals --lane" "$h_env" "$h_flag"
eq "holder ends with the lane" "${h_env##*/}" "s1#zed"
eq "--lane wins over the environment" "$h_both" "$h_flag"
eq "no lane: the plain holder" "${h_none##*/}" "s1"
eq "a human claim has no lane" "${h_human##*/}" "human"

LNOW=$(iso_to_epoch 2026-10-30T16:00:00Z)
FIX='[
 {"issue":10,"sha":"s10","state":"held","holder":"alice/mac/s1","human":"alice","lease_until":"2026-10-30T17:00:00Z","pr":"100","note":""},
 {"issue":11,"sha":"s11","state":"held","holder":"alice/mac/s1#y","human":"alice","lease_until":"2026-10-30T17:00:00Z","pr":"","note":""},
 {"issue":12,"sha":"s12","state":"held","holder":"alice/mac/s1#x","human":"alice","lease_until":"2026-10-30T17:00:00Z","pr":"120","note":""},
 {"issue":13,"sha":"s13","state":"released","holder":"alice/mac/s1#z","human":"alice","lease_until":"2026-10-30T17:00:00Z","pr":"","note":""},
 {"issue":14,"sha":"s14","state":"held","holder":"alice/mac/s1#z","human":"alice","lease_until":"2026-10-30T15:00:00Z","pr":"","note":""},
 {"issue":15,"sha":"s15","state":"held","holder":"bob/mac/s9","human":"bob","lease_until":"2026-10-30T17:00:00Z","pr":"","note":""},
 {"issue":16,"sha":"s16","state":"held","holder":"alice/mac/human","human":"alice","lease_until":"2026-10-30T17:00:00Z","pr":"","note":""}]'
eq "counts: no lane" "$(active_counts "$FIX" "$LNOW" alice s1)" "4 1"
eq "counts: lane x" "$(active_counts "$FIX" "$LNOW" alice 's1#x')" "4 1"
eq "counts: lane z ignores released and expired" "$(active_counts "$FIX" "$LNOW" alice 's1#z')" "4 0"
eq "counts: another session" "$(active_counts "$FIX" "$LNOW" bob s9)" "1 1"
eq "counts: the human holder never fills a session cap" "$(active_counts "$FIX" "$LNOW" alice human)" "4 0"
eq "held_by_key names issue and PR" "$(held_by_key "$FIX" "$LNOW" s1)" "#10 (PR #100)"
eq "held_by_key without a PR" "$(held_by_key "$FIX" "$LNOW" 's1#y')" "#11"

# Stubs: nothing below talks to GitHub.
LOG=$(mktemp "${TMPDIR:-/tmp}/claim_test.XXXXXX")
stub_github() {
  server_now() { echo "$LNOW"; }
  human_login() { echo alice; }
  list_claims() {
    if [[ -n ${LIST_FAIL:-} ]]; then echo "claim: gh blip" >&2; return 1; fi
    echo "$FIX"
  }
  beat_clock() { echo $((1000 + ${SLEEPS:-0} * 1000)); }
  issue_json() {
    if [[ -n ${ISSUE_FIX:-} ]]; then echo "$ISSUE_FIX"; return; fi
    echo '{"number":1,"state":"open","pull":false,"labels":["agent-ok"],"assignees":[],"milestone":""}'
  }
  load_claim() { C=$(jq -c --arg n "$1" '.[] | select((.issue | tostring) == $n)' <<<"$FIX"); [[ -n $C ]] || return 1; }
  find_pr() { :; }
  find_closed_pr() { :; }
  open_pr_closers() { echo "${CLOSERS_FIX:-[]}"; }
  rerun_checks_for_issue() { :; }
  rerun_claim_check() { :; }
  pr_is_open() { return 1; }
  write_claim() {
    if [[ -n ${WRITE_RC:-} ]]; then echo "claim: boom" >&2; exit "$WRITE_RC"; fi
    echo "write $1 $2 $3" >>"$LOG"; return 0
  }
  # The beat --every loop: after the 1st sleep the failure clears, after the 2nd the claims change to
  # $ON_SLEEP; more than 5 sleeps means the loop did not stop.
  sleep() {
    if [[ -z ${STUB_SLEEP:-} ]]; then command sleep "$@"; return; fi
    SLEEPS=$((${SLEEPS:-0} + 1))
    if ((SLEEPS == 1)); then WRITE_RC=; LIST_FAIL=; fi
    if ((SLEEPS == 2)) && [[ -n ${ON_SLEEP:-} ]]; then FIX=$ON_SLEEP; fi
    if ((SLEEPS > 5)); then echo "sleep stub: the loop did not stop"; exit 99; fi
  }
  warn_area_overlap() { :; }
  mirror_held() { :; }
  unassign() { :; }
  drop_label() { :; }
  upsert_comment() { :; }
  set_status() { :; }
  gh() { echo 0; }
}
# lanecmd FIX_JSON ARGS...: run claim.sh's command through the global-flag parser, in a subshell,
# as session s1 of alice; prints the output and the exit status.
lanecmd() {
  local fx=$1 rc=0 out
  shift
  out=$( {
    FIX=$fx; export KOINON_AGENT_SESSION=s1; unset CLAUDE_CODE_SESSION_ID KOINON_CLAIM_LANE
    stub_github
    LANE=; HUMAN_CAP=8; SESSION_CAP=1
    split_globals "$@" || exit 1
    set -- "${REST[@]}"
    cmd=$1; shift
    "cmd_${cmd//-/_}" "$@"
  } 2>&1) || rc=$?
  echo "$out"
  echo "rc=$rc"
}
rc_of() { sed -n 's/^rc=//p' <<<"$1"; }

mine_only='[{"issue":10,"sha":"s10","state":"held","holder":"alice/mac/s1","human":"alice","lease_until":"2026-10-30T17:00:00Z","pr":"100","note":""}]'
: >"$LOG"
r=$(lanecmd "$mine_only" take 20 --lane x)
eq "take on a second lane succeeds" "$(rc_of "$r")" 0
eq "take on a second lane writes the laned holder" "$(sed 's#alice/[^/]*/#alice/HOST/#' "$LOG")" "write 20 held alice/HOST/s1#x"
# shellcheck disable=SC2016
x_only='[{"issue":12,"sha":"s12","state":"held","holder":"alice/mac/s1#x","human":"alice","lease_until":"2026-10-30T17:00:00Z","pr":"120","note":""}]'
r=$(lanecmd "$x_only" take 21)
eq "take with no lane while only lane x is held succeeds" "$(rc_of "$r")" 0
r=$(lanecmd "$x_only" take 21 --lane x)
eq "second claim on the same lane is refused" "$(rc_of "$r")" 2
if grep -q '#12 (PR #120)' <<<"$r" && grep -q -- '--lane' <<<"$r" && grep -q 'release' <<<"$r"; then ok "refusal names the held claim and suggests --lane or release"; else bad "refusal text: $r"; fi
r=$(lanecmd "$mine_only" take 21)
eq "second claim with no lane is refused" "$(rc_of "$r")" 2
if grep -q '#10 (PR #100)' <<<"$r" && grep -q 'no lane' <<<"$r"; then ok "no-lane refusal names the no-lane claim"; else bad "no-lane refusal text: $r"; fi
two_lanes=$(jq -c '[.[] | select(.issue == 10 or .issue == 11 or .issue == 12 or .issue == 15)]' <<<"$FIX")
r=$(lanecmd "$two_lanes" take 22 --lane w)
eq "a third lane succeeds under the human cap" "$(rc_of "$r")" 0
eight=$(jq -c '[range(30; 38) as $i | {"issue":$i,"sha":"s\($i)","state":"held","holder":"alice/mac/s1#l\($i)","human":"alice","lease_until":"2026-10-30T17:00:00Z","pr":"","note":""}]' <<<null)
r=$(lanecmd "$eight" take 23 --lane w)
eq "the human cap refuses the 9th claim" "$(rc_of "$r")" 2
if grep -q 'cap of 8' <<<"$r"; then ok "human cap message"; else bad "human cap message: $r"; fi
r=$(lanecmd "$eight" take 23 --lane w --human)
eq "a human claim is not stopped by the session cap but by the human cap" "$(rc_of "$r")" 2
eq "a bad lane is an error" "$(rc_of "$(lanecmd "$mine_only" take 24 --lane NO)")" 1

# take (ADR 0094 decision 5): another author's open pull request, another assignee and another
# class's live claim refuse with exit 2; the caller's own human's open pull request is linked.
export CLOSERS_FIX='[{"number":300,"author":"Carol","issues":[25]},{"number":301,"author":"alice","issues":[26]}]'
r=$(lanecmd '[]' take 25)
eq "take: another author's open pull request refuses" "$(rc_of "$r")" 2
if grep -q '#300 by @Carol' <<<"$r"; then ok "take names the pull request and its author"; else bad "pr refusal text: $r"; fi
: >"$LOG"
r=$(lanecmd '[]' take 26)
eq "take: the own human's open pull request is no refusal" "$(rc_of "$r")" 0
r=$( {
  FIX='[]'; export KOINON_AGENT_SESSION=s1; unset CLAUDE_CODE_SESSION_ID KOINON_CLAIM_LANE
  stub_github
  write_claim() { echo "write $1 $2 pr=$6" >>"$LOG"; }
  LANE=; HUMAN_CAP=8; SESSION_CAP=1
  CLOSERS=$CLOSERS_FIX
  take_one 26 0 alice/mac/s1 alice '[]' "$LNOW"
} 2>&1 )
eq "take links the own human's pull request as pr" "$(grep -c 'write 26 held pr=301' "$LOG")" 1
r=$(ISSUE_FIX='{"number":27,"state":"open","pull":false,"labels":["agent-ok"],"assignees":["bob"],"milestone":""}' lanecmd '[]' take 27)
eq "take: another assignee refuses" "$(rc_of "$r")" 2
if grep -q 'assigned to @bob' <<<"$r"; then ok "take names the assignee"; else bad "assignee refusal text: $r"; fi
r=$(ISSUE_FIX='{"number":27,"state":"open","pull":false,"labels":["agent-ok"],"assignees":["Alice"],"milestone":""}' lanecmd '[]' take 27)
eq "take: assigned to the caller's own human is fine" "$(rc_of "$r")" 0
unset CLOSERS_FIX
person_live='[{"issue":28,"sha":"s28","state":"held","holder":"carol/github/outsider","human":"carol","lease_until":"2026-11-10T16:00:00Z","pr":"","note":""}]'
r=$( { FIX=$person_live; export KOINON_AGENT_SESSION=s1; unset CLAUDE_CODE_SESSION_ID KOINON_CLAIM_LANE; stub_github
  claim_liveness() { LIVE=$(iso_to_epoch 2026-11-10T16:00:00Z); PR_STATE=none; PR_NUM=; return 0; }
  LANE=; HUMAN_CAP=8; SESSION_CAP=1; cmd_take 28; } 2>&1 ) && rc=0 || rc=$?
eq "take: a live outsider claim refuses" "$rc" 2
if grep -q '@carol (outsider claim)' <<<"$r" && ! grep -q 'github/outsider' <<<"$r"; then ok "the refusal names the human and class, not the holder"; else bad "class refusal text: $r"; fi
outsider_old='[{"issue":29,"sha":"s29","state":"held","holder":"carol/github/outsider","human":"carol","lease_until":"2026-10-30T15:00:00Z","pr":"","note":""}]'
r=$( { FIX=$outsider_old; export KOINON_AGENT_SESSION=s1; unset CLAUDE_CODE_SESSION_ID KOINON_CLAIM_LANE; stub_github
  claim_liveness() { LIVE=$(iso_to_epoch 2026-10-30T15:00:00Z); PR_STATE=none; PR_NUM=; return 0; }
  LANE=; HUMAN_CAP=8; SESSION_CAP=1; cmd_take 29 --takeover; } 2>&1 ) && rc=0 || rc=$?
eq "take --takeover of a dead outsider claim" "$rc" 0
# Liveness uses the lease of the holder's class: pull request activity keeps an outsider claim 14 days.
lv() { (
  REPO=acme/koinon
  find_pr() { echo 50; }
  gh() {
    if [[ $1 == api ]]; then echo '{"state":"open","draft":true,"merged":false}'; return; fi
    echo '{"commits":[{"authors":[{"login":"carol"}],"committedDate":"2026-10-30T10:00:00Z"}],"comments":[]}'
  }
  claim_liveness "$(jq -nc --arg h "$1" '{issue:5,holder:$h,human:"carol",lease_until:"2026-10-30T09:00:00Z",pr:""}')"
  echo "$LIVE"
) }
act=$(iso_to_epoch 2026-10-30T10:00:00Z)
eq "liveness: an outsider claim lives 14 days past its activity" "$(lv carol/github/outsider)" $((act + 1209600))
eq "liveness: a person claim lives 3 days past its activity" "$(lv carol/github/human)" $((act + 259200))
eq "liveness: an agent claim lives 2 h past its activity" "$(lv carol/mac/s1)" $((act + 7200))

# beat, review and release with --lane touch only that lane's claim.
: >"$LOG"
eq "beat in the owning lane" "$(rc_of "$(lanecmd "$FIX" beat 12 --lane x)")" 0
eq "beat writes lane x's claim" "$(cut -d' ' -f1,2,3 "$LOG")" "write 12 held"
eq "beat from another lane is a lost claim" "$(rc_of "$(lanecmd "$FIX" beat 12 --lane y)")" 3
eq "beat with no lane cannot touch a laned claim" "$(rc_of "$(lanecmd "$FIX" beat 12)")" 3
eq "beat in a lane cannot touch the no-lane claim" "$(rc_of "$(lanecmd "$FIX" beat 10 --lane x)")" 3
eq "beat with no lane keeps working" "$(rc_of "$(lanecmd "$FIX" beat 10)")" 0
eq "review in the owning lane" "$(rc_of "$(lanecmd "$FIX" review 11 --lane y)")" 0
eq "review from another lane is a lost claim" "$(rc_of "$(lanecmd "$FIX" review 11 --lane x)")" 3
: >"$LOG"
eq "release in the owning lane" "$(rc_of "$(lanecmd "$FIX" release 12 --lane x --note finished)")" 0
eq "release writes only that issue" "$(cut -d' ' -f1,2,3 "$LOG")" "write 12 released"
: >"$LOG"
eq "release from another lane is a lost claim" "$(rc_of "$(lanecmd "$FIX" release 12 --lane y --note finished)")" 3
eq "a refused release writes nothing" "$(wc -l <"$LOG" | tr -d ' ')" 0
# wait-release: released -> 0, still held until the timeout -> 1 (poll interval 0).
held1='[{"issue":10,"sha":"s10","state":"held","holder":"alice/mac/s1","human":"alice","lease_until":"2026-10-30T17:00:00Z","pr":"","note":""}]'
rel1=$(jq -c '[.[] | .state = "released"]' <<<"$held1")
export KOINON_CLAIM_WAIT_INTERVAL=0
eq "wait-release returns 0 for a released claim" "$(rc_of "$(lanecmd "$rel1" wait-release 10 --timeout 5)")" 0
eq "wait-release returns 0 when there is no claim" "$(rc_of "$(lanecmd "$rel1" wait-release 99 --timeout 5)")" 0
eq "wait-release times out with exit 1 on a held claim" "$(rc_of "$(lanecmd "$held1" wait-release 10 --timeout 1)")" 1
eq "wait-release needs a number" "$(rc_of "$(lanecmd "$held1" wait-release x)")" 1
unset KOINON_CLAIM_WAIT_INTERVAL

# beat --mine: every claim of this session (lanes included), never another holder's.
: >"$LOG"
r=$(lanecmd "$FIX" beat --mine)
eq "beat --mine succeeds" "$(rc_of "$r")" 0
eq "beat --mine renews all held claims of the session, lanes included" "$(awk '{print $2}' "$LOG" | sort -n | paste -sd, -)" "10,11,12,14"
eq "beat --mine keeps each lane" "$(sed 's#alice/[^/]*/#alice/HOST/#' "$LOG" | awk '{print $2":"$4}' | sort -n | paste -sd, -)" "10:alice/HOST/s1,11:alice/HOST/s1#y,12:alice/HOST/s1#x,14:alice/HOST/s1#z"
eq "beat --mine prints one line per round" "$(grep -c '^beat [0-9]' <<<"$r")" 1
if grep -q 'renewed #10 #11 #12 #14' <<<"$r"; then ok "beat --mine names what it renewed"; else bad "beat --mine line: $r"; fi
: >"$LOG"
r=$(lanecmd "$FIX" beat --mine --lane x)
eq "beat --mine --lane x renews only that lane" "$(awk '{print $2}' "$LOG" | paste -sd, -)" "12"
eq "beat --mine wants no issue numbers" "$(rc_of "$(lanecmd "$FIX" beat --mine 10)")" 1
eq "beat --every needs --mine" "$(rc_of "$(lanecmd "$FIX" beat --every 5m)")" 1
eq "beat --every rejects junk" "$(rc_of "$(lanecmd "$FIX" beat --mine --every soon)")" 1
eq "duration: seconds" "$(parse_duration 90)" 90
eq "duration: minutes" "$(parse_duration 30m)" 1800
eq "duration: hours" "$(parse_duration 2h)" 7200
eq "duration: zero is refused" "$(parse_duration 0 && echo y || echo n)" n
eq "beat --mine with nothing held" "$(rc_of "$(lanecmd '[]' beat --mine)")" 0
# --every: a claim that leaves our hands (exit 3) stops the loop and is named.
STUB_SLEEP=1
other10=$(jq -c '[.[] | select(.issue == 10) | .holder = "alice/mac/other"]' <<<"$FIX")
ON_SLEEP=$other10
r=$(lanecmd "$mine_only" beat --mine --every 30m)
eq "beat --every stops with 3 when a claim is lost" "$(rc_of "$r")" 3
eq "beat --every ran a round after each sleep until the loss" "$(grep -c '^beat [0-9]' <<<"$r")" 3
if grep -q 'lost #10: stop work' <<<"$r"; then ok "beat --every names the lost claim"; else bad "lost claim not named: $r"; fi
# Exit 1 (an error) does not stop the loop: the next round retries and succeeds.
r=$(WRITE_RC=1 lanecmd "$mine_only" beat --mine --every 30m)
eq "beat --every keeps looping after an error, then stops on the loss" "$(rc_of "$r")" 3
if grep -q 'errors #10' <<<"$r" && grep -q 'renewed #10; lost none; errors none' <<<"$r"; then ok "beat --every retries a failed claim next round"; else bad "error retry: $r"; fi
r=$(WRITE_RC=1 lanecmd "$mine_only" beat --mine)
eq "beat --mine once: an error is exit 1" "$(rc_of "$r")" 1
# A gh failure in one round (listing the claims) is an error of that round, not the end of the loop.
r=$(LIST_FAIL=1 lanecmd "$mine_only" beat --mine --every 30m)
eq "beat --every survives a failed listing" "$(rc_of "$r")" 3
if grep -q 'errors (listing the claims)' <<<"$r" && grep -q 'renewed #10; lost none; errors none' <<<"$r"; then ok "a failed listing is retried next round"; else bad "listing retry: $r"; fi
# Releases: the merge/close release is expected, every other one (sweeper, --force, manual) is lost.
rel_note() { jq -c --arg n "$1" '[.[] | .state = "released" | .note = $n]' <<<"$mine_only"; }
ON_SLEEP=$(rel_note "released by the sweeper: no heartbeat or pull request activity")
r=$(lanecmd "$mine_only" beat --mine --every 30m)
eq "a sweeper release is a lost claim" "$(rc_of "$r")" 3
ON_SLEEP=$(rel_note "handing over")
eq "a manual or --force release is a lost claim" "$(rc_of "$(lanecmd "$mine_only" beat --mine --every 30m)")" 3
# released_by (ADR 0094 decision 7): this session's own release is not a loss, anyone else's is, and is named.
rel_by() { jq -c --arg b "$1" '[.[] | .state = "released" | .note = "handing over" | .released_by = $b]' <<<"$mine_only"; }
ON_SLEEP=$(rel_by "alice/mac/s1")
r=$(lanecmd "$mine_only" beat --mine --every 30m --for 3500)
eq "beat --every: a release by this session is not a lost claim" "$(rc_of "$r")" 0
if grep -q 'released #10' <<<"$r"; then ok "own release reported as released"; else bad "own release: $r"; fi
ON_SLEEP=$(rel_by "alice/mac/s1#x")
eq "beat --every: a release by a lane of this session is not lost" "$(rc_of "$(lanecmd "$mine_only" beat --mine --every 30m --for 3500)")" 0
ON_SLEEP=$(rel_by "pr-close")
eq "beat --every: released_by pr-close is not lost" "$(rc_of "$(lanecmd "$mine_only" beat --mine --every 30m --for 3500)")" 0
ON_SLEEP=$(rel_by "sweeper")
r=$(lanecmd "$mine_only" beat --mine --every 30m)
eq "beat --every: a sweeper release is lost (released_by)" "$(rc_of "$r")" 3
if grep -q 'lost #10: stop work' <<<"$r" && grep -q '#10 released by sweeper' <<<"$r"; then ok "the loss names who released it"; else bad "sweeper named: $r"; fi
ON_SLEEP=$(rel_by "comment:bob")
r=$(lanecmd "$mine_only" beat --mine --every 30m)
eq "beat --every: a release by another person is lost" "$(rc_of "$r")" 3
if grep -q 'released by comment:bob' <<<"$r"; then ok "the loss names the person"; else bad "person named: $r"; fi
ON_SLEEP=$other10
r=$(lanecmd "$mine_only" beat --mine --every 30m)
if grep -q 'taken over by alice/mac/other' <<<"$r"; then ok "a takeover names the new holder"; else bad "takeover named: $r"; fi
ON_SLEEP=$(rel_note "pull request #100 closed")
r=$(lanecmd "$mine_only" beat --mine --every 30m --for 3500)
eq "a merge/close release ends the claim quietly, then --for ends the loop" "$(rc_of "$r")" 0
if grep -q 'released #10' <<<"$r" && grep -q -- '--for elapsed' <<<"$r"; then ok "merge/close release reported, --for elapsed printed"; else bad "close release: $r"; fi
ON_SLEEP=
r=$(lanecmd "$mine_only" beat --mine --every 30m --for 1m)
eq "--for stops the loop with 0" "$(rc_of "$r")" 0
eq "--for stops after the first sleep" "$(grep -c '^beat [0-9]' <<<"$r")" 1
true & gone=$!
wait "$gone"
r=$(lanecmd "$mine_only" beat --mine --every 30m --while-pid "$gone")
eq "--while-pid stops with 0 when the process is gone" "$(rc_of "$r")" 0
eq "--while-pid stops before any round" "$(grep -c '^beat [0-9]' <<<"$r")" 0
r=$(lanecmd "$mine_only" beat --mine --every 30m --while-pid $$ --for 1m)
eq "--while-pid with a live process runs" "$(grep -c '^beat [0-9]' <<<"$r")" 1
eq "--for needs --mine" "$(rc_of "$(lanecmd "$mine_only" beat 10 --for 1m)")" 1
eq "\$PPID of a bash -c is the parent's pid" "$(bash -c 'echo $PPID')" "$$"
eq "--while-pid needs --mine" "$(rc_of "$(lanecmd "$mine_only" beat 10 --while-pid $$)")" 1
unset STUB_SLEEP ON_SLEEP

# adopt: a dead session's claims move to this session, each in its own lane.
dead() { # ISSUE HOLDER LEASE
  printf '{"issue":%s,"sha":"s%s","state":"held","holder":"%s","human":"alice","lease_until":"%s","pr":"","note":""}' "$1" "$1" "$2" "$3"
}
ADOPT_DEAD=$(jq -nc "[$(dead 30 alice/mac/old#x 2026-10-30T15:00:00Z), $(dead 31 alice/mac/old 2026-10-30T15:00:00Z)]")
host_fix() { sed 's#alice/[^/]*/#alice/HOST/#'; }
: >"$LOG"
r=$(lanecmd "$ADOPT_DEAD" adopt --from old)
eq "adopt all claims of a dead session" "$(rc_of "$r")" 0
eq "adopt keeps each claim's lane" "$(host_fix <"$LOG" | sort | paste -sd, -)" "write 30 held alice/HOST/s1#x,write 31 held alice/HOST/s1"
: >"$LOG"
r=$(lanecmd "$ADOPT_DEAD" adopt --from old 30)
eq "adopt one claim" "$(host_fix <"$LOG")" "write 30 held alice/HOST/s1#x"
r=$(lanecmd "$ADOPT_DEAD" adopt --from old 99)
eq "adopt refuses an issue the session does not hold" "$(rc_of "$r")" 2
live_old=$(jq -c ". + [$(dead 32 alice/mac/old#y 2026-10-30T17:00:00Z)]" <<<"$ADOPT_DEAD")
: >"$LOG"
r=$(lanecmd "$live_old" adopt --from old 32)
eq "adopt refuses a live holder" "$(rc_of "$r")" 2
if grep -q 'alive until' <<<"$r"; then ok "adopt names the takeover rule"; else bad "live holder text: $r"; fi
r=$(lanecmd "$live_old" adopt --from old)
eq "adopt without N refuses when one claim is alive" "$(rc_of "$r")" 2
eq "a refused adopt writes nothing" "$(wc -l <"$LOG" | tr -d ' ')" 0
grace_old=$(jq -nc "[$(dead 33 alice/mac/old 2026-10-30T15:50:00Z)]")
eq "adopt refuses a holder still in grace" "$(rc_of "$(lanecmd "$grace_old" adopt --from old)")" 2
ready_old=$(jq -nc "[$(dead 34 alice/mac/old 2026-10-30T15:00:00Z | jq -c '.pr = "340"')]")
# shellcheck disable=SC2329
if (stub_github; claim_liveness() { LIVE=$(iso_to_epoch 2026-10-30T15:00:00Z); PR_STATE=ready; PR_NUM=340; return 0; }
  dead_holder_check 34 "$(jq -c '.[0]' <<<"$ready_old")" "$LNOW" 2>/dev/null); then bad "ready PR must block adopt"; else ok "a ready PR blocks adopt like take --takeover"; fi
r=$(lanecmd "$ADOPT_DEAD" adopt --from old 30 30 30)
eq "repeated N is adopted once" "$(grep -c '^adopted ' <<<"$r")" 1
rc=0
r=$( {
  FIX=$ADOPT_DEAD; export KOINON_AGENT_SESSION=s1; unset CLAUDE_CODE_SESSION_ID KOINON_CLAIM_LANE
  stub_github
  issue_json() { echo '{"number":30,"state":"closed","pull":false,"labels":[],"milestone":""}'; }
  LANE=; HUMAN_CAP=4; SESSION_CAP=1
  cmd_adopt --from old 30
} 2>&1 ) || rc=$?
if ((rc == 2)) && grep -q 'is closed' <<<"$r"; then ok "adopt refuses a closed issue"; else bad "closed issue: rc=$rc $r"; fi
rc=0
r=$( {
  FIX=$ADOPT_DEAD; export KOINON_AGENT_SESSION=s1; unset CLAUDE_CODE_SESSION_ID KOINON_CLAIM_LANE
  stub_github
  issue_json() { return 1; }
  LANE=; HUMAN_CAP=4; SESSION_CAP=1
  cmd_adopt --from old 30
} 2>&1 ) || rc=$?
if ((rc == 1)) && grep -q 'API error' <<<"$r"; then ok "adopt: an API error is exit 1, not closed"; else bad "issue_json failure: rc=$rc $r"; fi
eq "adopt your own session" "$(rc_of "$(lanecmd "$ADOPT_DEAD" adopt --from s1)")" 1
eq "adopt needs --from" "$(rc_of "$(lanecmd "$ADOPT_DEAD" adopt)")" 1
# Caps: the human cap (8) counts the active claims, the session cap (1) counts per lane.
cap_fix=$(jq -c ". + [$(dead 30 alice/mac/old#x 2026-10-30T15:00:00Z)]" <<<"$FIX")
cap_fix=$(jq -c '[.[] | select(.issue != 14)]' <<<"$cap_fix")
# Four more live claims, each in its own lane, bring the human to the cap of 8 (the pre-8 fixture had 4).
cap_fix=$(jq -c '. + [range(40; 44) as $i | {"issue":$i,"sha":"s\($i)","state":"held","holder":"alice/mac/s1#f\($i)","human":"alice","lease_until":"2026-10-30T17:00:00Z","pr":"","note":""}]' <<<"$cap_fix")
r=$(lanecmd "$cap_fix" adopt --from old)
eq "adopt respects the human cap" "$(rc_of "$r")" 2
if grep -q 'cap of 8' <<<"$r"; then ok "adopt cap message"; else bad "cap message: $r"; fi
same_lane=$(jq -nc "[$(dead 30 alice/mac/old#x 2026-10-30T15:00:00Z), $(dead 35 alice/mac/old#x 2026-10-30T15:00:00Z)]")
r=$(lanecmd "$same_lane" adopt --from old)
eq "adopt respects the session cap per lane" "$(rc_of "$r")" 2
rm -f "$LOG"

# ---- check-pr: a conflict check (ADR 0094 decision 2), offline ----
LOG=$(mktemp "${TMPDIR:-/tmp}/claim_check.XXXXXX")
# ckpr CLAIMS_JSON AUTHOR BODY [ADDED_ADR_FILES] -> the check's output and rc. Claims carry `prs` (the
# pull request state claim_liveness would find) and their lease is their liveness.
ckpr() {
  local out rc=0
  out=$( {
    FIX=$1; stub_github; REPO=acme/koinon; RELEASE_AFTER=7200
    # shellcheck disable=SC2154 # ck_body and ck_closers come from chk's caller (an env prefix)
    pr_meta() { jq -nc --arg a "$AUTHOR" --arg b "$ck_body" --argjson c "${ck_closers:-[]}" '{author: $a, body: $b, closers: $c}'; }
    pr_added_adr_files() { if [[ -n ${ADDED_FAIL:-} ]]; then return 1; fi; printf '%s' "$ADDED"; }
    claim_liveness() {
      if [[ -n ${LIVE_FAIL:-} ]]; then return 4; fi
      LIVE=$(parse_lease "$(jq -r .lease_until <<<"$1")"); PR_STATE=$(jq -r '.prs // "none"' <<<"$1"); PR_NUM=$(jq -r .pr <<<"$1")
    }
    cmd_check_pr 77
  } 2>&1 ) || rc=$?
  echo "$out"
  echo "rc=$rc"
}
chk() { AUTHOR=$2 ck_body=$3 ADDED=${4:-} ckpr "$1"; }
cl() { # ISSUE STATE HUMAN LEASE [PR] [PR_STATE]
  jq -nc --arg i "$1" --arg s "$2" --arg h "$3" --arg l "$4" --arg p "${5:-}" --arg ps "${6:-none}" \
    '{issue: ($i | tonumber? // $i), sha: "x", state: $s, holder: "\($h)/mac/s1", human: $h, lease_until: $l, pr: $p, note: "", prs: $ps}'
}
CK=$(jq -sc . < <(
  cl 5 held bob 2026-10-30T17:00:00Z 60 draft
  cl 6 held bob 2026-10-30T10:00:00Z
  cl 7 released bob 2026-10-30T17:00:00Z
  cl 8 held Alice 2026-10-30T17:00:00Z
  cl 9 held bob 2026-10-30T10:00:00Z 61 ready
  cl adr-0095 held bob 2026-11-02T16:00:00Z
  cl adr-0096 held alice 2026-11-02T16:00:00Z
  cl adr-0097 held bob 2026-10-30T10:00:00Z))
r=$(chk "$CK" alice "no issue named")
eq "check: no Closes #N passes" "$(rc_of "$r")" 0
if grep -q '^::notice::' <<<"$r" && grep -qx 'claim check: no Closes #N; nothing to check' <<<"$r"; then ok "check: no Closes #N asks for one and says so"; else bad "no-issue text: $r"; fi
r=$(chk "$CK" alice "Closes #4")
eq "check: an issue without a claim passes" "$(rc_of "$r")" 0
if grep -qx 'claim check: #4 is free' <<<"$r"; then ok "check: free line"; else bad "free line: $r"; fi
eq "check: a released claim passes" "$(rc_of "$(chk "$CK" alice "Closes #7")")" 0
r=$(chk "$CK" alice "Fixes #8")
eq "check: the author's own claim passes (any case)" "$(rc_of "$r")" 0
if grep -qx 'claim check: #8 is claimed by @Alice (the author)' <<<"$r"; then ok "check: own line"; else bad "own line: $r"; fi
r=$(chk "$CK" alice "Closes #5")
eq "check: someone else's live claim fails" "$(rc_of "$r")" 1
eq "check: the failure is the fixed text" "$(grep '^::error::' <<<"$r")" \
  "$(conflict_issue_text 5 bob 2026-10-30T17:00:00Z 60 alice 2026-10-30T19:00:00Z)"
r=$(chk "$CK" alice "Closes #6")
eq "check: a dead claim (past the release delay) passes" "$(rc_of "$r")" 0
if grep -q '#6 is free (the claim of @bob went stale' <<<"$r"; then ok "check: stale line"; else bad "stale line: $r"; fi
eq "check: a claim with a ready pull request is never stale" "$(rc_of "$(chk "$CK" alice "Closes #9")")" 1
eq "check: one conflict among several issues fails" "$(rc_of "$(chk "$CK" alice "Closes #4, closes #5 and closes #8")")" 1
r=$(LIVE_FAIL=1 chk "$CK" alice "Closes #5")
eq "check: an API error is exit 1, never a pass" "$(rc_of "$r")" 1
r=$(chk "$CK" alice "" "docs/adr/0095-x.md")
eq "check: someone else's ADR reservation fails" "$(rc_of "$r")" 1
eq "check: the ADR failure is the fixed text" "$(grep '^::error::' <<<"$r")" "$(conflict_adr_text docs/adr/0095-x.md 0095 bob 2026-11-02T16:00:00Z)"
eq "check: the author's own ADR reservation passes" "$(rc_of "$(chk "$CK" alice "" "docs/adr/0096-x.md")")" 0
eq "check: a reservation that ran out passes" "$(rc_of "$(chk "$CK" alice "" "docs/adr/0097-x.md")")" 0
eq "check: an unreserved ADR number passes" "$(rc_of "$(chk "$CK" alice "" "docs/adr/0098-x.md")")" 0
eq "check: the files cannot be listed is exit 1" "$(rc_of "$(ADDED_FAIL=1 chk "$CK" alice "Closes #4")")" 1
eq "check: a bot or Dependabot author has no exemption to need" "$(rc_of "$(chk "$CK" 'dependabot[bot]' "bump")")" 0
r=$(ck_closers='[5]' chk "$CK" alice "Closes stateracloud/koinon#5")
eq "check: GitHub's closing link fails without a body keyword (owner/repo#N, a URL, the sidebar)" "$(rc_of "$r")" 1
eq "check: the linked issue's failure is the fixed text" "$(grep '^::error::' <<<"$r")" \
  "$(conflict_issue_text 5 bob 2026-10-30T17:00:00Z 60 alice 2026-10-30T19:00:00Z)"
eq "check: a link and a keyword for one issue check it once" "$(ck_closers='[8]' chk "$CK" alice "Fixes #8" | grep -c '#8 is claimed')" 1
# pr_meta reads closingIssuesReferences and keeps this repository's issues only.
r=$( (
  REPO=acme/koinon; OWNER=acme; NAME=koinon
  gh() { [[ $1 == api && $2 == graphql ]] || return 9
    echo '{"data":{"repository":{"pullRequest":{"author":{"login":"alice"},"body":"see the sidebar","closingIssuesReferences":{"nodes":[{"number":5,"repository":{"nameWithOwner":"acme/koinon"}},{"number":6,"repository":{"nameWithOwner":"other/repo"}}]}}}}}'; }
  pr_meta 77 ) )
eq "pr_meta: author, body and this repository's closing links" "$r" '{"author":"alice","body":"see the sidebar","closers":[5]}'
eq "pr_meta: an unknown pull request fails, never an empty pass" "$( (
  REPO=acme/koinon; OWNER=acme; NAME=koinon
  gh() { echo '{"data":{"repository":{"pullRequest":null}}}'; }
  pr_meta 77 >/dev/null && echo pass || echo fail ) )" fail

# ---- claims by comment (ADR 0094 decision 1), offline ----
# cmt CLAIMS_JSON LOGIN PERMISSION BODY [N] -> output and rc of `comment N` (default 40).
cmt() {
  local fx=$1 login=$2 perm=$3 body=$4 n=${5:-40} out rc=0
  out=$( {
    FIX=$fx; stub_github; REPO=acme/koinon; HUMAN_CAP=8; OUTSIDER_CAP=2
    commenter_class() { if [[ $perm == error ]]; then return 1; fi; perm_class "$perm"; }
    reply() { echo "reply #$1: $2" >>"$LOG"; return "${REPLY_RC:-0}"; }
    assign_checked() { if [[ -n ${ASSIGN_DROP:-} ]]; then text_unassignable "$2" "$1"; fi; }
    write_claim() { echo "write $1 $2 $3 $4 lease=$5 parent=${8:-} by=${9:-}" >>"$LOG"; return "${WRITE_RC2:-0}"; }
    upsert_comment() { echo "marker #$1: $2" >>"$LOG"; }
    unassign() { echo "unassign #$1 $2" >>"$LOG"; }
    gh() { echo "gh $*" >>"$LOG"; if [[ $* == *"issues?state=open"* ]]; then printf '%s' "${OPEN_ISSUES:-}"; fi; return 0; }
    KOINON_CLAIM_COMMENT_BODY=$body KOINON_CLAIM_COMMENT_LOGIN=$login cmd_comment "$n"
  } 2>&1 ) || rc=$?
  echo "$out"
  echo "rc=$rc"
}
open_issue() { jq -nc --argjson l "${1:-[]}" '{number:40,state:"open",pull:false,labels:$l,assignees:[],milestone:""}'; }
gfi=$(open_issue '["good first issue"]')
: >"$LOG"; r=$(cmt '[]' alice write /claim)
eq "comment: a person claims an open issue" "$(rc_of "$r")" 0
eq "comment: a person claim is 3 days, holder <login>/github/human" "$(grep '^write' "$LOG")" "write 40 held alice/github/human alice lease=2026-11-02T16:00:00Z parent= by="
: >"$LOG"; r=$(cmt '[]' carol read /claim)
eq "comment: an outsider on an unlabelled issue is answered" "$(rc_of "$r")" 0
eq "comment: the label refusal" "$(grep -c "^reply #40: $(text_label carol 40 | head -c 40)" "$LOG")" 1
eq "comment: a refusal writes no claim" "$(grep -c '^write' "$LOG")" 0
: >"$LOG"; r=$(ISSUE_FIX=$gfi cmt '[]' carol read /claim)
eq "comment: an outsider claim is 14 days, holder <login>/github/outsider" "$(grep '^write' "$LOG")" "write 40 held carol/github/outsider carol lease=2026-11-13T16:00:00Z parent= by="
: >"$LOG"; r=$(ISSUE_FIX=$(open_issue '["help wanted"]') cmt '[]' carol none /claim)
eq "comment: help wanted opens an issue to outsiders" "$(grep -c '^write 40 held carol/github/outsider' "$LOG")" 1
two=$(jq -sc . < <(cl 41 held carol 2026-11-10T16:00:00Z | jq -c '.holder = "carol/github/outsider"'; cl 42 held carol 2026-11-10T16:00:00Z | jq -c '.holder = "carol/github/outsider"'))
: >"$LOG"; r=$(ISSUE_FIX=$gfi cmt "$two" carol read /claim)
eq "comment: the outsider cap of 2" "$(grep -c '^write' "$LOG")" 0
if grep -q 'reply #40: @carol you already hold 2 live claim(s), the cap is 2: #41, #42' "$LOG"; then ok "comment: the cap refusal lists the live claims"; else bad "cap text: $(cat "$LOG")"; fi
bobs=$(cl 40 held bob 2026-10-30T17:00:00Z)
: >"$LOG"; r=$(cmt "[$bobs]" alice admin /claim)
eq "comment: a claim held by someone else is answered" "$(grep -c '^write' "$LOG")" 0
if grep -q 'reply #40: @alice #40 is claimed by @bob until 2026-10-30T17:00:00Z' "$LOG" && ! grep -q 'bob/mac' "$LOG"; then ok "comment: the held refusal names the human and the lease, not the session"; else bad "held text: $(cat "$LOG")"; fi
deadbob=$(cl 40 held bob 2026-10-30T10:00:00Z)
: >"$LOG"; r=$(cmt "[$deadbob]" alice admin /claim)
eq "comment: a dead claim is taken over" "$(grep -c '^write 40 held alice/github/human alice lease=2026-11-02T16:00:00Z parent=x' "$LOG")" 1
eq "comment: the dead claim's human is unassigned" "$(grep -c '^unassign #40 bob' "$LOG")" 1
mine40=$(cl 40 held carol 2026-11-01T16:00:00Z | jq -c '.holder = "carol/github/outsider"')
: >"$LOG"; r=$(ISSUE_FIX=$gfi cmt "[$mine40]" Carol read /claim)
if grep -q '^write 40 held Carol/github/outsider Carol lease=2026-11-13T16:00:00Z parent=x' "$LOG" && grep -q renewed <<<"$r"; then ok "comment: /claim again renews the holder's own claim"; else bad "renew: $r $(cat "$LOG")"; fi
: >"$LOG"; r=$(ISSUE_FIX='{"number":40,"state":"closed","pull":false,"labels":[],"assignees":[]}' cmt '[]' alice write /claim)
eq "comment: a closed issue is answered" "$(grep -c '^reply #40: @alice #40 is closed' "$LOG")" 1
: >"$LOG"; r=$(WRITE_RC2=1 cmt '[]' alice write /claim)
eq "comment: a lost race is answered with retry" "$(grep -c 'comment `/claim` again to retry' "$LOG")" 1
: >"$LOG"; r=$(ASSIGN_DROP=1 ISSUE_FIX=$gfi cmt '[]' carol read /claim)
eq "comment: an assignment GitHub refused is noted in the marker" "$(grep -c "GitHub did not allow assigning @carol" "$LOG")" 1
: >"$LOG"; r=$(cmt '[]' alice write $'thanks!\n/claim')
eq "comment: not a command does nothing" "$(rc_of "$r"):$(wc -l <"$LOG" | tr -d ' ')" "0:0"
: >"$LOG"; r=$(cmt '[]' 'renovate[bot]' write /claim)
eq "comment: a bot's comment is ignored" "$(rc_of "$r"):$(wc -l <"$LOG" | tr -d ' ')" "0:0"
eq "comment: a permission read error is exit 1" "$(rc_of "$(cmt '[]' alice error /claim)")" 1
# The write-refused fallback: pointer to a pull request, one deduplicated Backlog issue, exit 0.
: >"$LOG"; r=$(WRITE_RC2=5 ISSUE_FIX=$gfi cmt '[]' carol read /claim)
eq "fallback: exit 0" "$(rc_of "$r")" 0
eq "fallback: the commenter is pointed to a pull request" "$(grep -c "issues/40/comments -f body=$(text_fallback carol 40)" "$LOG")" 1
eq "fallback: one Backlog issue is opened" "$(grep -c "POST repos/acme/koinon/issues -f title=$FALLBACK_TITLE" "$LOG")" 1
: >"$LOG"; r=$(OPEN_ISSUES=$'12\t'"$FALLBACK_TITLE"$'\n' WRITE_RC2=5 ISSUE_FIX=$gfi cmt '[]' carol read /claim)
eq "fallback: the Backlog issue is deduplicated by its title" "$(grep -c 'POST repos/acme/koinon/issues -f title' "$LOG")" 0
: >"$LOG"; r=$(REPLY_RC=5 cmt '[]' carol read /claim)
eq "fallback: a refused answer also falls back, exit 0" "$(rc_of "$r"):$(grep -c 'not available for outside contributors' "$LOG")" "0:1"
# /release: the claim's human or any person.
c40=$(cl 40 held carol 2026-11-10T16:00:00Z | jq -c '.holder = "carol/github/outsider"')
: >"$LOG"; r=$(cmt "[$c40]" carol read /release)
eq "release: by the holder records the holder" "$(grep '^write' "$LOG")" "write 40 released carol/github/outsider carol lease=2026-10-30T16:00:00Z parent=x by=carol/github/outsider"
: >"$LOG"; r=$(cmt "[$c40]" alice maintain /release)
eq "release: by a person records comment:<login>" "$(grep -c '^write 40 released .* by=comment:alice$' "$LOG")" 1
agent40=$(cl 40 held alice 2026-10-30T17:00:00Z)
: >"$LOG"; r=$(cmt "[$agent40]" alice write /release)
eq "release: a person releasing their own agent's claim is comment:<login>" "$(grep -c 'by=comment:alice$' "$LOG")" 1
: >"$LOG"; r=$(cmt "[$c40]" dave read /release)
eq "release: another outsider is refused" "$(grep -c '^write' "$LOG")" 0
eq "release: the refusal names whose claim it is" "$(grep -c '^reply #40: @dave only @carol (whose claim it is) or a maintainer can release #40' "$LOG")" 1
: >"$LOG"; r=$(cmt '[]' alice write /release)
eq "release: nothing held is answered" "$(grep -c '^reply #40: @alice #40 has no held claim' "$LOG")" 1
: >"$LOG"; r=$(WRITE_RC2=5 cmt "[$c40]" carol read /release)
eq "release: a refused write falls back with exit 0" "$(rc_of "$r"):$(grep -c 'not available for outside contributors' "$LOG")" "0:1"
eq "live claims of: class filter" "$(live_claims_of "$two" "$LNOW" CAROL outsider | paste -sd, -)" "#41,#42"
eq "live claims of: the person class ignores outsider claims" "$(live_claims_of "$two" "$LNOW" carol person)" ""

# ---- sweeper classes (ADR 0094 decision 3), offline ----
# swp HOLDER LIVE_ISO -> what sweep_one did to one claim at LNOW (2026-10-30T16:00Z).
swp() {
  : >"$LOG"
  ( REPO=acme/koinon; GRACE=900; RELEASE_AFTER=7200; OUTSIDER_WARN=345600
    claim_liveness() { LIVE=$(iso_to_epoch "$LV"); PR_STATE=${PRS:-none}; PR_NUM=${PRS:+77}; PR_MERGED=${PRM:-0}; }
    add_label() { echo "label $2" >>"$LOG"; }
    drop_label() { :; }
    upsert_comment() { echo "marker $2" >>"$LOG"; }
    do_release_claim() { echo "release by=$4 merged=$3" >>"$LOG"; }
    gh() {
      if [[ $* == *"/comments --jq"* ]]; then printf '%s' "${SEEN:-}"; return 0; fi
      if [[ $* == *"/labels --jq"* ]]; then return 0; fi
      echo "gh $*" >>"$LOG"
    }
    LV=$2 sweep_one "$(jq -nc --arg h "$1" '{issue:40,sha:"abc",state:"held",holder:$h,human:"carol",lease_until:"x",pr:""}')" "$LNOW" >/dev/null 2>&1 )
  cat "$LOG"
}
eq "sweep: an outsider far from the end is left alone" "$(swp carol/github/outsider 2026-11-10T16:00:00Z)" ""
if [[ $(swp carol/github/outsider 2026-11-02T16:00:00Z) == *"koinon-claim-reminder abc -->"*"@carol your claim on #40 runs out at 2026-11-02T16:00:00Z"* ]]; then ok "sweep: day 10 of 14 reminds the outsider in a new comment"; else bad "reminder: $(swp carol/github/outsider 2026-11-02T16:00:00Z)"; fi
eq "sweep: the reminder is sent once per claim commit" "$(SEEN=123 swp carol/github/outsider 2026-11-02T16:00:00Z)" ""
eq "sweep: an agent claim gets no reminder" "$(swp carol/mac/s1 2026-10-30T17:00:00Z)" ""
if [[ $(swp carol/github/outsider 2026-10-30T15:59:00Z) == "label claim:stale"*'comment `/claim`'* ]]; then ok "sweep: an outsider claim is stale when liveness runs out, with the comment hint"; else bad "outsider stale: $(swp carol/github/outsider 2026-10-30T15:59:00Z)"; fi
eq "sweep: an agent claim keeps its 15-minute grace" "$(swp carol/mac/s1 2026-10-30T15:59:00Z)" ""
if [[ $(swp carol/mac/human 2026-10-30T15:00:00Z) == "label claim:stale"*'--human'* ]]; then ok "sweep: a person claim's hint names /claim and beat --human"; else bad "person stale: $(swp carol/mac/human 2026-10-30T15:00:00Z)"; fi
eq "sweep: released 2 h after liveness" "$(swp carol/github/outsider 2026-10-30T13:59:00Z)" "release by=sweeper merged=0"
# A closed pull request frees the claim; closed unmerged passes merged=0 so the human is unassigned.
eq "sweep: a pull request closed unmerged releases with merged=0 (unassign)" "$(PRS=closed PRM=0 swp carol/mac/s1 2026-10-30T17:00:00Z)" "release by=pr-close merged=0"
eq "sweep: a merged pull request releases with merged=1" "$(PRS=closed PRM=1 swp carol/mac/s1 2026-10-30T17:00:00Z)" "release by=pr-close merged=1"
# claim_liveness finds a closed pull request when the claim has no .pr and no open one exists.
# The lease starts 2026-10-30T16:00Z - 15 min for an agent claim; closedAt must be after that.
cl_liveness() { # [HOLDER] -> state of claim_liveness at CL_NOW (default 2026-10-30T17:00Z, 1 h after the lease start)
  ( REPO=acme/koinon; CLOSED_GRACE=2700
    KOINON_CLAIM_NOW_EPOCH=$(iso_to_epoch "${CL_NOW:-2026-10-30T17:00:00Z}"); export KOINON_CLAIM_NOW_EPOCH
    gh() {
      case $* in
        *"--state open"*) echo '[]' ;;
        *"--state closed"*) printf '%s' "$CLOSED_JSON" ;;
      esac
    }
    claim_liveness "$(jq -nc --arg h "${1:-carol/mac/s1}" '{issue:40,sha:"abc",state:"held",holder:$h,human:"carol",lease_until:"2026-10-30T16:00:00Z",pr:""}')" || exit 9
    echo "$PR_STATE $PR_NUM $PR_MERGED" )
}
# Agent lease 2 h: the lease starts 2026-10-30T14:00Z. Rule: closed unmerged frees only when closedAt >=
# lease start and now >= closedAt + CLOSED_GRACE (45 min); merged is immediate; person claims skip it.
CU='[{"number":81,"body":"Closes #40","author":{"login":"Carol"},"closedAt":"2026-10-30T16:30:00Z","mergedAt":null}]'
eq "liveness: closed unmerged after the grace is found" "$(CLOSED_JSON=$CU CL_NOW=2026-10-30T17:15:00Z cl_liveness)" "closed 81 0"
eq "liveness: closed unmerged inside the grace keeps the claim" "$(CLOSED_JSON=$CU CL_NOW=2026-10-30T17:00:00Z cl_liveness)" "none  0"
eq "liveness: a closed merged pull request is immediate" "$(CLOSED_JSON='[{"number":82,"body":"Fixes #40","author":{"login":"carol"},"closedAt":"2026-10-30T16:59:00Z","mergedAt":"2026-10-30T16:59:00Z"}]' cl_liveness)" "closed 82 1"
eq "liveness: a pull request closed before the lease start is ignored (a beat since)" "$(CLOSED_JSON='[{"number":83,"body":"Closes #40","author":{"login":"carol"},"closedAt":"2026-10-30T13:00:00Z","mergedAt":null}]' cl_liveness)" "none  0"
eq "liveness: another author's closed pull request is ignored" "$(CLOSED_JSON='[{"number":84,"body":"Closes #40","author":{"login":"mallory"},"closedAt":"2026-10-30T14:30:00Z","mergedAt":null}]' cl_liveness)" "none  0"
eq "liveness: a person claim ignores a closed unmerged pull request" "$(CLOSED_JSON=$CU CL_NOW=2026-10-30T20:00:00Z cl_liveness carol/mac/human)" "none  0"
eq "liveness: a person claim still frees on a merged pull request" "$(CLOSED_JSON='[{"number":85,"body":"Closes #40","author":{"login":"carol"},"closedAt":"2026-10-30T16:59:00Z","mergedAt":"2026-10-30T16:59:00Z"}]' cl_liveness carol/mac/human)" "closed 85 1"
eq "verdict: a claim kept inside the grace is not stale" "$(claim_verdict held bob alice 100000 99000 none)" conflict
rm -f "$LOG"

# ---- board skip in the token-limited jobs (ADR 0094 decision 3, #169) ----
# release-pr and the sweeper run on GITHUB_TOKEN, which cannot read an organization project. The board
# write must skip with one log line and the release must still complete.
board_release() { # PROJECT_VALUE: release one open-issue claim with the real set_status; prints output and rc
  local out rc=0
  out=$( {
    GITHUB_ACTIONS=true; PROJECT=$1; PROJECT_OWNER=acme; PROJECT_META=; REPO=acme/koinon; OWNER=acme; NAME=koinon
    GH_CALLS=0
    gh() {
      if [[ $1 == api && $2 == graphql ]]; then echo "graphql" >>"$LOG"; echo "Resource not accessible by integration" >&2; return 1; fi
      if [[ $1 == api && $2 == repos/acme/koinon/issues/7 ]]; then echo open; return 0; fi
      return 0
    }
    server_now() { echo 1000; }
    write_claim() { echo "write $1 $2 by=${9:-}" >>"$LOG"; return 0; }
    unassign() { :; }
    drop_label() { :; }
    upsert_comment() { echo "comment $1" >>"$LOG"; }
    c='{"issue":7,"sha":"s7","state":"held","holder":"alice/mac/s1","human":"alice","lease_until":"2026-10-30T17:00:00Z","pr":"100","note":""}'
    do_release_claim "$c" "pull request #100 closed" 0 pr-close
  } 2>&1) || rc=$?
  echo "$out"
  echo "rc=$rc"
}
: >"$LOG"
r=$(board_release acme-board)
eq "release with an unreadable board still releases (rc 0)" "$(rc_of "$r")" 0
eq "release records released_by pr-close" "$(grep -c 'write 7 released by=pr-close' "$LOG")" 1
eq "release still updates the marker comment after the board skip" "$(grep -c 'comment 7' "$LOG")" 1
eq "the board skip logs exactly one line" "$(grep -c 'board update skipped' <<<"$r")" 1
eq "the board skip is not a warning and does not ask for gh auth refresh" "$(grep -c 'warning\|auth refresh' <<<"$r")" 0
: >"$LOG"
r=$(board_release none)
eq "PROJECT=none releases without touching the board" "$(rc_of "$r")" 0
eq "PROJECT=none makes no graphql call" "$(grep -c graphql "$LOG")" 0
eq "PROJECT=none logs nothing about the board" "$(grep -c 'board' <<<"$r")" 0
rm -f "$LOG"

# ---- issue writes fail the job in Actions, only warn locally (koinon#315) ----
# gh always fails here (a missing issues: write); each write runs in a subshell so a die is observable.
write_try() { # ACTIONS(true|"") WRITE-COMMAND...: prints output and rc
  local act=$1 out rc=0; shift
  out=$( { if [[ -n $act ]]; then GITHUB_ACTIONS=$act; else unset GITHUB_ACTIONS; fi
    REPO=acme/koinon; MARKER='<!-- koinon-claim -->'
    gh() { return 1; }
    "$@"; echo "continued"; } 2>&1) || rc=$?
  echo "$out"; echo "rc=$rc"
}
for w in "unassign 7 alice" "add_label 7 claim:stale" "upsert_comment 7 body"; do
  read -ra wa <<<"$w"; r=$(write_try true "${wa[@]}")
  if [[ $(rc_of "$r") != 0 ]] && ! grep -q continued <<<"$r"; then ok "Actions: a failing '${w%% *}' fails the job"; else bad "Actions: a failing '${w%% *}' fails the job ($r)"; fi
  r=$(write_try "" "${wa[@]}")
  eq "local: a failing '${w%% *}' warns and continues (rc 0)" "$(rc_of "$r")" 0
  eq "local: a failing '${w%% *}' says continued" "$(grep -c continued <<<"$r")" 1
  eq "local: a failing '${w%% *}' warns" "$(grep -c 'warning' <<<"$r")" 1
done
# remind_once: reading the comments may fail softly; the post must not.
r=$({ GITHUB_ACTIONS=true; REPO=acme/koinon; REMINDER_MARKER='<!-- r'
  gh() { [[ $* == *"-X POST"* ]] && return 1; return 0; }
  ( remind_once 7 sha body ); echo "rc=$?"; } 2>&1)
eq "Actions: a failing reminder post fails the job" "$([[ $(rc_of "$r") != 0 ]] && echo yes)" yes
r=$(write_try true unassign 7 alice)
eq "Actions: the failure names the write" "$(grep -c 'cannot unassign alice from #7' <<<"$r")" 1

# A sweep whose write_claim dies (missing contents:write, ruleset refusal) must end red, after trying every claim.
LOG=$(mktemp "${TMPDIR:-/tmp}/claim_sweep.XXXXXX")
sweep_rc=0
(
  list_claims() { echo '[{"issue":7,"state":"held"},{"issue":8,"state":"held"}]'; }
  server_now() { echo 1000; }
  sweep_one() { echo "tried" >>"$LOG"; die "cannot write the claim ref"; }
  cmd_sweep
) >/dev/null 2>&1 || sweep_rc=$?
if ((sweep_rc != 0)); then ok "sweep exits non-zero when a claim write dies"; else bad "sweep exited 0 after a write died"; fi
eq "sweep still tries every claim after a failure" "$(grep -c tried "$LOG")" 2
rm -f "$LOG"

# release-pr over a pull request that closes two issues: a write that dies in the first claim's release
# (Actions) must not skip the second, and the job ends red.
LOG=$(mktemp "${TMPDIR:-/tmp}/claim_relpr.XXXXXX")
rp_rc=0
(
  GITHUB_ACTIONS=true; REPO=acme/koinon
  gh() { case $* in *mergedAt*) echo "2026-11-07T10:00:00Z" ;; *--json\ body*) printf 'Closes #7\nFixes #8' ;; esac; }
  load_claim() { C=$(jq -nc --argjson n "$1" '{issue:$n,state:"held",pr:"50"}'); }
  do_release_claim() { echo "released $(jq -r .issue <<<"$1")" >>"$LOG"; unassign() { :; }; write_failed "cannot unassign alice from #$(jq -r .issue <<<"$1")"; }
  cmd_release_pr 50
) >/dev/null 2>&1 || rp_rc=$?
if ((rp_rc != 0)); then ok "release-pr exits non-zero when a claim write dies"; else bad "release-pr exited 0 after a write died"; fi
eq "release-pr still releases every closing issue after a failure" "$(grep -c released "$LOG")" 2
rm -f "$LOG"

# release-merged (the push-to-main release job, #463): releases the merged pull requests of the pushed
# commits once each, ignores an open or closed-unmerged one, and ends red when a release fails.
# The fake gh applies the --jq filter with real jq to canned API JSON, so the merged_at filter is tested.
LOG=$(mktemp "${TMPDIR:-/tmp}/claim_relmerged.XXXXXX")
fake_gh_api() {
  local f="" a body=""
  for a in "$@"; do [[ $prev == --jq ]] && f=$a; prev=$a; done
  case $* in
    *compare/*) body='{"commits":[{"sha":"aaaaaaa"},{"sha":"bbbbbbb"}]}' ;;
    *commits/aaaaaaa/pulls*) body='[{"number":50,"merged_at":"2026-11-09T10:00:00Z"},{"number":60,"merged_at":null}]' ;;
    *commits/bbbbbbb/pulls*) body='[{"number":50,"merged_at":"2026-11-09T10:00:00Z"},{"number":51,"merged_at":"2026-11-09T10:00:00Z"},{"number":61,"merged_at":null}]' ;;
    *commits/ccccccc/pulls*) body='[{"number":52,"merged_at":"2026-11-09T10:00:00Z"}]' ;;
    *commits/ddddddd/pulls*) body='[{"number":53,"merged_at":"2026-11-09T10:00:00Z"}]' ;;
    *commits/eeeeeee/pulls*) body='[{"number":62,"merged_at":null}]' ;;
    *) return 0 ;;
  esac
  jq -r "$f" <<<"$body"
}
prev=
(
  REPO=acme/koinon
  gh() { fake_gh_api "$@"; }
  cmd_release_pr() { echo "pr $1" >>"$LOG"; }
  cmd_release_merged 1111111 bbbbbbb
) >/dev/null 2>&1
eq "release-merged releases each merged pull request once, not an open or closed-unmerged one" "$(tr '\n' ' ' <"$LOG")" "pr 50 pr 51 "
: >"$LOG"
(
  REPO=acme/koinon
  gh() { case $* in *compare/*) echo "unexpected compare" >>"$LOG" ;; *) fake_gh_api "$@" ;; esac; }
  cmd_release_pr() { echo "pr $1" >>"$LOG"; }
  cmd_release_merged 0000000000000000000000000000000000000000 ccccccc
) >/dev/null 2>&1
eq "release-merged on a new branch reads the head commit only" "$(tr '\n' ' ' <"$LOG")" "pr 52 "
# AFTER is read even when the compare list lacks it (a capped page), and the compare is paginated.
: >"$LOG"
(
  REPO=acme/koinon
  gh() { case $* in *compare/*) { [[ $* == *--paginate* ]] && echo "paginated" >>"$LOG"; echo aaaaaaa; } ;; *) fake_gh_api "$@" ;; esac; }
  cmd_release_pr() { echo "pr $1" >>"$LOG"; }
  cmd_release_merged 1111111 ddddddd
) >/dev/null 2>&1
eq "release-merged paginates the compare and always includes AFTER" "$(tr '\n' ' ' <"$LOG")" "paginated pr 50 pr 53 "
rm_out=$(
  REPO=acme/koinon
  gh() { fake_gh_api "$@"; }
  cmd_release_pr() { echo "pr $1" >>"$LOG"; }
  cmd_release_merged "" eeeeeee 2>&1
)
eq "release-merged says so when no merged pull request is found" "$rm_out" "release-merged: no merged pull request found for none..eeeeeee"
rm_rc=0
(
  REPO=acme/koinon
  gh() { fake_gh_api "$@"; }
  cmd_release_pr() { return 1; }
  cmd_release_merged "" ddddddd
) >/dev/null 2>&1 || rm_rc=$?
if ((rm_rc != 0)); then ok "release-merged exits non-zero when a release fails"; else bad "release-merged exited 0 after a release failed"; fi
rm -f "$LOG"

# The updater's needs-update label is informational (its App token has no issues: write): a failing label
# write in Actions mode must still reach update-branch for the target.
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

if [[ -z ${KOINON_CLAIM_TEST_REPO:-} ]]; then
  echo "(set KOINON_CLAIM_TEST_REPO=owner/scratch-repo to run the live tests)"
  exit "$fail"
fi

# ---- live tests against a scratch repository ----
export KOINON_CLAIM_REPO=$KOINON_CLAIM_TEST_REPO
export KOINON_CLAIM_PROJECT=none
# Earlier runs leave held claims behind (refs are never deleted); the cap has its own check below.
export KOINON_CLAIM_HUMAN_CAP=1000
repo=$KOINON_CLAIM_REPO
[[ $repo != stateracloud/koinon ]] || { echo "refusing to run live tests against $repo"; exit 1; }
export KOINON_CLAIM_HUMAN
KOINON_CLAIM_HUMAN=$(gh api user --jq .login)

# code CMD...: run and print the exit status.
code() { local rc=0; "$@" >/dev/null 2>&1 || rc=$?; echo "$rc"; }
newissue() { gh issue create -R "$repo" -t "claim test $1" -b "scratch" | grep -oE '[0-9]+$'; }
# Session ids are unique per run: claims from earlier runs stay held in the scratch repository.
run=t$$-$RANDOM
sess() { export KOINON_AGENT_SESSION="$run-$1"; }

i1=$(newissue one); i2=$(newissue two); i3=$(newissue three)
ok "created scratch issues #$i1 #$i2 #$i3"

sess A
eq "take" "$(code "$claim" take "$i1")" 0
eq "show" "$("$claim" show "$i1" | head -1 | grep -c active)" 1
sess B
eq "second session cannot take a held issue" "$(code "$claim" take "$i1")" 2
eq "beat by a non-holder is a lost claim" "$(code "$claim" beat "$i1")" 3
eq "release by a non-holder is a lost claim" "$(code "$claim" release "$i1" --note x)" 3
sess A
eq "beat" "$(code "$claim" beat "$i1")" 0
eq "session cap" "$(code "$claim" take "$i2")" 2
eq "release needs a note" "$(code "$claim" release "$i1")" 1
eq "release" "$(code "$claim" release "$i1" --note "handoff")" 0
sess B
eq "take a released claim" "$(code "$claim" take "$i1")" 0
sess A
eq "old holder lost it" "$(code "$claim" beat "$i1")" 3

# expiry and takeover
sess B
"$claim" release "$i1" --note reset >/dev/null
sess A
KOINON_CLAIM_LEASE_SECONDS=1 KOINON_CLAIM_GRACE_SECONDS=1 "$claim" take "$i2" >/dev/null
sleep 4
sess B
eq "takeover needs the flag" "$(KOINON_CLAIM_GRACE_SECONDS=1 code "$claim" take "$i2")" 2
eq "takeover" "$(KOINON_CLAIM_GRACE_SECONDS=1 code "$claim" take "$i2" --takeover)" 0
sess A
eq "taken-over holder is told" "$(code "$claim" beat "$i2")" 3

# block
sess B
eq "block" "$(code "$claim" block "$i2" "$i3")" 0
eq "blocked issue is released" "$("$claim" show "$i2" | head -1 | grep -c released)" 1

# race: two sessions, one issue, exactly one winner
i4=$(newissue race)
(rc=0; KOINON_AGENT_SESSION="$run-R1" "$claim" take "$i4" >"$tmp.o1" 2>&1 || rc=$?; echo "$rc" >"$tmp.r1") &
(rc=0; KOINON_AGENT_SESSION="$run-R2" "$claim" take "$i4" >"$tmp.o2" 2>&1 || rc=$?; echo "$rc" >"$tmp.r2") &
wait
eq "exactly one racer wins" "$(cat "$tmp.r1" "$tmp.r2" | sort | paste -sd, -)" "0,2"
if [[ $(cat "$tmp.r1" "$tmp.r2" | sort | paste -sd, -) != "0,2" ]]; then cat "$tmp.o1" "$tmp.o2"; fi
rm -f "$tmp.r1" "$tmp.r2" "$tmp.o1" "$tmp.o2"

# human claim: 3-day lease, outside the session cap, beat and release with --human
i5=$(newissue human)
sess A
"$claim" take "$i5" --human >/dev/null
eq "human claim lease is days" "$("$claim" show "$i5" | grep -c 'lease_until: ')" 1
until_epoch=$(iso_to_epoch "$("$claim" show "$i5" | sed -n 's/^ *lease_until: //p')")
now_epoch=$(date -u +%s)
if ((until_epoch - now_epoch > 250000)); then ok "human lease about 3 days"; else bad "human lease about 3 days"; fi
eq "agent session of the same human cannot beat a human claim" "$(code "$claim" beat "$i5")" 3
eq "human beat" "$(code "$claim" beat "$i5" --human)" 0
eq "human release" "$(code "$claim" release "$i5" --human --note finished)" 0

# several issues as one unit, all or nothing
i6=$(newissue unit-a); i7=$(newissue unit-b)
sess M
eq "take two as one unit" "$(code "$claim" take "$i6" "$i7")" 0
sess N
i8=$(newissue unit-c)
eq "unit rolls back when one is held" "$(code "$claim" take "$i8" "$i6")" 2
eq "rolled back issue is released" "$("$claim" show "$i8" | head -1 | grep -c released)" 1
sess M
"$claim" release "$i6" --note x >/dev/null; "$claim" release "$i7" --note x >/dev/null

# shellcheck disable=SC2016
# generated per-worktree holder: no session env, two worktrees do not share an id
holder_a=$(env -u KOINON_AGENT_SESSION -u CLAUDE_CODE_SESSION_ID bash -c 'CLAIM_LIB_ONLY=1; source "$1"; my_holder' _ "$claim")
# shellcheck disable=SC2016
holder_b_dir=$(mktemp -d "${TMPDIR:-/tmp}/claim_test.XXXXXX")
holder_b=$(cd "$holder_b_dir" && git init -q . && env -u KOINON_AGENT_SESSION -u CLAUDE_CODE_SESSION_ID bash -c 'CLAIM_LIB_ONLY=1; source "$1"; my_holder' _ "$claim")
if [[ -n $holder_a && $holder_a != "$holder_b" ]]; then ok "holders differ per git dir"; else bad "holders differ per git dir ($holder_a / $holder_b)"; fi

# sweeper: an expired claim with no PR is released after lease + grace + release window
i9=$(newissue sweep)
sess S
KOINON_CLAIM_LEASE_SECONDS=1 "$claim" take "$i9" >/dev/null
sleep 3
KOINON_CLAIM_GRACE_SECONDS=1 KOINON_CLAIM_STALE_RELEASE_SECONDS=2 "$claim" sweep >/dev/null 2>&1 || true
eq "sweeper releases a dead claim" "$("$claim" show "$i9" | head -1 | grep -c released)" 1
i10=$(newissue sweep-live)
"$claim" take "$i10" >/dev/null
KOINON_CLAIM_GRACE_SECONDS=1 KOINON_CLAIM_STALE_RELEASE_SECONDS=2 "$claim" sweep >/dev/null 2>&1 || true
eq "sweeper keeps a live claim" "$("$claim" show "$i10" | head -1 | grep -c active)" 1
"$claim" release "$i10" --note x >/dev/null

# check-pr (ADR 0094 decision 2): it fails only on someone else's live claim, so every case the
# scratch repository can make with one login passes. The conflict itself is covered offline.
i11=$(newissue pr-check)

default=$(gh api "repos/$repo" --jq .default_branch)
base=$(gh api "repos/$repo/git/ref/heads/$default" --jq .object.sha)
br=test/claim-$i11
gh api -X POST "repos/$repo/git/refs" -f ref="refs/heads/$br" -f sha="$base" >/dev/null
printf 'x\n' | base64 | { read -r b64; gh api -X PUT "repos/$repo/contents/src-$i11.go" -f message=add -f content="$b64" -f branch="$br" >/dev/null; }
prn=$(gh pr create -R "$repo" -H "$br" -B "$default" -t "chore: sneaky $i11" -b "Closes #$i11" | grep -oE '[0-9]+$')
export KOINON_CLAIM_CODEOWNERS=$here/../.github/CODEOWNERS
eq "no claim passes (no title or label exemption to need)" "$(code "$claim" check-pr "$prn")" 0
sess C
eq "take links the caller's own open pull request" "$(code "$claim" take "$i11")" 0
eq "claimed by the author passes" "$(code "$claim" check-pr "$prn")" 0
sess C
"$claim" release "$i11" --note x >/dev/null
eq "released claim passes" "$(code "$claim" check-pr "$prn")" 0
gh pr close -R "$repo" "$prn" >/dev/null

sess K
eq "human cap refuses" "$(KOINON_CLAIM_HUMAN_CAP=0 code "$claim" take "$i3")" 2

# ADR number reservation: distinct, increasing numbers
sess A
a1=$("$claim" adr 2>/dev/null); sess B; a2=$("$claim" adr 2>/dev/null)
if [[ $a1 =~ ^[0-9]{4}$ && $a2 =~ ^[0-9]{4}$ && $a1 != "$a2" ]]; then ok "adr reservations differ ($a1, $a2)"; else bad "adr reservations differ ($a1, $a2)"; fi
(rc=0; KOINON_AGENT_SESSION="$run-R1" "$claim" adr >"$tmp.a1" 2>/dev/null || rc=$?) &
(rc=0; KOINON_AGENT_SESSION="$run-R2" "$claim" adr >"$tmp.a2" 2>/dev/null || rc=$?) &
wait
if [[ -s $tmp.a1 && -s $tmp.a2 && $(cat "$tmp.a1") != "$(cat "$tmp.a2")" ]]; then ok "concurrent adr reservations differ"; else bad "concurrent adr reservations differ"; fi
rm -f "$tmp.a1" "$tmp.a2"

for i in "$i1" "$i2" "$i3" "$i4" "$i5" "$i6" "$i7" "$i8" "$i9" "$i10" "$i11"; do
  "$claim" release "$i" --force --note "test cleanup" >/dev/null 2>&1 || true
  gh issue close -R "$repo" "$i" >/dev/null; done
exit "$fail"
