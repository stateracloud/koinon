#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline tests for hack/claim.sh (ADR 0082, ADR 0104); hack/pr-updater-test.sh tests the updater.
# Nothing here talks to GitHub: the GitHub-facing functions are stubbed. CI runs this file in the
# `quick checks` job.
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
  "::error::#159 is claimed by @procoduck; this pull request is by @outsider1. Ask @procoduck to release the claim (hack/claim.sh release 159 or release-pr), then re-run this check."
eq "conflict text: ADR number" "$(conflict_adr_text docs/adr/0095-x.md 0095 procoduck)" \
  "::error::docs/adr/0095-x.md uses ADR number 0095, which @procoduck reserved (claim/adr-0095). Take a free number with hack/claim.sh adr and rename the file."

# JQ_PR_CLOSERS: closingIssuesReferences of this repository plus the body's closing keywords.
closers_page='[{"data":{"repository":{"pullRequests":{"nodes":[
 {"number":200,"body":"Closes #5\nfixes: #6","author":{"login":"Carol"},"closingIssuesReferences":{"nodes":[{"number":5,"repository":{"nameWithOwner":"acme/koinon"}},{"number":9,"repository":{"nameWithOwner":"acme/other"}}]}},
 {"number":201,"body":null,"author":{"login":"alice"},"closingIssuesReferences":{"nodes":[{"number":7,"repository":{"nameWithOwner":"acme/koinon"}}]}},
 {"number":202,"body":"see #8","author":null,"closingIssuesReferences":{"nodes":[]}}]}}}}]'
CL=$(jq -c --arg repo acme/koinon "$JQ_PR_CLOSERS" <<<"$closers_page")
eq "closers: references and body keywords, this repository only" "$(jq -c '[.[] | [.number, .author, .issues]]' <<<"$CL")" '[[200,"Carol",[5,6]],[201,"alice",[7]],[202,"",[]]]'
eq "prs closing: another author" "$(prs_closing "$CL" 5 alice)" '{"own":"","others":"#200 by @Carol"}'
eq "prs closing: the caller's own pull request (any case)" "$(prs_closing "$CL" 7 Alice)" '{"own":"201","others":""}'
eq "prs closing: none" "$(prs_closing "$CL" 8 alice)" '{"own":"","others":""}'
eq "assignees: others" "$(other_assignees '{"assignees":["bob","Alice"]}' alice)" "@bob"
eq "assignees: none" "$(other_assignees '{}' alice)" ""

# JQ_NEXT: skip held claims, open closing pull requests and other assignees; In Progress / In Review
# with nothing holding the issue count as Ready.
item() { # N STATUS ASSIGNEES_JSON [LABELS_JSON] [PRIORITY]
  jq -nc --argjson n "$1" --arg s "$2" --argjson a "$3" --argjson l "${4:-[\"agent-ok\"]}" --arg p "${5-P1}" \
    '{status:{name:$s},priority:{name:$p},component:null,content:{number:$n,title:"t\($n)",state:"OPEN",createdAt:"2026-01-0\($n % 9 + 1)T00:00:00Z",repository:{nameWithOwner:"acme/koinon"},milestone:{title:"m1"},labels:{nodes:($l|map({name:.}))},assignees:{nodes:($a|map({login:.}))}}}'
}
board=$(jq -sc '[{data:{organization:{projectV2:{items:{nodes:.}}}}}]' < <(
  item 1 Ready '[]'; item 2 Ready '[]'; item 3 Ready '[]'; item 4 Ready '["bob"]'; item 5 Ready '["Alice"]'
  item 6 "In Progress" '[]'; item 7 "In Review" '[]'; item 8 Backlog '[]'; item 9 Ready '[]' '[]'))
nx() { jq -r --arg repo acme/koinon --arg ms m1 --arg label "" --argjson held '[2]' --argjson prs '[3]' --arg human alice "$JQ_NEXT"' | map(.n) | join(",")' <<<"$board"; }
eq "next: skips held, PR-closed, other-assigned, Backlog and non-agent-ok issues" "$(nx)" "1,5,6,7"
board=$(jq -sc '[{data:{organization:{projectV2:{items:{nodes:.}}}}}]' < <(
  item 6 Ready '[]' '["agent-ok"]' P2; item 5 Ready '[]' '["agent-ok"]' ""; item 4 Ready '[]' '["agent-ok"]' P0; item 1 Ready '[]' '["agent-ok"]' P2))
