#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Work claims (ADR 0082, reduced to the lock by ADR 0104): two lanes must never take the same issue.
#
# The lock is the ref claim/<N>, written through the GitHub API as a compare-and-set (creating a ref
# fails when it exists; a non-forced update must fast-forward). It is the only truth: an empty-tree
# commit whose message records holder, human, pr, note and state. A release appends a commit; refs are
# never deleted. The one mirror is the issue's assignee. No lease, no heartbeat: a claim ends when a
# pull request that closes the issue merges (claims.yml runs release-merged on the push to main), or
# when its human runs `release` (or `release-pr` for a pull request closed without a merge).
#
# Holder: <login>#<lane>; --lane L (^[a-z0-9][a-z0-9-]{0,15}$), default `main`. Same login and lane =
# same holder, so a restarted session keeps its claims. A claim from before ADR 0104
# (<login>/<host>/<session>[#<lane>]) reads as its login in its lane (none: main); its login may release
# it from any lane. Limits, best effort (read, then write): one claim per lane (one `take` of several
# issues counts once). A per-human cap on held claims is off (ADR 0104, amendment 2026-11-11); set
# KOINON_CLAIM_HUMAN_CAP=N to turn it on.
#
# Usage:
#   claim.sh next [--label L]       Ready/In Progress/In Review, agent-ok, current-milestone, unblocked
#                                   issues with no held claim, open closing PR or other assignee
#   claim.sh take <N>... [--lane L] claim the issue(s), all or nothing (several only for one PR that
#                                   closes them all). Already yours in this lane: exit 0, no write
#   claim.sh release <N> [--note TEXT] [--lane L] [--force]
#                                   hand it back and unassign; --force: your claim of another lane
#   claim.sh block <N> <M> [--lane L]  add "N blocked by M", then release N
#   claim.sh show <N> | mine [--lane L]   one claim; your held claims (#N TAB lane TAB pr P TAB holder)
#   claim.sh adr                    reserve the next free ADR number for good (claim/adr-NNNN), print it
#   claim.sh sensitive-paths        the ruleset patterns of CODEOWNERS' sensitive section
#   claim.sh check-pr <PR>          (claims.yml, the `claim` check) fails only when another human holds a
#                                   claim on an issue the PR closes or reserved an ADR number it adds
#   claim.sh release-pr <PR>        release the held claims of the issues a closed PR closes (its
#                                   author's or pointing at it; unmerged: unless another open PR closes
#                                   the issue, and unassign)
#   claim.sh release-merged <BEFORE> <AFTER>
#                                   (claims.yml, push to main) release-pr for each PR merged in BEFORE..AFTER
#   claim.sh help
#
# Exit codes: 0 success; 1 error (usage, API failure: retry, check failed); 2 refused (someone holds
# the issue, or a limit): pick another issue; 3 not yours (release or block on a claim another lane or
# human holds, or none).
#
# Environment (all optional):
#   KOINON_CLAIM_REPO           owner/name (default: the current repository)
#   KOINON_CLAIM_PROJECT        project number for `next` (default 1)
#   KOINON_CLAIM_PROJECT_OWNER  project owner (default: the repository owner)
#   KOINON_CLAIM_HUMAN          your GitHub login (default: the authenticated user)
#   KOINON_CLAIM_HUMAN_CAP      held claims allowed per human (default: empty or 0, no cap)
#   KOINON_CLAIM_MILESTONE      milestone title for `next` (default: the open one due first)
#   KOINON_CLAIM_CODEOWNERS     the CODEOWNERS file of sensitive-paths (default: .github/CODEOWNERS)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

DEFAULT_LANE=main
EMPTY_TREE=4b825dc642cb6eb9a060e54bf8d69288fbee4904

# ---------------------------------------------------------------------------------------------
# Pure functions (no network); hack/claim_test.sh sources this file with CLAIM_LIB_ONLY=1.
# ---------------------------------------------------------------------------------------------

die() { echo "claim: $*" >&2; exit "${CLAIM_EXIT:-1}"; }
warn() { echo "claim: warning: $*" >&2; }
# write_failed MSG: in Actions the job has issues: write, so a failed issue write fails it (koinon#315).
write_failed() { if [[ ${GITHUB_ACTIONS:-} == true ]]; then die "$*"; fi; warn "$*"; }

