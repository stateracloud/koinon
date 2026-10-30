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

tmp=$(mktemp)
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
co=$(mktemp)
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
holder_b=$(cd "$(mktemp -d)" && git init -q . && env -u KOINON_AGENT_SESSION -u CLAUDE_CODE_SESSION_ID bash -c 'CLAIM_LIB_ONLY=1; source "$1"; my_holder' _ "$claim")
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
