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

# update_target: the updater only touches a ready, green pull request that is behind main (#596).
ut_ok='[{"__typename":"CheckRun","name":"a","status":"COMPLETED","conclusion":"SUCCESS"},{"__typename":"CheckRun","name":"b","status":"COMPLETED","conclusion":"SKIPPED"},{"__typename":"StatusContext","context":"c","state":"SUCCESS"}]'
ut_red='[{"__typename":"CheckRun","name":"a","status":"COMPLETED","conclusion":"FAILURE"}]'
ut_run='[{"__typename":"CheckRun","name":"a","status":"IN_PROGRESS","conclusion":null}]'
ut_pr() { # NUMBER CREATED DRAFT MERGESTATE AUTOMERGE ROLLUP
  jq -n --argjson n "$1" --arg c "$2" --argjson d "$3" --arg m "$4" --argjson a "$5" --argjson r "$6" \
    '{number: $n, createdAt: $c, isDraft: $d, mergeStateStatus: $m, autoMergeRequest: (if $a then {enabledAt: $c} else null end), statusCheckRollup: $r}'
}
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
eq "parse garbage" "$(claim_parse 'hello')" '{"state":"","holder":"","human":"","lease_until":"","pr":"","note":""}'
rel=$(claim_format 12 released "alice/mac/s1" alice 2026-10-30T16:00:00Z "" "done")
eq "parse released" "$(jq -r .state <<<"$(claim_parse "$rel")")" "released"

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

for t in "chore: x" "chore(ci): x" "ci: pin" "build(deps): bump" "build(deps-dev): bump" "chore!: x"; do
  if title_exempt "$t"; then ok "title exempt: $t"; else bad "title exempt: $t"; fi
done
for t in "feat: x" "fix(chore): x" "builder: x" "build: x" "Chore: x"; do
  if title_exempt "$t"; then bad "title not exempt: $t"; else ok "title not exempt: $t"; fi
done
if is_dependabot "app/dependabot"; then ok "dependabot by actor"; else bad "dependabot by actor"; fi
if is_dependabot "dependabot[bot]"; then ok "dependabot[bot]"; else bad "dependabot[bot]"; fi
if is_dependabot "not-dependabot"; then bad "lookalike is not dependabot"; else ok "lookalike is not dependabot"; fi
if printf 'docs/a.md\n.github/CODEOWNERS\n' | pr_touches_code; then bad "docs are not code"; else ok "docs are not code"; fi
if printf 'docs/a.md\ninternal/x/y.go\n' | pr_touches_code; then ok "go is code"; else bad "go is code"; fi
if printf 'charts/koinon/values.yaml\n' | pr_touches_code; then ok "chart is code"; else bad "chart is code"; fi
co=$(mktemp "${TMPDIR:-/tmp}/claim_test.XXXXXX")
printf '# c\n* @ProcoDuck @ovirandunu\n/docs/ @x\n' >"$co"
eq "humans list" "$(humans_list "$co" | paste -sd, -)" "procoduck,ovirandunu"
rm -f "$co"

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
  issue_json() { echo '{"number":1,"state":"open","pull":false,"labels":["agent-ok"],"milestone":""}'; }
  load_claim() { C=$(jq -c --argjson n "$1" '.[] | select(.issue == $n)' <<<"$FIX"); [[ -n $C ]] || return 1; }
  find_pr() { :; }
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

# check-pr: a pull request closing a claimed issue passes, an unclaimed one fails
i11=$(newissue pr-check)

default=$(gh api "repos/$repo" --jq .default_branch)
base=$(gh api "repos/$repo/git/ref/heads/$default" --jq .object.sha)
br=test/claim-$i11
gh api -X POST "repos/$repo/git/refs" -f ref="refs/heads/$br" -f sha="$base" >/dev/null
printf 'x\n' | base64 | { read -r b64; gh api -X PUT "repos/$repo/contents/src-$i11.go" -f message=add -f content="$b64" -f branch="$br" >/dev/null; }
prn=$(gh pr create -R "$repo" -H "$br" -B "$default" -t "chore: sneaky $i11" -b "Closes #$i11" | grep -oE '[0-9]+$')
export KOINON_CLAIM_CODEOWNERS=$here/../.github/CODEOWNERS
eq "chore title with source files and no claim fails" "$(code "$claim" check-pr "$prn")" 1
sess C
"$claim" take "$i11" >/dev/null
eq "claimed by the author passes" "$(code "$claim" check-pr "$prn")" 0
sess C
"$claim" release "$i11" --note x >/dev/null
eq "released claim fails" "$(code "$claim" check-pr "$prn")" 1
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