# claim_format ISSUE STATE HOLDER HUMAN PR NOTE [RELEASED_BY] -> the commit message. RELEASED_BY is
# written only on a release (the scripts from before ADR 0104 read it). No lease_until since ADR 0104.
claim_format() {
  local note=${6//$'\n'/ } by=${7:-}
  note=${note//$'\r'/ }
  by=${by//$'\n'/ }
  by=${by//$'\r'/ }
  printf 'claim #%s: %s\n\nholder: %s\nhuman: %s\npr: %s\nnote: %s\n' "$1" "$2" "$3" "$4" "$5" "$note"
  if [[ -n $by ]]; then printf 'released_by: %s\n' "$by"; fi
  printf 'state: %s\n' "$2"
}

# JQ_PARSE: jq definition turning a claim commit message (string) into an object. Old fields
# (lease_until) are read and ignored.
# shellcheck disable=SC2016
JQ_PARSE='def claim_parse:
  ([split("\n")[] | capture("^(?<k>holder|human|pr|note|released_by|state): ?(?<v>.*)$")]
    | map({(.k): .v}) | add // {})
  | {state: (.state // ""), holder: (.holder // ""), human: (.human // ""), pr: (.pr // ""),
     note: (.note // ""), released_by: (.released_by // "")};'

# claim_parse MESSAGE -> compact JSON {state,holder,human,pr,note,released_by}.
claim_parse() { printf '%s' "$1" | jq -Rsc "$JQ_PARSE claim_parse"; }

# holder_login HOLDER, holder_lane HOLDER: for <login>#<lane> and the old <login>/<host>/<session>[#lane].
holder_login() { local h=${1%%#*}; echo "${h%%/*}"; }
holder_lane() { if [[ $1 == *'#'* ]]; then echo "${1##*#}"; else echo "$DEFAULT_LANE"; fi; }
holder_is_old() { [[ ${1%%#*} == */* ]]; }

# JQ_HOLDER: the same two rules in jq.
# shellcheck disable=SC2016
JQ_HOLDER='def h_lane: if test("#") then split("#") | last else "main" end;'

valid_lane() { [[ $1 =~ ^[a-z0-9][a-z0-9-]{0,15}$ ]]; }

# lower S: GitHub logins are case-insensitive.
lower() { printf '%s' "${1,,}"; }

# same_holder A B: same login (any case) and lane.
same_holder() { [[ $(lower "$(holder_login "$1")") == "$(lower "$(holder_login "$2")")" && $(holder_lane "$1") == "$(holder_lane "$2")" ]]; }

# sensitive_paths CODEOWNERS_FILE -> the path patterns of the section after the `# Sensitive paths`
# marker, as ruleset patterns (no leading slash; a directory gets `**`). One source of truth: the
# main ruleset's required_reviewers rule is configured from this list.
sensitive_paths() {
  [[ -r $1 ]] || return 0
  awk '/^# Sensitive paths/ {on=1; next} on && /^\// {print $1}' "$1" | sed 's#^/##; s#/$#/**#'
}

# next_adr_number NUMBERS...: the highest number given plus one, zero padded to four digits.
next_adr_number() { local max=0 n; for n in "$@"; do ((10#$n > max)) && max=$((10#$n)); done; printf '%04d' $((max + 1)); }

# is_lost_race OUTPUT: only these 422s mean another writer won; any other (a ruleset, a bad sha) is an error.
is_lost_race() { [[ $1 == *"Reference already exists"* || $1 == *"not a fast forward"* || $1 == *"Reference cannot be updated"* ]]; }

# closing_issues BODY -> issue numbers named by closing keywords, one per line.
closing_issues() { printf '%s\n' "$1" | grep -oiE '\b(close[sd]?|fix(e[sd])?|resolve[sd]?)[: ]+#[0-9]+' | grep -oE '[0-9]+$' | sort -un || true; }

# claim_verdict STATE HUMAN AUTHOR -> free | own | conflict: only another human's held claim conflicts.
claim_verdict() { if [[ $1 != held ]]; then echo free; elif [[ $(lower "$2") == "$(lower "$3")" ]]; then echo own; else echo conflict; fi; }

# The fixed failure texts of the `claim` check; hack/claim_test.sh pins them.
conflict_issue_text() { # N HUMAN AUTHOR
  printf '::error::#%s is claimed by @%s; this pull request is by @%s. Ask @%s to release the claim (hack/claim.sh release %s or release-pr), then re-run this check.\n' \
    "$1" "$2" "$3" "$2" "$1"
}
conflict_adr_text() { # PATH NNNN HUMAN
  printf '::error::%s uses ADR number %s, which @%s reserved (claim/adr-%s). Take a free number with hack/claim.sh adr and rename the file.\n' \
    "$1" "$2" "$3" "$2"
}

# JQ_PR_CLOSERS: the open pull requests (GraphQL pages, slurped) as [{number, author, issues}], issues
# being what the pull request closes: closingIssuesReferences in this repository plus the closing
# keywords of its body.
# shellcheck disable=SC2016
JQ_PR_CLOSERS='[.[].data.repository.pullRequests.nodes[]
  | {number, author: (.author.login // ""),
     issues: ([(.closingIssuesReferences.nodes // [])[] | select(.repository.nameWithOwner == $repo) | .number]
              + [(.body // "") | scan("(?i)\\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)[: ]+#([0-9]+)\\b") | .[0] | tonumber]
              | unique)}]'

# JQ_NEXT: the pickable items of the board pages (slurped), by priority (P0..P3, anything else last),
# then age. Args: $repo, $ms, $label, $held (issue numbers with a held claim), $prs (issue numbers an
# open pull request closes), $human. In Progress and In Review count as Ready when nothing holds the
# issue: no script moves the board back on a release.
# shellcheck disable=SC2016
JQ_NEXT='def rank: ({"P0":0,"P1":1,"P2":2,"P3":3}[.] // 9);
  [.[].data.organization.projectV2.items.nodes[]
  | select(.content.number != null and .content.state == "OPEN" and .content.repository.nameWithOwner == $repo)
  | select(.status.name // "" | IN("Ready", "In Progress", "In Review"))
  | select([.content.labels.nodes[].name] | index("agent-ok"))
  | select($ms == "" or .content.milestone.title == $ms)
  | select($label == "" or ([.content.labels.nodes[].name] | index($label)))
  | select(.content.number as $n | ($held | index($n) | not) and ($prs | index($n) | not))
  | select([(.content.assignees.nodes // [])[].login | ascii_downcase] | all(. == ($human | ascii_downcase)))
  | {n: .content.number, p: (.priority.name // ""), c: (.component.name // ""), t: .content.title, at: .content.createdAt}]
  | sort_by([(.p | rank), .at])'

# held_of CLAIMS_JSON LOGIN [LANE] [EXCEPT_JSON] -> "#N" for each held claim of LOGIN (in LANE when
# given), leaving out the issue numbers in EXCEPT_JSON; one per line.
held_of() {
  jq -r --arg l "$(lower "$2")" --arg lane "${3:-}" --argjson ex "${4:-[]}" "$JQ_HOLDER"'
    .[] | select(.state == "held" and (.human | ascii_downcase) == $l and (.issue as $i | $ex | index($i) | not)
                 and ($lane == "" or (.holder | h_lane) == $lane)) | "#\(.issue)"' <<<"$1"
}

# ADR_PR_JQ: the ADR numbers that open pull requests ADD (added, renamed or copied files), from the
# pages of open_pr_adr_numbers (slurped).
# shellcheck disable=SC2016
ADR_PR_JQ='[.[].data.repository.pullRequests.nodes[] | .files.nodes[]
  | select(.changeType == "ADDED" or .changeType == "RENAMED" or .changeType == "COPIED") | .path | capture("^docs/adr/(?<n>[0-9]{4})-[^/]*\\.md$").n] | unique | .[]'

# ---------------------------------------------------------------------------------------------
# GitHub plumbing
# ---------------------------------------------------------------------------------------------

REPO=${KOINON_CLAIM_REPO:-}
PROJECT=${KOINON_CLAIM_PROJECT:-1}
PROJECT_OWNER=${KOINON_CLAIM_PROJECT_OWNER:-}
OWNER='' NAME='' LANE=$DEFAULT_LANE
init_repo() {
  [[ -n $REPO ]] || REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
  OWNER=${REPO%%/*}
  NAME=${REPO#*/}
  PROJECT_OWNER=${PROJECT_OWNER:-$OWNER}
}

# gh_try ARGS...: run `gh api`, keep the output in TRY_OUT and the exit status in TRY_RC.
TRY_OUT='' TRY_RC=0
gh_try() { TRY_RC=0; TRY_OUT=$(gh api "$@" 2>&1) || TRY_RC=$?; }

human_login() { if [[ -n ${KOINON_CLAIM_HUMAN:-} ]]; then echo "$KOINON_CLAIM_HUMAN"; else gh api user --jq .login; fi; }

# read_claim N -> JSON {issue,sha,at,state,holder,human,pr,note,released_by} (at: the claim commit's
# date). Returns 0 found, 1 no claim ref, 4 API error. N is an issue number, or adr-NNNN for an ADR
# number reservation.
read_claim() {
  local out
  # shellcheck disable=SC2016
  out=$(gh api graphql -f o="$OWNER" -f n="$NAME" -f q="refs/heads/claim/$1" -f query='
    query($o:String!,$n:String!,$q:String!){repository(owner:$o,name:$n){ref(qualifiedName:$q){target{... on Commit{oid message committedDate}}}}}' \
    --jq '.data.repository.ref.target // empty | [.oid, .message, .committedDate] | @json') || { echo "claim: cannot read claim/$1" >&2; return 4; }
  [[ -n $out ]] || return 1
  jq -c --arg n "$1" "$JQ_PARSE"'
    . as [$sha, $msg, $at] | ($msg | claim_parse) + {issue: ($n | tonumber? // $n), sha: $sha, at: ($at // "")}' <<<"$out" || return 4
}

# load_claim N: sets C and returns read_claim's code; an API error dies (exit 1).
C=
load_claim() {
  local rc=0
  C=$(read_claim "$1") || rc=$?
  if ((rc == 4)); then die "cannot read the claim on #$1 (API error); retry"; fi
  return "$rc"
}

# list_claims -> JSON array of every claim on an issue (numeric claim/<N> refs only).
list_claims() {
  # shellcheck disable=SC2016
  gh api graphql --paginate -f o="$OWNER" -f n="$NAME" -f query='
    query($o:String!,$n:String!,$endCursor:String){repository(owner:$o,name:$n){
      refs(refPrefix:"refs/heads/claim/",first:100,after:$endCursor){
        pageInfo{hasNextPage endCursor} nodes{name target{... on Commit{oid message}}}}}}' |
    jq -s "$JQ_PARSE"'
      [.[].data.repository.refs.nodes[] | select(.name | test("^[0-9]+$"))
        | (.target.message | claim_parse) + {issue: (.name | tonumber), sha: .target.oid}]'
}

# write_claim REF STATE HOLDER HUMAN PR NOTE [PARENT_SHA] [RELEASED_BY]: the compare-and-set. Creates
# the commit, then the ref claim/REF (no parent) or fast-forwards it. Returns 0 won, 1 lost the race;
# any other failure (including other 422s, e.g. a ruleset) dies.
write_claim() {
  local ref=$1 parent=${7:-} msg sha
  msg=$(claim_format "$ref" "$2" "$3" "$4" "$5" "$6" "${8:-}")
  local args=(-X POST "repos/$REPO/git/commits" -f "message=$msg" -f "tree=$EMPTY_TREE")
  [[ -z $parent ]] || args+=(-f "parents[]=$parent")
  gh_try "${args[@]}" --jq .sha
  ((TRY_RC == 0)) || die "cannot create the claim commit: $TRY_OUT"
  sha=$TRY_OUT
  if [[ -z $parent ]]; then
    gh_try -X POST "repos/$REPO/git/refs" -f "ref=refs/heads/claim/$ref" -f "sha=$sha"
  else
    gh_try -X PATCH "repos/$REPO/git/refs/heads/claim/$ref" -f "sha=$sha" -F force=false
  fi
  if ((TRY_RC == 0)); then return 0; fi
  if [[ $TRY_OUT == *"HTTP 422"* ]] && is_lost_race "$TRY_OUT"; then return 1; fi
  die "claim write failed: $TRY_OUT"
}

# issue_json N -> {number,state,pull,labels[],assignees[]}
issue_json() {
  gh api "repos/$REPO/issues/$1" --jq '{number, state, pull: (.pull_request != null),
    labels: [.labels[].name], assignees: [.assignees[]?.login]}'
}

# open_pr_closers -> the open pull requests as JSON [{number, author, issues}]. An API error fails:
# `next` and `take` must not hand out an issue from a partial view.
open_pr_closers() {
  local pages
  # shellcheck disable=SC2016
  pages=$(gh api graphql --paginate -f o="$OWNER" -f n="$NAME" -f query='
    query($o:String!,$n:String!,$endCursor:String){repository(owner:$o,name:$n){
      pullRequests(states:OPEN,first:50,after:$endCursor){pageInfo{hasNextPage endCursor}
        nodes{number body author{login} closingIssuesReferences(first:25){nodes{number repository{nameWithOwner}}}}}}}' | jq -s .) || return 1
  jq -c --arg repo "$REPO" "$JQ_PR_CLOSERS" <<<"$pages"
}

# prs_closing CLOSERS N HUMAN -> the open pull requests that close N, as JSON {own: the first one by
# HUMAN (or ""), others: "#P by @author, ..." for the rest (or "")}.
prs_closing() {
  jq -c --argjson n "$2" --arg h "$(lower "$3")" '[.[] | select(.issues | index($n))]
    | {own: ([.[] | select((.author | ascii_downcase) == $h) | .number] | first // "" | tostring),
       others: ([.[] | select((.author | ascii_downcase) != $h) | "#\(.number) by @\(.author)"] | join(", "))}' <<<"$1"
}

# other_assignees ISSUE_JSON HUMAN -> the assignees other than HUMAN, space separated.
other_assignees() {
  jq -r --arg h "$(lower "$2")" '[(.assignees // [])[] | select(ascii_downcase != $h) | "@" + .] | join(" ")' <<<"$1"
}

# pr_meta P -> {author, body, state, merged, closers}: closers are the issues of this repository GitHub links
# pull request P to as closed by it (closingIssuesReferences: keywords in any form and the sidebar).
pr_meta() {
  local out
  # shellcheck disable=SC2016
  out=$(gh api graphql -f o="$OWNER" -f n="$NAME" -F p="$1" -f query='
    query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){
      author{login} body state merged closingIssuesReferences(first:100){nodes{number repository{nameWithOwner}}}}}}') || return 1
  jq -ce --arg repo "$REPO" '.data.repository.pullRequest | select(. != null)
    | {author: (.author.login // ""), body: (.body // ""), state: (.state // ""), merged: (.merged // false),
       closers: [(.closingIssuesReferences.nodes // [])[] | select(.repository.nameWithOwner == $repo) | .number]}' <<<"$out"
}

# pr_added_adr_files P -> the docs/adr/NNNN-*.md files pull request P adds (added, renamed or copied).
pr_added_adr_files() {
  local files
  files=$(gh api --paginate "repos/$REPO/pulls/$1/files" \
    --jq '.[] | select(.status == "added" or .status == "renamed" or .status == "copied") | .filename') || return 1
  grep -E '^docs/adr/[0-9]{4}-[^/]*\.md$' <<<"$files" || true
}

# open_pr_adr_numbers -> the ADR numbers that open pull requests add, one per line. An API error dies:
# a number handed out from a partial view is how 0093 was duplicated. A pull request with more than
# 100 files is read through the REST list.
open_pr_adr_numbers() {
  local pages p
  # shellcheck disable=SC2016
  pages=$(gh api graphql --paginate -f o="$OWNER" -f n="$NAME" -f query='
    query($o:String!,$n:String!,$endCursor:String){repository(owner:$o,name:$n){
      pullRequests(states:OPEN,first:50,after:$endCursor){
        pageInfo{hasNextPage endCursor}
        nodes{number files(first:100){pageInfo{hasNextPage} nodes{path changeType}}}}}}' | jq -s .) ||
    die "cannot list the open pull requests' files (API error); retry, no ADR number was reserved"
  jq -r "$ADR_PR_JQ" <<<"$pages" || die "cannot read the open pull requests' files"
  for p in $(jq -r '[.[].data.repository.pullRequests.nodes[] | select(.files.pageInfo.hasNextPage) | .number] | .[]' <<<"$pages"); do
    gh api --paginate "repos/$REPO/pulls/$p/files" --jq '.[] | select(.status == "added" or .status == "renamed" or .status == "copied") | .filename' |
      { grep -oE '^docs/adr/[0-9]{4}-[^/]*\.md$' || true; } | cut -c10-13 ||
      die "cannot list the files of pull request #$p (API error); retry"
  done
}

assign() { gh api -X POST "repos/$REPO/issues/$1/assignees" -f "assignees[]=$2" >/dev/null 2>&1 || warn "cannot assign $2 to #$1"; }
unassign() { gh api -X DELETE "repos/$REPO/issues/$1/assignees" -f "assignees[]=$2" >/dev/null 2>&1 || write_failed "cannot unassign $2 from #$1"; }

# ---------------------------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------------------------

cmd_next() {
  local label="" ms human claims held closers prs json n p c t blockers
  if [[ ${1:-} == --label ]]; then label=${2:-}; [[ -n $label ]] || die "usage: next [--label L]"; fi
  [[ $PROJECT != none ]] || die "next needs the project board (KOINON_CLAIM_PROJECT)"
  ms=${KOINON_CLAIM_MILESTONE:-}
  [[ -n $ms ]] || ms=$(gh api "repos/$REPO/milestones?state=open" --jq 'sort_by(.due_on // "9999", .number) | .[0].title // empty')
  human=$(human_login)
  claims=$(list_claims)
  held=$(jq -c '[.[] | select(.state == "held") | .issue]' <<<"$claims")
  closers=$(open_pr_closers) || die "cannot list the open pull requests (API error); retry"
  prs=$(jq -c '[.[].issues[]] | unique' <<<"$closers")
  # shellcheck disable=SC2016
  json=$(gh api graphql --paginate -f o="$PROJECT_OWNER" -F n="$PROJECT" -f query='
    query($o:String!,$n:Int!,$endCursor:String){organization(login:$o){projectV2(number:$n){
      items(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{
        status:fieldValueByName(name:"Status"){... on ProjectV2ItemFieldSingleSelectValue{name}}
        priority:fieldValueByName(name:"Priority"){... on ProjectV2ItemFieldSingleSelectValue{name}}
        component:fieldValueByName(name:"Component"){... on ProjectV2ItemFieldSingleSelectValue{name}}
        content{... on Issue{number title state createdAt repository{nameWithOwner} milestone{title}
          labels(first:50){nodes{name}} assignees(first:10){nodes{login}}}}}}}}}' |
    jq -s --arg repo "$REPO" --arg ms "$ms" --arg label "$label" --argjson held "$held" --argjson prs "$prs" \
      --arg human "$human" "$JQ_NEXT")
  while IFS=$'\037' read -r n p c t; do
    [[ -n $n ]] || continue
    blockers=$(gh api "repos/$REPO/issues/$n/dependencies/blocked_by" --jq '[.[] | select(.state == "open")] | length' 2>/dev/null || echo 0)
    [[ $blockers == 0 ]] || continue
    printf '#%s\t%s\t%s\t%s\n' "$n" "${p:--}" "${c:--}" "$t"
  done < <(jq -r '.[] | [.n, .p, .c, .t] | map(tostring) | join("\u001f")' <<<"$json")
}

# take_one N HOLDER HUMAN: 0 taken (or already yours), 2 refused or lost the race (reason printed).
# Reads CLOSERS (open_pr_closers of this take).
CLOSERS="[]"
take_one() {
  local n=$1 holder=$2 human=$3 issue parent="" rc=0 held_by prs others
  issue=$(issue_json "$n") || { echo "claim: cannot read issue #$n" >&2; return 1; }
  if [[ $(jq -r .state <<<"$issue") != open ]]; then echo "claim: #$n is closed" >&2; return 2; fi
  if [[ $(jq -r .pull <<<"$issue") != false ]]; then echo "claim: #$n is a pull request" >&2; return 2; fi
  load_claim "$n" || rc=$?
  if ((rc == 0)); then
    parent=$(jq -r .sha <<<"$C")
    if [[ $(jq -r .state <<<"$C") == held ]]; then
      held_by=$(jq -r .holder <<<"$C")
      if same_holder "$held_by" "$holder"; then
        # A restart in the same lane keeps its claim (exit 0); a second session in the lane must see it.
        warn "#$n is already held by $held_by since $(jq -r '.at // "" | if . == "" then "an unknown date" else . end' <<<"$C"); if another session took it, stop and use your own --lane"
        echo "#$n is already yours (lane $LANE)"; TAKE_NEW=0; return 0
      fi
      if [[ $(lower "$(jq -r .human <<<"$C")") == "$(lower "$human")" ]]; then
        echo "claim: #$n is held by your lane $(holder_lane "$held_by") ($held_by): use --lane $(holder_lane "$held_by"), or release it first (release $n --force)" >&2
      else
        echo "claim: #$n is held by @$(jq -r .human <<<"$C"); ask them to release it. Pick another issue." >&2
      fi
      return 2
    fi
  fi
  prs=$(prs_closing "$CLOSERS" "$n" "$human")
  if [[ -n $(jq -r .others <<<"$prs") ]]; then
    echo "claim: #$n has an open pull request by someone else: $(jq -r .others <<<"$prs"). Pick another issue." >&2; return 2
  fi
  others=$(other_assignees "$issue" "$human")
  if [[ -n $others ]]; then echo "claim: #$n is assigned to $others. Pick another issue." >&2; return 2; fi
  if ! write_claim "$n" held "$holder" "$human" "$(jq -r .own <<<"$prs")" "" "$parent"; then
    echo "claim: lost the race for #$n: someone claimed it first. Pick another issue." >&2; return 2
  fi
  TAKE_NEW=1
  echo "claimed #$n as $holder"
  jq -e '.labels | index("agent-ok")' <<<"$issue" >/dev/null || warn "#$n has no agent-ok label"
  assign "$n" "$human"
}

cmd_take() {
  local issues=() a human holder claims want lane_held human_held human_cap new_count n rc
  for a in "$@"; do
    [[ $a =~ ^[0-9]+$ ]] || die "usage: take <N>... [--lane L]"
    issues+=("$a")
  done
  ((${#issues[@]})) || die "usage: take <N>... [--lane L]"
  human=$(human_login); holder="$human#$LANE"
  claims=$(list_claims)
  want=$(printf '%s\n' "${issues[@]}" | jq -sc 'map(tonumber)')
  lane_held=$(held_of "$claims" "$human" "$LANE" "$want" | paste -sd, - | sed 's/,/, /g')
  if [[ -n $lane_held ]]; then
    CLAIM_EXIT=2 die "lane $LANE already holds $lane_held (one claim per lane). Finish or release it, or take this issue in its own lane with --lane <name>."
  fi
  human_held=$(held_of "$claims" "$human" "" "$want" | wc -l | tr -d ' ')
  human_cap=${KOINON_CLAIM_HUMAN_CAP:-0}
  [[ $human_cap =~ ^[0-9]+$ ]] || die "KOINON_CLAIM_HUMAN_CAP must be a number (is '$human_cap')"
  human_cap=$((10#$human_cap)) # decimal: 010 is 10, 08 is 8
  # Only issues the human does not hold yet count against the cap (a repeat take stays the no-op path).
  new_count=$(jq -r --argjson want "$want" --arg l "$(lower "$human")" '
    [.[] | select(.state == "held" and (.human | ascii_downcase) == $l) | .issue] as $mine
    | [$want[] | select(. as $i | $mine | index($i) | not)] | length' <<<"$claims")
  if ((human_cap > 0 && human_held + new_count > human_cap)); then
    CLAIM_EXIT=2 die "$human holds $human_held claims; $new_count more would pass the cap of $human_cap."
  fi
  CLOSERS=$(open_pr_closers) || die "cannot list the open pull requests (API error); retry, nothing was claimed"
  TAKE_HOLDER=$holder TAKE_HUMAN=$human
  trap take_rollback EXIT # all or nothing, also when a read or a write dies
  for n in "${issues[@]}"; do
    rc=0; TAKE_NEW=0; TAKE_AT=$n
    take_one "$n" "$holder" "$human" || rc=$?
    if ((rc == 0)); then ((TAKE_NEW == 0)) || TAKEN+=("$n"); continue; fi
    exit "$rc"
  done
  trap - EXIT
}

# take_rollback: the EXIT trap of take. A pull request closing several issues needs all of them, so
# on any exit before the end it releases and unassigns the claims this take wrote (TAKEN). Each one
# runs in a subshell, so a step that dies does not stop the next; the exit code stays the first one.
# Only a claim still held by this take's holder is released.
TAKEN=() TAKE_HOLDER='' TAKE_HUMAN='' TAKE_AT=''
take_rollback() {
  local rc=$? t r
  trap - EXIT
  for t in ${TAKEN[@]+"${TAKEN[@]}"}; do
    r=0
    (
      load_claim "$t" || exit 5
      [[ $(jq -r .state <<<"$C") == held ]] && same_holder "$(jq -r .holder <<<"$C")" "$TAKE_HOLDER" || exit 5
      write_claim "$t" released "$TAKE_HOLDER" "$TAKE_HUMAN" "" "rolled back: #$TAKE_AT was not available" "$(jq -r .sha <<<"$C")" "$TAKE_HOLDER" || exit 5
      unassign "$t" "$TAKE_HUMAN"
    ) || r=$?
    case $r in
      0) echo "rolled back #$t" >&2 ;;
      5) warn "#$t is no longer held by $TAKE_HOLDER; left as it is" ;;
      *) warn "rollback of #$t failed; release it by hand (release $t)" ;;
    esac
  done
  exit "$rc"
}

# release_check N FORCE: sets C, HUMAN; exit 3 unless the held claim on N is this login's in this lane
# (another lane only with FORCE; a claim from before ADR 0104 from any lane). Exit 0 with a message
# when the claim is already released (RELEASED=1).
RELEASED=0
release_check() {
  local n=$1 force=$2 rc=0 holder
  HUMAN=$(human_login)
  load_claim "$n" || rc=$?
  ((rc == 0)) || CLAIM_EXIT=3 die "no claim on #$n"
  if [[ $(jq -r .state <<<"$C") != held ]]; then echo "#$n is already released"; RELEASED=1; return 0; fi
  holder=$(jq -r .holder <<<"$C")
  if [[ $(lower "$(jq -r .human <<<"$C")") != "$(lower "$HUMAN")" ]]; then
    CLAIM_EXIT=3 die "#$n is held by @$(jq -r .human <<<"$C"); only they can release it"
  fi
  if ! holder_is_old "$holder" && [[ $(holder_lane "$holder") != "$LANE" ]] && ((!force)); then
    CLAIM_EXIT=3 die "#$n is held by your lane $(holder_lane "$holder"), not lane $LANE: use --lane $(holder_lane "$holder"), or --force"
  fi
}

# do_release CLAIM_JSON NOTE BY MERGED: write state released on top of the claim's sha (a claim that
# moved since it was read makes the write fail: return 1). Unassigns the human unless MERGED.
do_release() {
  local c=$1 n human
  n=$(jq -r .issue <<<"$c"); human=$(jq -r .human <<<"$c")
  write_claim "$n" released "$(jq -r .holder <<<"$c")" "$human" "$(jq -r .pr <<<"$c")" "$2" "$(jq -r .sha <<<"$c")" "$3" ||
    { echo "claim: #$n changed since it was read; not released" >&2; return 1; }
  echo "released #$n"
  if [[ ${4:-0} != 1 && $(gh api "repos/$REPO/issues/$n" --jq .state 2>/dev/null) == open ]]; then unassign "$n" "$human"; fi
}

cmd_release() {
  local n=${1:-} note="released" force=0
  [[ $n =~ ^[0-9]+$ ]] || die "usage: release <N> [--note TEXT] [--lane L] [--force]"
  shift
  while (($#)); do
    case $1 in
      --note) note=${2:?--note needs a text}; shift 2 ;;
      --force) force=1; shift ;;
      *) die "unknown option $1" ;;
    esac
  done
  release_check "$n" "$force"
  ((RELEASED)) || do_release "$C" "$note" "$HUMAN#$LANE" 0 || CLAIM_EXIT=3 die "#$n changed while releasing it: re-read with show"
}

cmd_block() {
  local n=${1:-} m=${2:-} id
  [[ $n =~ ^[0-9]+$ && $m =~ ^[0-9]+$ && $# -eq 2 ]] || die "usage: block <N> <M> [--lane L]"
  release_check "$n" 0
  id=$(gh api "repos/$REPO/issues/$m" --jq .id) || die "cannot read #$m"
  gh_try -X POST "repos/$REPO/issues/$n/dependencies/blocked_by" -F "issue_id=$id"
  if ((TRY_RC != 0)) && [[ $TRY_OUT != *"already"* && $TRY_OUT != *"HTTP 422"* ]]; then die "cannot add the dependency: $TRY_OUT"; fi
  echo "#$n is now blocked by #$m"
  ((RELEASED)) || do_release "$C" "blocked by #$m" "$HUMAN#$LANE" 0 || CLAIM_EXIT=3 die "#$n changed while releasing it"
}

cmd_show() {
  local rc=0
  [[ ${1:-} =~ ^([0-9]+|adr-[0-9]{4})$ ]] || die "usage: show <N>"
  load_claim "$1" || rc=$?
  if ((rc == 1)); then echo "#$1: no claim"; return 0; fi
  jq -r "$JQ_HOLDER"'"#\(.issue): \(.state)\n  holder: \(.holder)\n  lane:   \(.holder | h_lane)\n  human:  \(.human)\n  pr:     \(.pr)\n  note:   \(.note)\n  ref sha: \(.sha)"' <<<"$C"
}

cmd_mine() {
  local claims human
  claims=$(list_claims); human=$(human_login)
  jq -r --arg h "$(lower "$human")" --arg lane "$LANE_FILTER" "$JQ_HOLDER"'
    .[] | select(.state == "held" and (.human | ascii_downcase) == $h and ($lane == "" or (.holder | h_lane) == $lane))
    | "#\(.issue)\t\(.holder | h_lane)\tpr \(.pr)\t\(.holder)"' <<<"$claims"
}

# adr: reserve the next free ADR number for good. claim/adr-NNNN is created like a claim (create fails
# when it exists), so two lanes cannot take one number. A number is free when no docs/adr file on the
# default branch, no claim/adr-* ref and no file an open pull request adds uses it.
cmd_adr() {
  local files refs prs n human tries=0
  human=$(human_login)
  while ((tries < 5)); do
    tries=$((tries + 1))
    files=$(gh api "repos/$REPO/contents/docs/adr" --jq '.[].name' | grep -oE '^[0-9]{4}' || true)
    refs=$(gh api "repos/$REPO/git/matching-refs/heads/claim/adr-" --jq '.[].ref' | grep -oE '[0-9]{4}$' || true)
    prs=$(open_pr_adr_numbers)
    # shellcheck disable=SC2086
    n=$(next_adr_number $files $refs $prs)
    if write_claim "adr-$n" reserved "$human#$LANE" "$human" "" "ADR number reservation"; then
      echo "$n"
      echo "reserved ADR $n (ref claim/adr-$n); name your file docs/adr/$n-<title>.md" >&2
      return 0
    fi
  done
  die "could not reserve an ADR number"
}

# check-pr P: the required `claim` check. Fails only when another human holds a claim on an issue the
# pull request closes, or reserved an ADR number the pull request adds a file under. Read-only.
cmd_check_pr() {
  local p=${1:-} pr author body issues n rc human path num fail=0 added
  [[ $p =~ ^[0-9]+$ ]] || die "usage: check-pr <PR>"
  pr=$(pr_meta "$p") || die "cannot read pull request #$p (API error); re-run the check"
  author=$(jq -r .author <<<"$pr"); body=$(jq -r .body <<<"$pr")
  issues=$({ jq -r '.closers[]?' <<<"$pr"; closing_issues "$body"; } | sort -un)
  [[ -n $issues ]] || echo "claim check: no Closes #N; nothing to check"
  for n in $issues; do
    rc=0
    load_claim "$n" || rc=$?
    if ((rc == 1)); then echo "claim check: #$n is free"; continue; fi
    human=$(jq -r .human <<<"$C")
    case $(claim_verdict "$(jq -r .state <<<"$C")" "$human" "$author") in
      free) echo "claim check: #$n is free (the claim was released)" ;;
      own) echo "claim check: #$n is claimed by @$human (the author)" ;;
      *) conflict_issue_text "$n" "$human" "$author"; fail=1 ;;
    esac
  done
  added=$(pr_added_adr_files "$p") || die "cannot list the files of pull request #$p (API error); re-run the check"
  while IFS= read -r path; do
    [[ -n $path ]] || continue
    num=${path:9:4}
    rc=0
    load_claim "adr-$num" || rc=$?
    if ((rc == 1)); then echo "claim check: $path: ADR $num is not reserved"; continue; fi
    human=$(jq -r .human <<<"$C")
    # A reservation is permanent (ADR 0104): old ones say held and carry a lease, which is ignored.
    if [[ $(jq -r .state <<<"$C") != held && $(jq -r .state <<<"$C") != reserved ]]; then
      echo "claim check: $path: the reservation of ADR $num was released"; continue
    fi
    if [[ $(lower "$human") == "$(lower "$author")" ]]; then echo "claim check: $path: ADR $num is reserved by @$human (the author)"; continue; fi
    conflict_adr_text "$path" "$num" "$human"
    fail=1
  done <<<"$added"
  return "$fail"
}

# release-pr P: pull request P closed. Release the held claims of the issues it closes when the claim
# is its author's or points at P; an unmerged close keeps a claim that another open pull request also
# closes. Each issue is tried; any failure ends red.
cmd_release_pr() {
  local p=${1:-} pr author merged n rc others closers="[]" failed=0
  [[ $p =~ ^[0-9]+$ ]] || die "usage: release-pr <PR>"
  pr=$(pr_meta "$p") || die "cannot read pull request #$p (API error)"
  # An open PR would release claims around release's "only its human" rule.
  [[ $(jq -r .state <<<"$pr") =~ ^(CLOSED|MERGED)$ ]] || die "pull request #$p is not closed or merged; use release <N>"
  author=$(jq -r .author <<<"$pr")
  merged=0; [[ $(jq -r .merged <<<"$pr") != true ]] || merged=1
  ((merged)) || closers=$(open_pr_closers) || die "cannot list the open pull requests (API error)"
  for n in $({ jq -r '.closers[]?' <<<"$pr"; closing_issues "$(jq -r .body <<<"$pr")"; } | sort -un); do
    rc=0
    load_claim "$n" || rc=$?
    ((rc == 0)) || continue
    [[ $(jq -r .state <<<"$C") == held ]] || continue
    if [[ $(jq -r .pr <<<"$C") != "$p" && $(lower "$(jq -r .human <<<"$C")") != "$(lower "$author")" ]]; then
      echo "#$n: claimed by @$(jq -r .human <<<"$C"), not by the author of #$p; keeping the claim"; continue
    fi
    if ((!merged)); then
      others=$(jq -r --argjson n "$n" --argjson p "$p" '.[] | select((.issues | index($n)) and .number != $p) | .number' <<<"$closers")
      [[ -z $others ]] || { echo "#$n: another open pull request closes it; keeping the claim"; continue; }
    fi
    # A subshell: a failed write (write_failed dies under Actions) must not skip the other issues.
    (do_release "$C" "pull request #$p closed" pr-close "$merged") || failed=$((failed + 1))
  done
  ((failed == 0)) || { echo "claim: $failed claim(s) failed to release" >&2; return 1; }
}

# release-merged BEFORE AFTER: release the claims of the pull requests merged by a push to main (the
# `release` job of claims.yml, #463). The commits are BEFORE..AFTER (AFTER alone when BEFORE is empty
# or all zeros, a new branch); GitHub links each commit to its pull requests, and only merged ones
# count. Nothing runs on a close without a merge: its human runs `release-pr <PR>` or `release <N>`.
cmd_release_merged() {
  local before=${1:-} after=${2:-} shas sha prs p failed=0
  [[ $after =~ ^[0-9a-f]{7,64}$ ]] || die "usage: release-merged <BEFORE> <AFTER>"
  if [[ $before =~ ^[0-9a-f]{7,64}$ && ! $before =~ ^0+$ ]]; then
    shas=$(gh api --paginate "repos/$REPO/compare/$before...$after" --jq '.commits[].sha') ||
      die "cannot list the commits $before..$after (API error); re-run the job"
    # AFTER is always in the list: a compare page is capped and may omit the newest commit.
    shas=$(printf '%s\n%s\n' "$shas" "$after" | grep . | sort -u)
  else
    shas=$after
  fi
  prs=$(for sha in $shas; do
    gh api "repos/$REPO/commits/$sha/pulls" --jq '.[] | select(.merged_at != null) | .number' ||
      die "cannot list the pull requests of $sha (API error); re-run the job"
  done | sort -un)
  # A lagging API (the commit not yet linked to its pull request) shows here. No sweeper backs it up
  # (ADR 0104): the human releases the claim (`release-pr <PR>` or `release <N>`).
  [[ -n $prs ]] || echo "release-merged: no merged pull request found for ${before:-none}..$after"
  for p in $prs; do
    echo "release-merged: pull request #$p"
    (cmd_release_pr "$p") || failed=$((failed + 1))
  done
  ((failed == 0)) || { echo "claim: $failed pull request(s) failed to release" >&2; return 1; }
}

usage() { sed -n '4,/^# Needs bash 5/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }

# parse_lane ARGS...: takes --lane L out of the arguments into LANE (and LANE_FILTER, for mine); the
# other arguments go to REST.
REST=()
LANE_FILTER=
parse_lane() {
  REST=()
  while (($#)); do
    case $1 in
      --lane)
        (($# >= 2)) || die "--lane needs a name"
        valid_lane "$2" || die "bad lane '$2': use 1 to 16 characters of a-z, 0-9 and -, starting with a letter or digit"
        LANE=$2; LANE_FILTER=$2; shift 2 ;;
      *) REST+=("$1"); shift ;;
    esac
  done
}

main() {
  local cmd=${1:-}
  case $cmd in
    "") usage; exit 1 ;;
    -h | --help | help) usage; exit 0 ;;
  esac
  shift
  command -v gh >/dev/null || die "gh is required"
  command -v jq >/dev/null || die "jq is required"
  parse_lane "$@"
  if ((${#REST[@]})); then set -- "${REST[@]}"; else set --; fi
  case $cmd in
    sensitive-paths) sensitive_paths "${KOINON_CLAIM_CODEOWNERS:-$(cd "$(dirname "$0")/.." && pwd)/.github/CODEOWNERS}"; return ;;
    next | take | release | block | show | mine | adr | check-pr | release-pr | release-merged) ;;
    *) usage >&2; exit 1 ;;
  esac
  init_repo
  case $cmd in
    next) cmd_next "$@" ;;
    take) cmd_take "$@" ;;
    release) cmd_release "$@" ;;
    block) cmd_block "$@" ;;
    show) cmd_show "$@" ;;
    mine) cmd_mine ;;
    adr) cmd_adr ;;
    check-pr) cmd_check_pr "$@" ;;
    release-pr) cmd_release_pr "$@" ;;
    release-merged) cmd_release_merged "$@" ;;
  esac
}

if [[ -z ${CLAIM_LIB_ONLY:-} ]]; then main "$@"; fi
