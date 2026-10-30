#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Work claims (ADR 0082): several humans' coding agents must never pick up the same issue.
#
# The lock is a branch `claim/<issue>` created through the GitHub API. Creating a ref fails when it
# exists and a non-forced update must fast-forward, so ref creation and update are a compare-and-set.
# The ref points at an empty-tree commit whose message records the claim (holder, human, lease_until,
# pr, note, state). Heartbeat, release and takeover append a commit (fast-forward). Claim refs are
# never deleted. Mirrors for humans (assignee, Project Status, one marked comment) are best effort:
# the ref is the truth.
#
# Usage:
#   claim.sh next [--lane]            Ready, agent-ok, current-milestone, unblocked, unclaimed issues
#   claim.sh take <N> [--takeover]    claim issue N (--takeover: the holder's lease expired + grace)
#   claim.sh beat <N> [--pr P]        extend the lease (every 30 min and before long steps)
#   claim.sh review <N>               the PR is ready: Status In Review, lease extended
#   claim.sh release <N> --note TEXT  hand the issue back (keep the branch and draft PR)
#   claim.sh block <N> <M>            N waits for issue M: add "blocked by", release, Status Ready
#   claim.sh show <N> | mine | limits
#   claim.sh sweep | check-pr <PR> | release-pr <PR> | update-oldest     (used by claims.yml)
#
# Exit codes:
#   0  success
#   1  error (usage, API failure, missing tool, check failed)
#   2  refused: the issue is claimed by someone else, not takeable yet, or a WIP cap is reached.
#      Pick another issue.
#   3  lost claim: the claim is not yours (taken over, released, or gone). Stop work on the issue
#      and tell your human.
#
# Environment (all optional):
#   KOINON_CLAIM_REPO           owner/name (default: the current repository)
#   KOINON_CLAIM_PROJECT        project number (default 1; "none" skips every board write)
#   KOINON_CLAIM_PROJECT_OWNER  project owner (default: the repository owner)
#   KOINON_CLAIM_HUMAN          the human's GitHub login (default: the authenticated user)
#   KOINON_CLAIM_MILESTONE      milestone title for `next` (default: the open one with the earliest
#                               due date, else the lowest number)
#   KOINON_CLAIM_LANES          lanes file (default .github/lanes.yaml)
#   CLAUDE_SESSION_ID / KOINON_AGENT_SESSION   the session id (else generated once per worktree)
#   KOINON_CLAIM_LEASE_SECONDS (7200), _GRACE_SECONDS (900), _HUMAN_CAP (4), _SESSION_CAP (1),
#   _STALE_RELEASE_SECONDS (7200: the sweeper releases this long after the lease ran out)
set -euo pipefail

LEASE=${KOINON_CLAIM_LEASE_SECONDS:-7200}
GRACE=${KOINON_CLAIM_GRACE_SECONDS:-900}
HUMAN_CAP=${KOINON_CLAIM_HUMAN_CAP:-4}
SESSION_CAP=${KOINON_CLAIM_SESSION_CAP:-1}
RELEASE_AFTER=${KOINON_CLAIM_STALE_RELEASE_SECONDS:-7200}
MARKER='<!-- koinon-claim -->'
EMPTY_TREE=4b825dc642cb6eb9a060e54bf8d69288fbee4904

# ---------------------------------------------------------------------------------------------
# Pure functions (no network); hack/claim_test.sh sources this file with CLAIM_LIB_ONLY=1.
# ---------------------------------------------------------------------------------------------

die() { echo "claim: $*" >&2; exit "${CLAIM_EXIT:-1}"; }
warn() { echo "claim: warning: $*" >&2; }

# An epoch -> RFC 3339 UTC, and back. GNU and BSD date both work.
if date -u -d @0 +%s >/dev/null 2>&1; then
  epoch_to_iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }
  iso_to_epoch() { date -u -d "$1" +%s; }
  http_date_to_epoch() { LC_ALL=C date -u -d "$1" +%s; }
else
  epoch_to_iso() { date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }
  iso_to_epoch() { date -u -j -f %Y-%m-%dT%H:%M:%SZ "$1" +%s; }
  http_date_to_epoch() { LC_ALL=C date -u -j -f '%a, %d %b %Y %H:%M:%S GMT' "$1" +%s; }
fi

