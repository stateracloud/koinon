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

lanes=$(mktemp)
trap 'rm -f "$lanes"' EXIT
printf '# c\nAlice: api, web  # trailing\nbob: connectors/github\n' >"$lanes"
eq "lane lookup" "$(lane_components alice "$lanes" | paste -sd, -)" "api,web"
eq "lane with slash" "$(lane_components BOB "$lanes")" "connectors/github"
eq "lane missing" "$(lane_components carol "$lanes")" ""

if pr_exempt "chore(deps): bump" someone ""; then ok "chore title exempt"; else bad "chore title exempt"; fi
if pr_exempt "ci: pin" someone ""; then ok "ci title exempt"; else bad "ci title exempt"; fi
if pr_exempt "feat: x" "app/dependabot" ""; then ok "dependabot exempt"; else bad "dependabot exempt"; fi
if pr_exempt "feat: x" someone "a,unclaimed-ok"; then ok "label exempt"; else bad "label exempt"; fi
if pr_exempt "feat: x" someone "a"; then bad "feat not exempt"; else ok "feat not exempt"; fi
eq "closing issues" "$(closing_issues $'Closes #5 and fixes #7.\nresolved: #5, see #9' | paste -sd, -)" "5,7"

ranked=$(jq -nc "$JQ_RANK"' [{p:"P2",at:"1"},{p:"",at:"0"},{p:"P0",at:"9"},{p:"P2",at:"0"}] | sort_by([(.p|rank),.at]) | map(.p+.at) | join(",")')
eq "priority then age" "$ranked" '"P09,P20,P21,0"'

holder=alice/host.local/sess-1
eq "holder login" "$(holder_login "$holder")" alice
eq "holder session" "$(holder_session "$holder")" sess-1

if [[ -z ${KOINON_CLAIM_TEST_REPO:-} ]]; then
  echo "(set KOINON_CLAIM_TEST_REPO=owner/scratch-repo to run the live tests)"
  exit "$fail"
fi

# ---- live tests against a scratch repository ----
export KOINON_CLAIM_REPO=$KOINON_CLAIM_TEST_REPO
export KOINON_CLAIM_PROJECT=none
repo=$KOINON_CLAIM_REPO
[[ $repo != stateracloud/koinon ]] || { echo "refusing to run live tests against $repo"; exit 1; }
export KOINON_CLAIM_HUMAN
KOINON_CLAIM_HUMAN=$(gh api user --jq .login)

# code CMD...: run and print the exit status.
code() { local rc=0; "$@" >/dev/null 2>&1 || rc=$?; echo "$rc"; }
newissue() { gh issue create -R "$repo" -t "claim test $1" -b "scratch" | grep -oE '[0-9]+$'; }
sess() { export KOINON_AGENT_SESSION=$1; }

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
(KOINON_AGENT_SESSION=R1 "$claim" take "$i4" >/dev/null 2>&1; echo $? >"$lanes.r1") &
(KOINON_AGENT_SESSION=R2 "$claim" take "$i4" >/dev/null 2>&1; echo $? >"$lanes.r2") &
wait
eq "exactly one racer wins" "$(cat "$lanes.r1" "$lanes.r2" | sort | paste -sd, -)" "0,2"
rm -f "$lanes.r1" "$lanes.r2"

for i in "$i1" "$i2" "$i3" "$i4"; do gh issue close -R "$repo" "$i" >/dev/null; done
exit "$fail"