eq "next: priority (none last), then age" "$(nx)" "4,1,6,5"

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
  load_claim() { # API_ERR names a claim whose read fails like the API does
    [[ ${API_ERR:-} != "$1" ]] || die "cannot read the claim on #$1 (API error); retry"
    C=$(jq -c --arg n "$1" '.[] | select((.issue | tostring) == $n)' <<<"$FIX"); [[ -n $C ]] || return 1
  }
  issue_json() { echo "${ISSUE_FIX:-{\"state\":\"open\",\"pull\":false,\"labels\":[\"agent-ok\"],\"assignees\":[]\}}"; }
  open_pr_closers() { echo "${CLOSERS_FIX:-[]}"; }
  if [[ -z ${REAL_WRITE:-} ]]; then
    write_claim() { # logs the write and, unless LOSE names the ref, stores it in FIX
      echo "write $1 $2 holder=$3 pr=$5 by=${8:-}" >>"$WRITES"
      [[ ${LOSE:-} != "$1" ]] || return 1
      FIX=$(jq -c --arg n "$1" --argjson c "$(cl "${1#adr-}" "$2" "$3" "$4" "$5")" '[.[] | select((.issue | tostring) != $n)] + [$c]' <<<"$FIX")
    }
  fi
  assign() { echo "assign $1 $2" >>"$WRITES"; }
  unassign() { [[ ${UNASSIGN_ERR:-} != "$1" ]] || die "cannot unassign $2 from #$1"; echo "unassign $1 $2" >>"$WRITES"; }
  pr_meta() { echo "$PR_FIX"; }
  pr_added_adr_files() { echo "${ADR_FILES:-}"; }
  gh() { # with REAL_WRITE: the real write_claim; PATCH_ERR is what GitHub answers to the ref PATCH
    case $* in
      *issues/*--jq\ .state*) echo open ;;
      *git/commits*) echo newsha ;;
      *"-X PATCH"*) if [[ -n ${PATCH_ERR:-} ]]; then echo "$PATCH_ERR"; return 1; fi ;;
      *) echo 1 ;;
    esac
  }
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
cl() { jq -nc --argjson n "$1" --arg s "$2" --arg h "$3" --arg u "$4" --arg p "${5:-}" '{issue:$n,state:$s,holder:$h,human:$u,pr:$p,note:"",released_by:"",sha:"sha\($n)",at:"2026-11-09T10:00:00Z"}'; }
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
eq "take: already yours warns a second session in the lane" \
  "$(grep -c '^claim: warning: #10 is already held by alice#l10 since 2026-11-09T10:00:00Z; if another session took it, stop and use your own --lane$' <<<"$r")" 1
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
FIX=$(fix </dev/null)
r=$(API_ERR=11 run take 10 11)
eq "take: an API error on a later issue is an error" "$(rc_of "$r")" 1
eq "take: an API error rolls back and unassigns the taken ones" "$(paste -sd';' "$WRITES")" \
  "write 10 held holder=alice#main pr= by=;assign 10 alice;write 10 released holder=alice#main pr= by=alice#main;unassign 10 alice"
FIX=$(cl 10 held alice#main alice | fix)
r=$(API_ERR=11 run take 10 11)
eq "take: a claim held before the call stays held and assigned on a later failure" "$(rc_of "$r"),$(wc -l <"$WRITES" | tr -d ' ')" "1,0"
FIX=$(cl 12 held bob#main bob | fix)
r=$(UNASSIGN_ERR=10 run take 10 11 12)
eq "take: a failing rollback step keeps the first exit code" "$(rc_of "$r")" 2
eq "take: a failing rollback step is reported" "$(grep -c 'rollback of #10 failed' <<<"$r")" 1
eq "take: a failing rollback step does not stop the next" "$(grep -c '^unassign 11 alice' "$WRITES")" 1
FIX=$(cl 10 released bob#main bob | fix)
r=$(REAL_WRITE=1 PATCH_ERR='gh: Update is not a fast forward (HTTP 422)' run take 10)
eq "take: a lost race on the PATCH of a released ref is exit 2" "$(rc_of "$r"),$(grep -c 'lost the race for #10' <<<"$r")" "2,1"
r=$(REAL_WRITE=1 PATCH_ERR='gh: Repository rule violations found (HTTP 422)' run take 10)
eq "take: another 422 on the PATCH is an error, not a race" "$(rc_of "$r")" 1
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
r=$(PR_FIX='{"author":"alice","body":"Closes #7, closes #8","state":"OPEN","merged":false,"closers":[9]}' run release-pr 5)
eq "release-pr: an open PR is refused" "$(rc_of "$r"):$(grep -c '^write' "$WRITES")" "1:0"
r=$(PR_FIX='{"author":"alice","body":"Closes #7, closes #8","state":"MERGED","merged":true,"closers":[9]}' run release-pr 5)
eq "release-pr: author's and pointing claims released" "$(grep '^write' "$WRITES" | awk '{print $2}' | paste -sd, -)" "7,9"
eq "release-pr: a merge does not unassign" "$(grep -c unassign "$WRITES")" 0
eq "release-pr: the note old beat loops recognise" "$(grep -c 'by=pr-close' "$WRITES")" 2
r=$(CLOSERS_FIX='[{"number":6,"author":"alice","issues":[7]}]' PR_FIX='{"author":"alice","body":"Closes #7","state":"CLOSED","merged":false,"closers":[]}' run release-pr 5)
eq "release-pr: an unmerged close keeps a claim another open PR closes" "$(grep -c '^write' "$WRITES")" 0
r=$(PR_FIX='{"author":"alice","body":"Closes #7","state":"CLOSED","merged":false,"closers":[]}' run release-pr 5)
eq "release-pr: an unmerged close releases and unassigns" "$(paste -sd';' "$WRITES")" "write 7 released holder=alice#l7 pr= by=pr-close;unassign 7 alice"

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
r=$(run release-merged 1111111 nothex)
eq "release-merged: needs a commit sha" "$(rc_of "$r")" 1

exit "$fail"