# claim_format ISSUE STATE HOLDER HUMAN LEASE_UNTIL PR NOTE -> the commit message.
claim_format() {
  local note=${7//$'\n'/ }
  note=${note//$'\r'/ }
  printf 'claim #%s: %s\n\nholder: %s\nhuman: %s\nlease_until: %s\npr: %s\nnote: %s\nstate: %s\n' \
    "$1" "$2" "$3" "$4" "$5" "$6" "$note" "$2"
}

# JQ_PARSE: jq definition turning a claim commit message (string) into an object.
# shellcheck disable=SC2016
JQ_PARSE='def claim_parse:
  ([split("\n")[] | capture("^(?<k>holder|human|lease_until|pr|note|state): ?(?<v>.*)$")]
    | map({(.k): .v}) | add // {})
  | {state: (.state // ""), holder: (.holder // ""), human: (.human // ""),
     lease_until: (.lease_until // ""), pr: (.pr // ""), note: (.note // "")};'

# claim_parse MESSAGE -> compact JSON {state,holder,human,lease_until,pr,note}.
claim_parse() { printf '%s' "$1" | jq -Rsc "$JQ_PARSE claim_parse"; }

# claim_status STATE LEASE_EPOCH NOW -> released | active | grace | takeover
# grace: lease ran out less than GRACE ago; takeover: the lease and the grace both ran out.
claim_status() {
  local state=$1 lease=$2 now=$3
  if [[ $state != held ]]; then echo released; return; fi
  if ((now <= lease)); then echo active
  elif ((now <= lease + GRACE)); then echo grace
  else echo takeover; fi
}

# lane_components LOGIN FILE -> one Component per line. File lines look like `login: a, b/c`;
# `#` starts a comment. Logins compare case-insensitively.
lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

lane_components() {
  local login file=$2 line key val
  login=$(lower "$1")
  [[ -r $file ]] || return 0
  while IFS= read -r line || [[ -n $line ]]; do
    line=${line%%#*}
    [[ $line == *:* ]] || continue
    key=${line%%:*}
    key=${key//[[:space:]]/}
    [[ $(lower "$key") == "$login" ]] || continue
    val=${line#*:}
    printf '%s\n' "$val" | tr ',' '\n' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | grep -v '^$' || true
  done <"$file"
}

# holder_login HOLDER, holder_session HOLDER: holder is `<login>/<host>/<session>`.
holder_login() { echo "${1%%/*}"; }
holder_session() { echo "${1##*/}"; }

# pr_exempt TITLE AUTHOR LABELS(comma separated) -> exit 0 when the PR needs no `Closes #N`.
pr_exempt() {
  local title=$1 author=$2 labels=",$3,"
  [[ $title =~ ^(chore|ci|deps)(\([^\)]*\))?!?: ]] && return 0
  [[ $author == dependabot* || $author == app/dependabot ]] && return 0
  [[ $labels == *,unclaimed-ok,* ]] && return 0
  return 1
}

# closing_issues BODY -> issue numbers named by closing keywords, one per line.
closing_issues() {
  printf '%s\n' "$1" | grep -oiE '\b(close[sd]?|fix(e[sd])?|resolve[sd]?)[: ]+#[0-9]+' | grep -oE '[0-9]+$' | sort -un || true
}

# JQ_PRIORITY: sort key for `next`: P0..P3, anything else last.
# shellcheck disable=SC2016
JQ_RANK='def rank: ({"P0":0,"P1":1,"P2":2,"P3":3}[.] // 9);'

# ---------------------------------------------------------------------------------------------
# GitHub plumbing
# ---------------------------------------------------------------------------------------------

REPO=${KOINON_CLAIM_REPO:-}
PROJECT=${KOINON_CLAIM_PROJECT:-1}
PROJECT_OWNER=${KOINON_CLAIM_PROJECT_OWNER:-}
OWNER=
NAME=
init_repo() {
  [[ -n $REPO ]] || REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
  OWNER=${REPO%%/*}
  NAME=${REPO#*/}
  PROJECT_OWNER=${PROJECT_OWNER:-$OWNER}
}

# gh_try ARGS...: run `gh api`, keep output in TRY_OUT and exit status in TRY_RC.
TRY_OUT=
TRY_RC=0
gh_try() {
  TRY_RC=0
  TRY_OUT=$(gh api "$@" 2>&1) || TRY_RC=$?
}

server_now() {
  if [[ -n ${KOINON_CLAIM_NOW_EPOCH:-} ]]; then echo "$KOINON_CLAIM_NOW_EPOCH"; return; fi
  local d
  d=$(gh api -i /rate_limit 2>/dev/null | tr -d '\r' | sed -n 's/^[Dd]ate: *//p' | head -1)
  [[ -n $d ]] || die "cannot read the server time (Date header)"
  http_date_to_epoch "$d"
}

human_login() {
  if [[ -n ${KOINON_CLAIM_HUMAN:-} ]]; then echo "$KOINON_CLAIM_HUMAN"; else gh api user --jq .login; fi
}

session_id() {
  local s=${CLAUDE_SESSION_ID:-${KOINON_AGENT_SESSION:-}}
  if [[ -z $s ]]; then
    s=$(git config --get koinon.claim-session 2>/dev/null || true)
    if [[ -z $s ]]; then
      s="s$(date +%s)-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
      git config --worktree koinon.claim-session "$s" 2>/dev/null ||
        git config koinon.claim-session "$s" 2>/dev/null || warn "could not store the session id; it changes per call"
    fi
  fi
  echo "${s//[^A-Za-z0-9._-]/-}"
}

my_holder() { echo "$(human_login)/$(hostname -s 2>/dev/null || echo host)/$(session_id)"; }

# read_claim N -> JSON {issue,sha,state,holder,human,lease_until,pr,note}; exit 1 when no claim ref.
read_claim() {
  local out
  # shellcheck disable=SC2016
  out=$(gh api graphql -F o="$OWNER" -F n="$NAME" -F q="refs/heads/claim/$1" -f query='
    query($o:String!,$n:String!,$q:String!){repository(owner:$o,name:$n){ref(qualifiedName:$q){target{... on Commit{oid message}}}}}' \
    --jq '.data.repository.ref.target // empty | [.oid, .message] | @json') || die "cannot read claim/$1"
  [[ -n $out ]] || return 1
  jq -c --argjson n "$1" "$JQ_PARSE"'
    . as [$sha, $msg] | ($msg | claim_parse) + {issue: $n, sha: $sha}' <<<"$out"
}

# list_claims -> JSON array of every claim (numeric claim/<N> refs only).
list_claims() {
  # shellcheck disable=SC2016
  gh api graphql --paginate -F o="$OWNER" -F n="$NAME" -f query='
    query($o:String!,$n:String!,$endCursor:String){repository(owner:$o,name:$n){
      refs(refPrefix:"refs/heads/claim/",first:100,after:$endCursor){
        pageInfo{hasNextPage endCursor} nodes{name target{... on Commit{oid message}}}}}}' |
    jq -s "$JQ_PARSE"'
      [.[].data.repository.refs.nodes[] | select(.name | test("^[0-9]+$"))
        | (.target.message | claim_parse) + {issue: (.name | tonumber), sha: .target.oid}]'
}

# write_claim ISSUE STATE HOLDER HUMAN LEASE PR NOTE [PARENT_SHA]
# Creates the commit, then the ref (no parent) or fast-forwards it. Returns 0 won, 1 lost the
# compare-and-set (HTTP 422), exits on other API errors.
write_claim() {
  local issue=$1 parent=${8:-} msg sha
  msg=$(claim_format "$issue" "$2" "$3" "$4" "$5" "$6" "$7")
  local args=(-X POST "repos/$REPO/git/commits" -f "message=$msg" -f "tree=$EMPTY_TREE")
  [[ -z $parent ]] || args+=(-f "parents[]=$parent")
  sha=$(gh api "${args[@]}" --jq .sha) || die "cannot create the claim commit"
  if [[ -z $parent ]]; then
    gh_try -X POST "repos/$REPO/git/refs" -f "ref=refs/heads/claim/$issue" -f "sha=$sha"
  else
    gh_try -X PATCH "repos/$REPO/git/refs/heads/claim/$issue" -f "sha=$sha" -F force=false
  fi
  if ((TRY_RC == 0)); then return 0; fi
  if [[ $TRY_OUT == *"HTTP 422"* ]]; then return 1; fi
  die "claim write failed: $TRY_OUT"
}

# issue_json N -> {number,state,title,labels[],milestone}
issue_json() {
  gh api "repos/$REPO/issues/$1" --jq '{number, state, title, pull: (.pull_request != null),
    labels: [.labels[].name], milestone: (.milestone.title // "")}'
}

# find_pr N -> number of the open PR whose body closes issue N (empty if none).
find_pr() {
  gh pr list -R "$REPO" --state open --limit 100 --json number,body |
    jq -r --arg n "$1" '.[] | select(.body // "" | test("(?i)\\b(close[sd]?|fix(e[sd])?|resolve[sd]?)[: ]+#" + $n + "\\b")) | .number' | head -1
}

# ---- board mirrors (best effort) ----

PROJECT_META=
project_meta() {
  [[ $PROJECT != none ]] || return 1
  if [[ -z $PROJECT_META ]]; then
    # shellcheck disable=SC2016
    PROJECT_META=$(gh api graphql -F o="$PROJECT_OWNER" -F n="$PROJECT" -f query='
      query($o:String!,$n:Int!){organization(login:$o){projectV2(number:$n){id fields(first:50){nodes{
        ... on ProjectV2SingleSelectField{id name options{id name}} ... on ProjectV2Field{id name}}}}}}' \
      --jq .data.organization.projectV2 2>/dev/null) || { warn "cannot read project $PROJECT_OWNER/$PROJECT"; PROJECT=none; return 1; }
  fi
}

# set_status N OPTION: set the Project Status single-select; warns and returns 1 on any problem.
set_status() {
  project_meta || return 1
  local pid fid oid item cid
  pid=$(jq -r .id <<<"$PROJECT_META")
  fid=$(jq -r '.fields.nodes[] | select(.name == "Status") | .id' <<<"$PROJECT_META")
  oid=$(jq -r --arg o "$2" '.fields.nodes[] | select(.name == "Status") | .options[]? | select(.name == $o) | .id' <<<"$PROJECT_META")
  if [[ -z $fid || -z $oid ]]; then warn "project has no Status option '$2'"; return 1; fi
  # shellcheck disable=SC2016
  local q='query($o:String!,$n:String!,$i:Int!){repository(owner:$o,name:$n){issue(number:$i){id projectItems(first:20){nodes{id project{id}}}}}}'
  local res
  res=$(gh api graphql -F o="$OWNER" -F n="$NAME" -F i="$1" -f query="$q" --jq .data.repository.issue 2>/dev/null) ||
    { warn "cannot read issue #$1 project items"; return 1; }
  cid=$(jq -r .id <<<"$res")
  item=$(jq -r --arg p "$pid" '.projectItems.nodes[] | select(.project.id == $p) | .id' <<<"$res" | head -1)
  if [[ -z $item ]]; then
    # shellcheck disable=SC2016
    item=$(gh api graphql -f p="$pid" -f c="$cid" -f query='mutation($p:ID!,$c:ID!){addProjectV2ItemById(input:{projectId:$p,contentId:$c}){item{id}}}' \
      --jq .data.addProjectV2ItemById.item.id 2>/dev/null) || { warn "cannot add #$1 to the project"; return 1; }
  fi
  # shellcheck disable=SC2016
  gh api graphql -f p="$pid" -f i="$item" -f f="$fid" -f v="$oid" -f query='
    mutation($p:ID!,$i:ID!,$f:ID!,$v:String!){updateProjectV2ItemFieldValue(input:{projectId:$p,itemId:$i,fieldId:$f,value:{singleSelectOptionId:$v}}){projectV2Item{id}}}' \
    >/dev/null 2>&1 || { warn "cannot set Status '$2' on #$1"; return 1; }
}

assign() { gh api -X POST "repos/$REPO/issues/$1/assignees" -f "assignees[]=$2" >/dev/null 2>&1 || warn "cannot assign $2 to #$1"; }
unassign() { gh api -X DELETE "repos/$REPO/issues/$1/assignees" -f "assignees[]=$2" >/dev/null 2>&1 || warn "cannot unassign $2 from #$1"; }
add_label() { gh api -X POST "repos/$REPO/issues/$1/labels" -f "labels[]=$2" >/dev/null 2>&1 || warn "cannot label #$1 $2"; }
drop_label() { gh api -X DELETE "repos/$REPO/issues/$1/labels/$2" >/dev/null 2>&1 || true; }

# upsert_comment N BODY: the one marked comment on the issue, edited in place.
upsert_comment() {
  local id
  id=$(gh api --paginate "repos/$REPO/issues/$1/comments" --jq ".[] | select(.body | startswith(\"$MARKER\")) | .id" 2>/dev/null | head -1 || true)
  if [[ -n $id ]]; then
    gh api -X PATCH "repos/$REPO/issues/comments/$id" -f "body=$2" >/dev/null 2>&1 || warn "cannot edit the claim comment on #$1"
  else
    gh api -X POST "repos/$REPO/issues/$1/comments" -f "body=$2" >/dev/null 2>&1 || warn "cannot comment on #$1"
  fi
}

# claim_comment STATE HOLDER HUMAN LEASE PR NOTE
claim_comment() {
  local pr=${5:-none}
  [[ $pr == "" ]] && pr=none
  # shellcheck disable=SC2016
  printf '%s\n**Work claim: %s** by `%s`\n\n- holder: `%s`\n- lease until: %s\n- pull request: %s\n- note: %s\n\n_Managed by `hack/claim.sh` (ADR 0082); the claim ref `claim/<issue>` is the source of truth._\n' \
    "$MARKER" "$1" "$3" "$2" "${4:-n/a}" "$pr" "${6:-none}"
}

# ---------------------------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------------------------

active_counts() { # CLAIMS_JSON NOW HUMAN SESSION -> "human_count session_count"
  jq -r --argjson now "$2" --arg h "$3" --arg s "$4" '
    [.[] | select(.state == "held" and (.lease_until | length > 0))] as $held
    | ($held | map(select((.lease_until | fromdateiso8601) >= $now))) as $act
    | "\($act | map(select(.human == $h)) | length) \($act | map(select((.holder | split("/") | last) == $s)) | length)"' <<<"$1"
}

cmd_limits() {
  local claims now human sess counts
  claims=$(list_claims); now=$(server_now); human=$(human_login); sess=$(session_id)
  counts=$(active_counts "$claims" "$now" "$human" "$sess")
  echo "human:    $human (${counts% *} of $HUMAN_CAP held claims)"
  echo "session:  $sess (${counts#* } of $SESSION_CAP)"
  echo "lease:    ${LEASE}s; heartbeat every 1800s; takeover after lease + ${GRACE}s grace"
  echo "sweeper:  stale mark after lease + ${GRACE}s; release ${RELEASE_AFTER}s after the lease ran out"
}

cmd_mine() {
  local claims now human
  claims=$(list_claims); now=$(server_now); human=$(human_login)
  jq -r --argjson now "$now" --arg h "$human" '
    .[] | select(.state == "held" and .human == $h)
    | "#\(.issue)\t\(if (.lease_until | fromdateiso8601) >= $now then "active" else "expired" end)\tlease \(.lease_until)\tpr \(.pr)\t\(.holder)"' <<<"$claims"
}

cmd_show() {
  local c now status lease
  c=$(read_claim "$1") || { echo "#$1: no claim"; return 0; }
  now=$(server_now)
  lease=0
  [[ $(jq -r .lease_until <<<"$c") == "" ]] || lease=$(iso_to_epoch "$(jq -r .lease_until <<<"$c")")
  status=$(claim_status "$(jq -r .state <<<"$c")" "$lease" "$now")
  jq -r --arg st "$status" '"#\(.issue): \($st)\n  state:       \(.state)\n  holder:      \(.holder)\n  human:       \(.human)\n  lease_until: \(.lease_until)\n  pr:          \(.pr)\n  note:        \(.note)\n  ref sha:     \(.sha)"' <<<"$c"
}

cmd_next() {
  local lane=0 claims comps="" ms human json held lanes
  [[ ${1:-} == --lane ]] && lane=1
  [[ $PROJECT != none ]] || die "next needs the project board (KOINON_CLAIM_PROJECT)"
  ms=${KOINON_CLAIM_MILESTONE:-}
  if [[ -z $ms ]]; then
    ms=$(gh api "repos/$REPO/milestones?state=open" --jq 'sort_by(.due_on // "9999", .number) | .[0].title // empty')
  fi
  if ((lane)); then
    human=$(human_login)
    lanes=${KOINON_CLAIM_LANES:-$(git rev-parse --show-toplevel)/.github/lanes.yaml}
    comps=$(lane_components "$human" "$lanes" | paste -sd'|' -)
    [[ -n $comps ]] || warn "no lane for $human in $lanes; showing every Component"
  fi
  claims=$(list_claims)
  held=$(jq -c '[.[] | select(.state == "held") | .issue]' <<<"$claims")
  # shellcheck disable=SC2016
  json=$(gh api graphql --paginate -F o="$PROJECT_OWNER" -F n="$PROJECT" -f query='
    query($o:String!,$n:Int!,$endCursor:String){organization(login:$o){projectV2(number:$n){
      items(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{
        status:fieldValueByName(name:"Status"){... on ProjectV2ItemFieldSingleSelectValue{name}}
        priority:fieldValueByName(name:"Priority"){... on ProjectV2ItemFieldSingleSelectValue{name}}
        component:fieldValueByName(name:"Component"){... on ProjectV2ItemFieldSingleSelectValue{name}}
        content{... on Issue{number title state createdAt repository{nameWithOwner} milestone{title}
          labels(first:50){nodes{name}}}}}}}}}' |
    jq -s --arg repo "$REPO" --arg ms "$ms" --arg comps "$comps" --argjson held "$held" "$JQ_RANK"'
      [.[].data.organization.projectV2.items.nodes[]
        | select(.content.number != null and .content.state == "OPEN" and .content.repository.nameWithOwner == $repo)
        | select(.status.name == "Ready")
        | select([.content.labels.nodes[].name] | index("agent-ok"))
        | select($ms == "" or .content.milestone.title == $ms)
        | select(($comps == "") or (.component.name // "" | . == "" or test("^(" + $comps + ")$")))
        | select(.content.number as $n | $held | index($n) | not)
        | {n: .content.number, p: (.priority.name // ""), c: (.component.name // ""), t: .content.title, at: .content.createdAt}]
      | sort_by([(.p | rank), .at])')
  local n blockers
  while IFS=$'\t' read -r n p c t; do
    blockers=$(gh api "repos/$REPO/issues/$n/dependencies/blocked_by" --jq '[.[] | select(.state == "open")] | length' 2>/dev/null || echo 0)
    [[ $blockers == 0 ]] || continue
    printf '#%s\t%s\t%s\t%s\n' "$n" "${p:--}" "${c:--}" "$t"
  done < <(jq -r '.[] | [.n, .p, .c, .t] | @tsv' <<<"$json")
}

warn_area_overlap() { # N CLAIMS_JSON NOW
  local labels l other
  labels=$(gh api "repos/$REPO/issues/$1/labels" --jq '.[].name | select(startswith("area:"))' 2>/dev/null || true)
  for l in $labels; do
    for other in $(jq -r --argjson now "$3" --argjson me "$1" '.[] | select(.state == "held" and .issue != $me and (.lease_until | fromdateiso8601) >= $now) | .issue' <<<"$2"); do
      if gh api "repos/$REPO/issues/$other/labels" --jq '.[].name' 2>/dev/null | grep -qxF "$l"; then
        warn "#$other is also claimed and shares $l; coordinate on files you both touch"
      fi
    done
  done
}

cmd_take() {
  local n=${1:-} takeover=0
  [[ $n =~ ^[0-9]+$ ]] || die "usage: take <N> [--takeover]"
  [[ ${2:-} == --takeover ]] && takeover=1
  local issue human holder sess claims now counts existing parent="" status lease
  issue=$(issue_json "$n") || die "cannot read issue #$n"
  [[ $(jq -r .state <<<"$issue") == open ]] || { CLAIM_EXIT=2 die "#$n is closed"; }
  [[ $(jq -r .pull <<<"$issue") == false ]] || die "#$n is a pull request"
  human=$(human_login); holder=$(my_holder); sess=$(session_id)
  claims=$(list_claims); now=$(server_now)
  if existing=$(read_claim "$n"); then
    parent=$(jq -r .sha <<<"$existing")
    lease=0
    [[ $(jq -r .lease_until <<<"$existing") == "" ]] || lease=$(iso_to_epoch "$(jq -r .lease_until <<<"$existing")")
    status=$(claim_status "$(jq -r .state <<<"$existing")" "$lease" "$now")
    if [[ $(jq -r .holder <<<"$existing") == "$holder" && $status != released ]]; then
      echo "#$n is already yours; extending the lease"
      cmd_beat "$n"; return
    fi
    case $status in
      active | grace)
        CLAIM_EXIT=2 die "#$n is held by $(jq -r .holder <<<"$existing") until $(jq -r .lease_until <<<"$existing") (takeover after +${GRACE}s grace). Pick another issue." ;;
      takeover)
        ((takeover)) || CLAIM_EXIT=2 die "#$n's lease expired ($(jq -r .lease_until <<<"$existing")); use: take $n --takeover" ;;
      released) ;;
    esac
  elif ((takeover)); then
    echo "#$n has no claim; taking it normally"
  fi
  counts=$(active_counts "$claims" "$now" "$human" "$sess")
  ((${counts#* } < SESSION_CAP)) || CLAIM_EXIT=2 die "this session already holds ${counts#* } claim(s) (cap $SESSION_CAP). Finish or release one first."
  ((${counts% *} < HUMAN_CAP)) || CLAIM_EXIT=2 die "$human already holds ${counts% *} claims (cap $HUMAN_CAP)."
  local lease_until pr
  lease_until=$(epoch_to_iso $((now + LEASE)))
  pr=$(find_pr "$n")
  if ! write_claim "$n" held "$holder" "$human" "$lease_until" "$pr" "" "$parent"; then
    CLAIM_EXIT=2 die "lost the race for #$n: another session claimed it first. Pick another issue."
  fi
  echo "claimed #$n as $holder until $lease_until"
  warn_area_overlap "$n" "$claims" "$now"
  [[ $(jq -r '.labels | index("agent-ok") | not' <<<"$issue") == false ]] || warn "#$n has no agent-ok label"
  [[ $(gh api "repos/$REPO/issues/$n/dependencies/blocked_by" --jq '[.[] | select(.state == "open")] | length' 2>/dev/null || echo 0) == 0 ]] || warn "#$n has open blockers"
  mirror_held "$n" "$human" "$holder" "$lease_until" "$pr" "" "In Progress"
}

mirror_held() { # N HUMAN HOLDER LEASE PR NOTE [STATUS]
  assign "$1" "$2"
  [[ -z ${7:-} ]] || set_status "$1" "$7" || true
  drop_label "$1" claim:stale
  upsert_comment "$1" "$(claim_comment held "$3" "$2" "$4" "$5" "$6")"
}

# require_mine N -> sets C (claim JSON), HOLDER, HUMAN, NOW; exit 3 unless the head claim is held by us.
require_mine() {
  HOLDER=$(my_holder); HUMAN=$(human_login)
  if ! C=$(read_claim "$1"); then CLAIM_EXIT=3 die "no claim on #$1: stop and tell your human"; fi
  if [[ $(jq -r .state <<<"$C") != held || $(jq -r .holder <<<"$C") != "$HOLDER" ]]; then
    CLAIM_EXIT=3 die "#$1 is not yours any more ($(jq -r '.state + " by " + .holder' <<<"$C")): stop work on it and tell your human"
  fi
  NOW=$(server_now)
}

cmd_beat() {
  local n=${1:-} pr=""
  [[ $n =~ ^[0-9]+$ ]] || die "usage: beat <N> [--pr P]"
  [[ ${2:-} == --pr ]] && pr=${3:-}
  require_mine "$n"
  [[ -n $pr ]] || pr=$(jq -r .pr <<<"$C")
  [[ -n $pr ]] || pr=$(find_pr "$n")
  local lease_until note
  lease_until=$(epoch_to_iso $((NOW + LEASE)))
  note=$(jq -r .note <<<"$C")
  if ! write_claim "$n" held "$HOLDER" "$HUMAN" "$lease_until" "$pr" "$note" "$(jq -r .sha <<<"$C")"; then
    CLAIM_EXIT=3 die "lost #$n while renewing it: stop work and tell your human"
  fi
  echo "#$n lease extended to $lease_until"
  drop_label "$n" claim:stale
  upsert_comment "$n" "$(claim_comment held "$HOLDER" "$HUMAN" "$lease_until" "$pr" "$note")"
}

cmd_review() {
  local n=${1:-}
  [[ $n =~ ^[0-9]+$ ]] || die "usage: review <N>"
  require_mine "$n"
  local pr lease_until
  pr=$(jq -r .pr <<<"$C"); [[ -n $pr ]] || pr=$(find_pr "$n")
  [[ -n $pr ]] || warn "no open pull request closes #$n"
  lease_until=$(epoch_to_iso $((NOW + LEASE)))
  write_claim "$n" held "$HOLDER" "$HUMAN" "$lease_until" "$pr" "in review" "$(jq -r .sha <<<"$C")" ||
    CLAIM_EXIT=3 die "lost #$n: stop work and tell your human"
  echo "#$n is in review (pull request ${pr:-none}); the claim is released when it merges or closes"
  set_status "$n" "In Review" || true
  upsert_comment "$n" "$(claim_comment held "$HOLDER" "$HUMAN" "$lease_until" "$pr" "in review")"
}

# do_release N NOTE [force] [merged]: write state released and update the mirrors.
do_release() {
  local n=$1 note=$2 force=${3:-0} merged=${4:-0} c holder human now
  if ! c=$(read_claim "$n"); then CLAIM_EXIT=3 die "no claim on #$n"; fi
  holder=$(my_holder)
  if ((!force)) && [[ $(jq -r .state <<<"$c") != held || $(jq -r .holder <<<"$c") != "$holder" ]]; then
    CLAIM_EXIT=3 die "#$n is not yours any more: stop work on it and tell your human"
  fi
  if [[ $(jq -r .state <<<"$c") == released ]]; then echo "#$n is already released"; return 0; fi
  human=$(jq -r .human <<<"$c"); now=$(server_now)
  write_claim "$n" released "$(jq -r .holder <<<"$c")" "$human" "$(epoch_to_iso "$now")" "$(jq -r .pr <<<"$c")" "$note" "$(jq -r .sha <<<"$c")" ||
    CLAIM_EXIT=3 die "#$n changed while releasing it: re-read with show"
  echo "released #$n"
  drop_label "$n" claim:stale
  if ((!merged)) && [[ $(gh api "repos/$REPO/issues/$n" --jq .state 2>/dev/null) == open ]]; then
    unassign "$n" "$human"
    set_status "$n" Ready || true
  fi
  upsert_comment "$n" "$(claim_comment released "$(jq -r .holder <<<"$c")" "$human" "" "$(jq -r .pr <<<"$c")" "$note")"
}

cmd_release() {
  local n=${1:-} note="" force=0
  [[ $n =~ ^[0-9]+$ ]] || die "usage: release <N> --note TEXT"
  shift
  while (($#)); do
    case $1 in
      --note) note=${2:-}; shift 2 ;;
      --force) force=1; shift ;;
      *) die "unknown option $1" ;;
    esac
  done
  [[ -n $note ]] || die "release needs --note \"<state, branch, next step>\""
  do_release "$n" "$note" "$force"
}

cmd_block() {
  local n=${1:-} m=${2:-} id
  [[ $n =~ ^[0-9]+$ && $m =~ ^[0-9]+$ ]] || die "usage: block <N> <M>"
  require_mine "$n"
  id=$(gh api "repos/$REPO/issues/$m" --jq .id) || die "cannot read #$m"
  gh_try -X POST "repos/$REPO/issues/$n/dependencies/blocked_by" -F "issue_id=$id"
  if ((TRY_RC != 0)) && [[ $TRY_OUT != *"already"* && $TRY_OUT != *"HTTP 422"* ]]; then die "cannot add the dependency: $TRY_OUT"; fi
  echo "#$n is now blocked by #$m"
  do_release "$n" "blocked by #$m" 0
}

# ---- workflow commands ----

cmd_sweep() {
  local claims now n c lease live act pr pr_json last labels
  claims=$(list_claims); now=$(server_now)
  for n in $(jq -r '.[] | select(.state == "held") | .issue' <<<"$claims"); do
    c=$(jq -c --argjson n "$n" '.[] | select(.issue == $n)' <<<"$claims")
    lease=$(iso_to_epoch "$(jq -r .lease_until <<<"$c")")
    live=$lease
    pr=$(jq -r .pr <<<"$c"); [[ -n $pr ]] || pr=$(find_pr "$n")
    if [[ -n $pr ]]; then
      pr_json=$(gh pr view "$pr" -R "$REPO" --json state,isDraft,commits,comments 2>/dev/null || echo '{}')
      case $(jq -r '.state // ""' <<<"$pr_json") in
        MERGED | CLOSED) echo "#$n: pull request #$pr is closed; releasing"
          do_release "$n" "pull request #$pr closed" 1 1 || true; continue ;;
        OPEN) [[ $(jq -r '.isDraft' <<<"$pr_json") == true ]] || { echo "#$n: pull request #$pr is in review; skipping"; continue; } ;;
      esac
      last=$(jq -r '[(.commits[-1].committedDate // empty), (.comments[-1].createdAt // empty)] | max // empty' <<<"$pr_json")
      if [[ -n $last ]]; then
        act=$(iso_to_epoch "$last")
        ((act + LEASE > live)) && live=$((act + LEASE))
      fi
    fi
    labels=$(gh api "repos/$REPO/issues/$n/labels" --jq '.[].name' 2>/dev/null || true)
    if ((now > live + RELEASE_AFTER)); then
      echo "#$n: no activity since $(epoch_to_iso "$live"); releasing"
      do_release "$n" "released by the sweeper: no heartbeat or pull request activity" 1 || true
    elif ((now > live + GRACE)); then
      if ! grep -qxF claim:stale <<<"$labels"; then
        echo "#$n: stale; marking"
        add_label "$n" claim:stale
        gh api -X POST "repos/$REPO/issues/$n/comments" -f "body=@$(jq -r .human <<<"$c") the claim on #$n ($(jq -r .holder <<<"$c")) ran out at $(epoch_to_iso "$live") with no heartbeat or pull request activity. If the work continues, run \`hack/claim.sh beat $n\`; otherwise \`hack/claim.sh release $n --note ...\`. The sweeper releases it at $(epoch_to_iso $((live + RELEASE_AFTER))) and keeps the draft pull request." >/dev/null || true
      fi
    else
      if grep -qxF claim:stale <<<"$labels"; then drop_label "$n" claim:stale; fi
    fi
  done
}

cmd_release_pr() {
  local p=${1:-} body merged n
  [[ $p =~ ^[0-9]+$ ]] || die "usage: release-pr <PR>"
  body=$(gh pr view "$p" -R "$REPO" --json body,mergedAt --jq '.body // ""')
  merged=0
  [[ -z $(gh pr view "$p" -R "$REPO" --json mergedAt --jq '.mergedAt // empty') ]] || merged=1
  for n in $(closing_issues "$body"); do
    if read_claim "$n" >/dev/null; then do_release "$n" "pull request #$p closed" 1 "$merged" || true; fi
  done
}

cmd_check_pr() {
  local p=${1:-} pr title author labels body n c fail=0 issues
  [[ $p =~ ^[0-9]+$ ]] || die "usage: check-pr <PR>"
  pr=$(gh pr view "$p" -R "$REPO" --json title,body,author,labels)
  title=$(jq -r .title <<<"$pr"); author=$(jq -r .author.login <<<"$pr"); body=$(jq -r '.body // ""' <<<"$pr")
  labels=$(jq -r '[.labels[].name] | join(",")' <<<"$pr")
  if pr_exempt "$title" "$author" "$labels"; then echo "claim check: exempt (chore/ci/deps title, Dependabot or unclaimed-ok)"; return 0; fi
  issues=$(closing_issues "$body")
  if [[ -z $issues ]]; then
    echo "::error::the pull request must reference its issue with 'Closes #N' (or have a chore/ci/deps title, or the unclaimed-ok label)"
    return 1
  fi
  for n in $issues; do
    if ! c=$(read_claim "$n"); then echo "::error::#$n has no claim; run hack/claim.sh take $n"; fail=1; continue; fi
    if [[ $(jq -r .state <<<"$c") != held ]]; then echo "::error::the claim on #$n is $(jq -r .state <<<"$c"); take it again"; fail=1; continue; fi
    if [[ $(lower "$author") != "$(jq -r '.human | ascii_downcase' <<<"$c")" ]]; then
      echo "::error::#$n is claimed for $(jq -r .human <<<"$c"), not the pull request author $author"; fail=1; continue
    fi
    echo "claim check: #$n is claimed for $author"
  done
  return "$fail"
}

cmd_update_oldest() {
  local prs inflight target
  prs=$(gh pr list -R "$REPO" --state open --limit 100 --json number,createdAt,isDraft,autoMergeRequest,mergeStateStatus,statusCheckRollup)
  inflight=$(jq -r '[.[] | select(.autoMergeRequest != null and .mergeStateStatus != "BEHIND"
    and ([.statusCheckRollup[]? | select((.status // "COMPLETED") != "COMPLETED" or .state == "PENDING")] | length > 0)) | .number] | first // empty' <<<"$prs")
  if [[ -n $inflight ]]; then echo "::notice::#$inflight is up to date and its checks are running; not updating another branch"; return 0; fi
  target=$(jq -r '[.[] | select(.autoMergeRequest != null and .mergeStateStatus == "BEHIND")] | sort_by(.createdAt) | first.number // empty' <<<"$prs")
  if [[ -z $target ]]; then echo "no auto-merge pull request is behind main"; return 0; fi
  gh api -X PUT "repos/$REPO/pulls/$target/update-branch" --silent
  echo "updated #$target with main"
}

usage() { sed -n '4,41p' "$0" | sed 's/^# \{0,1\}//'; }

main() {
  command -v gh >/dev/null || die "gh is required"
  command -v jq >/dev/null || die "jq is required"
  local cmd=${1:-}
  [[ -n $cmd ]] || { usage; exit 1; }
  shift || true
  case $cmd in
    -h | --help | help) usage; exit 0 ;;
  esac
  init_repo
  case $cmd in
    next) cmd_next "$@" ;;
    take) cmd_take "$@" ;;
    beat) cmd_beat "$@" ;;
    review) cmd_review "$@" ;;
    release) cmd_release "$@" ;;
    block) cmd_block "$@" ;;
    show) cmd_show "${1:?usage: show <N>}" ;;
    mine) cmd_mine ;;
    limits) cmd_limits ;;
    sweep) cmd_sweep ;;
    check-pr) cmd_check_pr "$@" ;;
    release-pr) cmd_release_pr "$@" ;;
    update-oldest) cmd_update_oldest ;;
    *) usage; exit 1 ;;
  esac
}

if [[ -z ${CLAIM_LIB_ONLY:-} ]]; then main "$@"; fi
