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
# Claim classes (ADR 0094 decision 1): an agent claim (`take`, 2 h lease, holder
# `<login>/<host>/<session>[#lane]`), a person claim (`take --human`, holder `<login>/<host>/human`, or a
# maintainer's `/claim` comment, holder `<login>/github/human`; 3 days) and an outsider claim (an outside
# contributor's `/claim` comment, holder `<login>/github/outsider`; 14 days, at most 2 per login, only
# on `good first issue` and `help wanted` issues).
#
# Usage:
#   claim.sh next [--label L]         Ready (or In Progress/In Review without a claim), agent-ok,
#                                     current-milestone, unblocked issues with no held claim, no open
#                                     pull request that closes them and no assignee but your human
#   claim.sh take <N>... [--takeover] [--human] [--lane L]
#                                     claim issue(s) N as one unit (several only for one PR that
#                                     closes them all). --takeover: the holder is dead (lease and
#                                     PR activity + grace, by the lease of the holder's class).
#                                     --human: a 3-day claim by a person. --lane L: claim in lane L
#                                     (see Lanes below). Refused (exit 2) while someone else holds
#                                     it, another author's open pull request closes it or someone
#                                     other than your human is assigned; your human's own open pull
#                                     request is linked as the claim's pr.
#   claim.sh beat <N>... [--pr P] [--lane L]
#                                     extend the lease (every 30 min and before long steps; PR
#                                     pushes alone are not liveness). Human claims: daily.
#   claim.sh beat --mine [--every DURATION] [--for DURATION] [--while-pid PID]
#                                     renew every claim this session holds, lanes included (only
#                                     lane L with --lane); one line per round. --every 30m (or 90,
#                                     90s, 2h) loops: exit 3 stops it and names the lost claim; an
#                                     error (exit 1) is retried next round. The loop ends (exit 0)
#                                     after --for (default 12h, the backstop) or when PID is gone
#                                     (in the background use --while-pid $PPID, the agent that
#                                     started the shell; $$ dies with that one call). A claim this
#                                     session released itself (released_by) or that its pull request
#                                     close released is dropped quietly; a takeover or a release by
#                                     the sweeper, another session or person is lost (exit 3, named).
#   claim.sh adopt --from <session> [N...]
#                                     re-key the claims of your dead session to this one, each in
#                                     its own lane (all of them without N). The holder must be dead
#                                     by the take --takeover rule; the caps still apply (exit 2). A
#                                     session that died after `review` (PR ready) cannot be adopted:
#                                     take --takeover once the PR is not ready, or ask the human.
#   claim.sh review <N> [--lane L]    the PR is ready: Status In Review, lease extended
#   claim.sh release <N> --note TEXT [--lane L]
#                                     hand the issue back (keep the branch and draft PR)
#   claim.sh wait-release <N> [--timeout SECONDS]
#                                     block until no live claim on N remains (released, or its PR
#                                     merged or closed); exit 0, or 1 on timeout (default 7200)
#   claim.sh block <N> <M>            N waits for issue M: add "blocked by", release, Status Ready
#   claim.sh show <N> | mine [--lane L] | limits [--lane L]
#   claim.sh adr                      reserve the next free ADR number (claim/adr-NNNN) and print it;
#                                     counts docs/adr files, claim/adr-* refs and ADR files that open
#                                     pull requests add
#   claim.sh sensitive-paths          the ruleset patterns for CODEOWNERS' sensitive section
#   claim.sh sweep | check-pr <PR> | release-pr <PR> | update-oldest     (used by claims.yml, updater.yml)
#                                     check-pr fails only on a conflict: someone else's human holds a
#                                     live claim on an issue the PR closes, or reserved an ADR number
#                                     the PR adds a file under (ADR 0094 decision 2)
#   claim.sh comment <N>              (used by claim-comment.yml) run the /claim or /release comment in
#                                     KOINON_CLAIM_COMMENT_BODY by KOINON_CLAIM_COMMENT_LOGIN on issue
#                                     N; every refusal is answered on the issue, and a write GitHub
#                                     refuses (401/403) points the commenter to a pull request, opens
#                                     one Backlog issue for the maintainers and exits 0
#
# Lanes (ADR 0082, amendment):
#   The session cap is one claim per (session, lane). A session that runs several agents at once,
#   one issue each in its own worktree, gives each agent a lane: `--lane L` on take, beat, review,
#   release, limits and mine, or KOINON_CLAIM_LANE in the environment (the flag wins). L matches
#   ^[a-z0-9][a-z0-9-]{0,15}$. With a lane the holder is `<login>/<host>/<session>#<L>`; without
#   one it is `<login>/<host>/<session>`, which is its own lane. A claim is yours only under the
#   lane that took it: a lane never beats, reviews or releases another lane's claim. The human cap
#   (default 8) still counts every claim of the human, lanes or not. Subagents still never call
#   claim.sh: the parent takes one lane per subagent and does all the bookkeeping. --human claims
#   have no lane.
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
#   KOINON_AGENT_SESSION / CLAUDE_CODE_SESSION_ID   the session id, in that order. Without either, a
#                               holder is generated once per worktree and stored under that
#                               worktree's git dir. Claude subagents share the parent's id: only the
#                               parent takes claims. Other agents must export KOINON_AGENT_SESSION.
#   KOINON_CLAIM_LEASE_SECONDS (7200), _HUMAN_LEASE_SECONDS (259200), _GRACE_SECONDS (900),
#   _HUMAN_CAP (8), _SESSION_CAP (1: per session and lane),
#   KOINON_CLAIM_LANE           the lane (same as --lane)
#   KOINON_CLAIM_WAIT_INTERVAL  poll interval of wait-release in seconds (30)
#   _STALE_RELEASE_SECONDS (7200: the sweeper releases this long after the lease ran out)
#   _OUTSIDER_LEASE_SECONDS (1209600), _OUTSIDER_CAP (2), _OUTSIDER_WARN_SECONDS (345600: the
#   sweeper reminds an outsider this long before the claim runs out, day 10 of 14)

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

LEASE=${KOINON_CLAIM_LEASE_SECONDS:-7200}
HUMAN_LEASE=${KOINON_CLAIM_HUMAN_LEASE_SECONDS:-259200}
HUMAN_MODE=0
GRACE=${KOINON_CLAIM_GRACE_SECONDS:-900}
HUMAN_CAP=${KOINON_CLAIM_HUMAN_CAP:-8}
SESSION_CAP=${KOINON_CLAIM_SESSION_CAP:-1}
RELEASE_AFTER=${KOINON_CLAIM_STALE_RELEASE_SECONDS:-7200}
OUTSIDER_LEASE=${KOINON_CLAIM_OUTSIDER_LEASE_SECONDS:-1209600}
OUTSIDER_CAP=${KOINON_CLAIM_OUTSIDER_CAP:-2}
OUTSIDER_WARN=${KOINON_CLAIM_OUTSIDER_WARN_SECONDS:-345600}
# The labels that open an issue to outsider claims (ADR 0094 decision 1).
OUTSIDER_LABELS='["good first issue","help wanted"]'
# The fixed title of the one Backlog issue the write-refused fallback opens (deduplicated by it).
FALLBACK_TITLE='Comment claims: GitHub refused the claim write for an outside contributor'
MARKER='<!-- koinon-claim -->'
REMINDER_MARKER='<!-- koinon-claim-reminder'
EMPTY_TREE=4b825dc642cb6eb9a060e54bf8d69288fbee4904

# ---------------------------------------------------------------------------------------------
# Pure functions (no network); hack/claim_test.sh sources this file with CLAIM_LIB_ONLY=1.
# ---------------------------------------------------------------------------------------------

die() { echo "claim: $*" >&2; exit "${CLAIM_EXIT:-1}"; }
warn() { echo "claim: warning: $*" >&2; }
# write_failed MSG: an issue write (unassign, label, comment) failed. In Actions the job has
# issues: write, so that is a real fault and fails the job; locally it only warns (koinon#315).
write_failed() {
  if [[ ${GITHUB_ACTIONS:-} == true ]]; then die "$*"; fi
  warn "$*"
}

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

# claim_format ISSUE STATE HOLDER HUMAN LEASE_UNTIL PR NOTE [RELEASED_BY] -> the commit message.
# RELEASED_BY (ADR 0094 decision 7) is written only on a release, as its own line; a commit without
# it is a claim from before the field existed and parses with released_by "".
claim_format() {
  local note=${7//$'\n'/ } by=${8:-}
  note=${note//$'\r'/ }
  by=${by//$'\n'/ }
  by=${by//$'\r'/ }
  printf 'claim #%s: %s\n\nholder: %s\nhuman: %s\nlease_until: %s\npr: %s\nnote: %s\n' \
    "$1" "$2" "$3" "$4" "$5" "$6" "$note"
  if [[ -n $by ]]; then printf 'released_by: %s\n' "$by"; fi
  printf 'state: %s\n' "$2"
}

# JQ_PARSE: jq definition turning a claim commit message (string) into an object.
# shellcheck disable=SC2016
JQ_PARSE='def claim_parse:
  ([split("\n")[] | capture("^(?<k>holder|human|lease_until|pr|note|released_by|state): ?(?<v>.*)$")]
    | map({(.k): .v}) | add // {})
  | {state: (.state // ""), holder: (.holder // ""), human: (.human // ""),
     lease_until: (.lease_until // ""), pr: (.pr // ""), note: (.note // ""),
     released_by: (.released_by // "")};'

# claim_parse MESSAGE -> compact JSON {state,holder,human,lease_until,pr,note,released_by}.
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

# sensitive_paths CODEOWNERS_FILE -> the path patterns of the section after the `# Sensitive paths`
# marker, as ruleset patterns (no leading slash; a directory gets `**`). One source of truth: the
# main ruleset's required_reviewers rule is configured from this list.
sensitive_paths() {
  [[ -r $1 ]] || return 0
  awk '/^# Sensitive paths/ {on=1; next} on && /^\// {print $1}' "$1" | sed 's#^/##; s#/$#/**#'
}

# next_adr_number NUMBERS...: the highest number given plus one, zero padded to four digits.
next_adr_number() {
  local max=0 n
  for n in "$@"; do ((10#$n > max)) && max=$((10#$n)); done
  printf '%04d' $((max + 1))
}

# holder is `<login>/<host>/<session>` or, in a lane, `<login>/<host>/<session>#<lane>`. Session ids
# never contain `#` (my_holder maps it to `-`), so the first `#` of the last segment starts the lane.
# holder_login HOLDER, holder_session HOLDER (without the lane), holder_lane HOLDER (empty: no lane),
# holder_key HOLDER (session and lane: what the session cap counts).
holder_login() { echo "${1%%/*}"; }
holder_key() { echo "${1##*/}"; }
holder_session() { local k=${1##*/}; echo "${k%%#*}"; }
holder_lane() { local k=${1##*/}; if [[ $k == *'#'* ]]; then echo "${k#*#}"; fi; }

# lane_label HOLDER: "lane x" or "no lane", for messages.
lane_label() { local l; l=$(holder_lane "$1"); if [[ -n $l ]]; then echo "lane $l"; else echo "no lane"; fi; }

# valid_lane LANE: ^[a-z0-9][a-z0-9-]{0,15}$
valid_lane() { [[ $1 =~ ^[a-z0-9][a-z0-9-]{0,15}$ ]]; }

# LANE: this invocation's lane ("" = none); KOINON_CLAIM_LANE, overridden by --lane (split_globals).
LANE=${KOINON_CLAIM_LANE:-}

# split_globals ARGS...: takes --human and --lane L out of the arguments. Sets HUMAN_MODE, LANE and
# REST (the other arguments). Returns 1 with a message on a bad lane.
REST=()
split_globals() {
  REST=()
  while (($#)); do
    case $1 in
      --human) HUMAN_MODE=1; shift ;;
      --lane)
        if (($# < 2)); then echo "claim: --lane needs a name" >&2; return 1; fi
        LANE=$2; shift 2 ;;
      *) REST+=("$1"); shift ;;
    esac
  done
  if [[ -n $LANE ]] && ! valid_lane "$LANE"; then
    echo "claim: bad lane '$LANE': use 1 to 16 characters of a-z, 0-9 and -, starting with a letter or digit" >&2
    return 1
  fi
  return 0
}

# lower S: S in lower case. Logins are compared with it (GitHub logins are case-insensitive).
lower() { printf '%s' "${1,,}"; }

# same_holder A B: same login, session and lane (the host name can change, e.g. macOS hostname -s).
same_holder() {
  # GitHub logins are case-insensitive; the host/session key is not.
  [[ $(lower "$(holder_login "$1")") == "$(lower "$(holder_login "$2")")" && $(holder_key "$1") == "$(holder_key "$2")" ]]
}

# parse_lease LEASE_UNTIL -> epoch, or fail on anything that is not RFC 3339 UTC.
parse_lease() {
  [[ $1 =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || return 1
  iso_to_epoch "$1" 2>/dev/null
}

# is_lost_race OUTPUT: only these three 422s mean another writer won. Any other 422 (a ruleset
# violation, a bad sha) is an error, not a lost claim.
is_lost_race() {
  [[ $1 == *"Reference already exists"* || $1 == *"not a fast forward"* || $1 == *"Reference cannot be updated"* ]]
}

# holder_class HOLDER -> agent | person | outsider (ADR 0094 decision 1). Person claims are
# `<login>/<host>/human` (take --human) and `<login>/github/human` (/claim by a maintainer); outsider
# claims are `<login>/github/outsider`. Session ids are never `human` or `outsider` (my_base_holder).
holder_class() {
  case $(holder_session "$1") in
    human) echo person ;;
    outsider) echo outsider ;;
    *) echo agent ;;
  esac
}

# lease_for_holder HOLDER -> lease seconds of the holder's class.
lease_for_holder() {
  case $(holder_class "$1") in
    person) echo "$HUMAN_LEASE" ;;
    outsider) echo "$OUTSIDER_LEASE" ;;
    *) echo "$LEASE" ;;
  esac
}

# stale_after HOLDER -> seconds after liveness at which the sweeper marks the claim stale: the grace
# for agents and people, none for outsiders (they were reminded 4 days before, ADR 0094 decision 3).
stale_after() { if [[ $(holder_class "$1") == outsider ]]; then echo 0; else echo "$GRACE"; fi; }

# closing_issues BODY -> issue numbers named by closing keywords, one per line.
closing_issues() {
  printf '%s\n' "$1" | grep -oiE '\b(close[sd]?|fix(e[sd])?|resolve[sd]?)[: ]+#[0-9]+' | grep -oE '[0-9]+$' | sort -un || true
}

# claim_verdict STATE HUMAN AUTHOR NOW LIVE PR_STATE -> free | own | stale | conflict: what a claim
# means for a pull request by AUTHOR (ADR 0094 decision 2). Live is `held` and not what the sweeper
# would release now: a claim whose pull request is ready for review is never swept, one whose pull
# request closed is released, any other is released RELEASE_AFTER after its liveness (LIVE).
claim_verdict() {
  local state=$1 human=$2 author=$3 now=$4 live=$5 prs=$6
  if [[ $state != held ]]; then echo free; return; fi
  if [[ $(lower "$human") == "$(lower "$author")" ]]; then echo own; return; fi
  if [[ $prs == closed ]]; then echo stale; return; fi
  if [[ $prs != ready ]] && ((now > live + RELEASE_AFTER)); then echo stale; return; fi
  echo conflict
}

# The fixed failure texts of the `claim` check (ADR 0094 decision 2). hack/claim_test.sh pins them:
# outsiders and agents match on them.
# conflict_issue_text N HUMAN UNTIL PR AUTHOR RELEASE_AT (PR "" = no pull request yet)
conflict_issue_text() {
  local pr="no pull request yet"
  [[ -z $4 ]] || pr="pull request #$4"
  printf '::error::#%s is claimed by @%s until %s (%s); this pull request is by @%s. Ask @%s on #%s to comment /release, or ask a maintainer. If the claim goes stale the sweeper frees it at %s, and this check runs again when it does.\n' \
    "$1" "$2" "$3" "$pr" "$5" "$2" "$1" "$6"
}
# conflict_adr_text PATH NNNN HUMAN LEASE
conflict_adr_text() {
  printf '::error::%s uses ADR number %s, which @%s reserved (claim/adr-%s) until %s. Take a free number with hack/claim.sh adr (a maintainer can do it for you) and rename the file.\n' \
    "$1" "$2" "$3" "$2" "$4"
}

# comment_command BODY -> claim | release | nothing: a comment is a command only when its first line
# is exactly /claim or /release (ADR 0094 decision 1).
comment_command() {
  local first=${1%%$'\n'*}
  first=${first%$'\r'}
  if [[ $first =~ ^/(claim|release)[[:space:]]*$ ]]; then echo "${BASH_REMATCH[1]}"; fi
}

# perm_class PERMISSION -> person | outsider: the `permission` of collaborators/{login}/permission.
perm_class() { case $1 in admin | maintain | write) echo person ;; *) echo outsider ;; esac; }

# comment_holder LOGIN CLASS -> the holder a comment claim writes.
comment_holder() { if [[ $2 == person ]]; then echo "$1/github/human"; else echo "$1/github/outsider"; fi; }

# is_denied OUTPUT: GitHub refused a write for lack of permission (the comment fallback's trigger): HTTP
# 401 or 403, or GraphQL's "Resource not accessible". Only those: a ruleset refusal is a 422 whose text
# can say "permission", and answering it as a read-only token would hide a real failure.
is_denied() {
  [[ $1 == *"HTTP 401"* || $1 == *"HTTP 403"* || $1 == *"Resource not accessible"* ]]
}

# The answers of the comment commands (refusals are always answered, never silent).
# shellcheck disable=SC2016
text_held() { printf '@%s #%s is claimed by @%s until %s, so it cannot be claimed now. Ask @%s to comment `/release`, or ask a maintainer.\n' "$1" "$2" "$3" "$4" "$3"; }
text_closed() { printf '@%s #%s is closed; there is nothing to claim.\n' "$1" "$2"; }
# shellcheck disable=SC2016
text_label() { printf '@%s maintainers have not opened #%s to outside claims (it has neither the `good first issue` nor the `help wanted` label). A pull request that says `Closes #%s` is welcome anyway.\n' "$1" "$2" "$2"; }
# shellcheck disable=SC2016
text_cap() { printf '@%s you already hold %s live claim(s), the cap is %s: %s. Finish one or comment `/release` on it first.\n' "$1" "$2" "$3" "$4"; }
# shellcheck disable=SC2016
text_race() { printf '@%s another claim on #%s was written at the same moment; comment `/claim` again to retry.\n' "$1" "$2"; }
text_no_claim() { printf '@%s #%s has no held claim; there is nothing to release.\n' "$1" "$2"; }
text_release_refused() { printf '@%s only @%s (whose claim it is) or a maintainer can release #%s.\n' "$1" "$2" "$3"; }
# shellcheck disable=SC2016
text_fallback() { printf '@%s comment claims are not available for outside contributors yet; open a pull request that says Closes #%s instead. An open pull request already makes agents skip this issue.\n' "$1" "$2"; }
# shellcheck disable=SC2016
text_unassignable() { printf 'GitHub did not allow assigning @%s (only collaborators, org members with read access and people who commented here can be assigned); the claim is recorded in the ref `claim/%s`.\n' "$1" "$2"; }

# renew_hint CLASS N -> how the holder of that class keeps or ends the claim (sweeper messages).
# shellcheck disable=SC2016
renew_hint() {
  case $1 in
    person) printf 'If the work continues, comment `/claim` (or run `hack/claim.sh beat %s --human`); otherwise comment `/release`.' "$2" ;;
    outsider) printf 'If the work continues, comment `/claim` or push to your pull request; otherwise comment `/release`.' ;;
    *) printf 'If the work continues, run `hack/claim.sh beat %s`; otherwise `hack/claim.sh release %s --note ...`.' "$2" "$2" ;;
  esac
}

# JQ_PR_CLOSERS: the open pull requests (GraphQL pages, slurped) as [{number, author, issues}], issues
# being what the pull request closes: GitHub's closingIssuesReferences in this repository plus the
# closing keywords of its body (the claim check reads the body).
# shellcheck disable=SC2016
JQ_PR_CLOSERS='[.[].data.repository.pullRequests.nodes[]
  | {number, author: (.author.login // ""),
     issues: ([(.closingIssuesReferences.nodes // [])[] | select(.repository.nameWithOwner == $repo) | .number]
              + [(.body // "") | scan("(?i)\\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)[: ]+#([0-9]+)\\b") | .[0] | tonumber]
              | unique)}]'

# JQ_NEXT: the pickable items of the board pages (slurped), sorted by priority then age. Args: $repo,
# $ms, $label, $held (issue numbers with a held claim), $prs (issue numbers an open pull request
# closes), $human (the caller's human). In Progress and In Review count as Ready when nothing holds
# the issue: the bots make no board writes, so a released claim can leave the item there (ADR 0094
# decision 3).
# shellcheck disable=SC2016
JQ_NEXT='[.[].data.organization.projectV2.items.nodes[]
  | select(.content.number != null and .content.state == "OPEN" and .content.repository.nameWithOwner == $repo)
  | select(.status.name // "" | IN("Ready", "In Progress", "In Review"))
  | select([.content.labels.nodes[].name] | index("agent-ok"))
  | select($ms == "" or .content.milestone.title == $ms)
  | select($label == "" or ([.content.labels.nodes[].name] | index($label)))
  | select(.content.number as $n | ($held | index($n) | not) and ($prs | index($n) | not))
  | select([(.content.assignees.nodes // [])[].login | ascii_downcase] | all(. == ($human | ascii_downcase)))
  | {n: .content.number, p: (.priority.name // ""), c: (.component.name // ""), t: .content.title, at: .content.createdAt}]
  | sort_by([(.p | rank), .at])'

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
# CLOSERS: open_pr_closers of this `take` (JSON), read by take_one.
CLOSERS="[]"
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

host_short() { hostname -s 2>/dev/null || echo host; }

# my_holder -> `<login>/<host>/<session>`, plus `#<lane>` when a lane is set (human claims have none). The session is KOINON_AGENT_SESSION, then Claude Code's
# CLAUDE_CODE_SESSION_ID. Without either, one holder is generated per worktree and stored whole under
# that worktree's git dir (never the shared .git/config: worktrees would share one id).
my_holder() {
  local h
  h=$(my_base_holder) || return 1
  if [[ -n $LANE && $h != */human ]]; then echo "$h#$LANE"; else echo "$h"; fi
}

my_base_holder() {
  local login s gd f
  login=$(human_login)
  if ((HUMAN_MODE)); then echo "$login/$(host_short)/human"; return; fi
  s=${KOINON_AGENT_SESSION:-${CLAUDE_CODE_SESSION_ID:-}}
  if [[ $s == human || $s == outsider ]]; then echo "claim: the session id '$s' names a claim class; pick another KOINON_AGENT_SESSION" >&2; return 1; fi
  if [[ -n $s ]]; then echo "$login/$(host_short)/${s//[^A-Za-z0-9._-]/-}"; return; fi
  gd=$(git rev-parse --git-dir 2>/dev/null) || { echo "claim: no session id: export KOINON_AGENT_SESSION" >&2; return 1; }
  f=$gd/koinon-claim-holder
  if [[ -r $f && $(cut -d/ -f1 "$f") == "$login" ]]; then cat "$f"; return; fi
  s="$login/$(host_short)/s$(date +%s)-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
  printf '%s\n' "$s" >"$f" || { echo "claim: cannot store the holder in $gd" >&2; return 1; }
  echo "$s"
}

session_id() { local h; h=$(my_holder) || return 1; holder_key "$h"; }

# read_claim N -> JSON {issue,sha,state,holder,human,lease_until,pr,note,released_by}.
# Returns 0 found, 1 no claim ref, 4 API error (message on stderr). Callers must tell 1 from 4: a
# network blip is not a lost claim.
read_claim() {
  local out
  # shellcheck disable=SC2016
  out=$(gh api graphql -f o="$OWNER" -f n="$NAME" -f q="refs/heads/claim/$1" -f query='
    query($o:String!,$n:String!,$q:String!){repository(owner:$o,name:$n){ref(qualifiedName:$q){target{... on Commit{oid message}}}}}' \
    --jq '.data.repository.ref.target // empty | [.oid, .message] | @json') || { echo "claim: cannot read claim/$1" >&2; return 4; }
  [[ -n $out ]] || return 1
  # N is an issue number, or adr-NNNN for an ADR number reservation (issue stays that string).
  jq -c --arg n "$1" "$JQ_PARSE"'
    . as [$sha, $msg] | ($msg | claim_parse) + {issue: ($n | tonumber? // $n), sha: $sha}' <<<"$out" || return 4
}

# load_claim N: sets C and returns read_claim's code; code 4 dies (exit 1), never "lost".
load_claim() {
  local rc=0
  C=$(read_claim "$1") || rc=$?
  if ((rc == 4)); then die "cannot read the claim on #$1 (API error); retry, the claim is not known lost"; fi
  return "$rc"
}

# list_claims -> JSON array of every claim (numeric claim/<N> refs only).
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

# write_claim ISSUE STATE HOLDER HUMAN LEASE PR NOTE [PARENT_SHA] [RELEASED_BY]
# Creates the commit, then the ref (no parent) or fast-forwards it. Returns 0 won, 1 lost the
# compare-and-set; any other failure (including other 422s, e.g. a ruleset) dies. With CLAIM_SOFT_DENY
# set (the comment commands), a write GitHub refuses for lack of permission returns 5 instead of dying.
CLAIM_SOFT_DENY=
write_claim() {
  local issue=$1 parent=${8:-} msg sha
  msg=$(claim_format "$issue" "$2" "$3" "$4" "$5" "$6" "$7" "${9:-}")
  local args=(-X POST "repos/$REPO/git/commits" -f "message=$msg" -f "tree=$EMPTY_TREE")
  [[ -z $parent ]] || args+=(-f "parents[]=$parent")
  gh_try "${args[@]}" --jq .sha
  if ((TRY_RC != 0)); then
    if [[ -n $CLAIM_SOFT_DENY ]] && is_denied "$TRY_OUT"; then return 5; fi
    die "cannot create the claim commit: $TRY_OUT"
  fi
  sha=$TRY_OUT
  if [[ -z $parent ]]; then
    gh_try -X POST "repos/$REPO/git/refs" -f "ref=refs/heads/claim/$issue" -f "sha=$sha"
  else
    gh_try -X PATCH "repos/$REPO/git/refs/heads/claim/$issue" -f "sha=$sha" -F force=false
  fi
  if ((TRY_RC == 0)); then return 0; fi
  if [[ $TRY_OUT == *"HTTP 422"* ]] && is_lost_race "$TRY_OUT"; then return 1; fi
  if [[ -n $CLAIM_SOFT_DENY ]] && is_denied "$TRY_OUT"; then return 5; fi
  die "claim write failed: $TRY_OUT"
}

# issue_json N -> {number,state,title,pull,labels[],assignees[],milestone}
issue_json() {
  gh api "repos/$REPO/issues/$1" --jq '{number, state, title, pull: (.pull_request != null),
    labels: [.labels[].name], assignees: [.assignees[]?.login], milestone: (.milestone.title // "")}'
}

# find_pr N [AUTHOR] -> number of the open PR whose body closes issue N (empty if none); with AUTHOR,
# only a pull request by that login (a claim links and draws liveness only from its own human's).
find_pr() {
  gh pr list -R "$REPO" --state open --limit 100 --json number,body,author |
    jq -r --arg n "$1" --arg a "$(lower "${2:-}")" '.[] | select(.body // "" | test("(?i)\\b(close[sd]?|fix(e[sd])?|resolve[sd]?)[: ]+#" + $n + "\\b"))
      | select($a == "" or ((.author.login // "") | ascii_downcase) == $a) | .number' | head -1
}

# open_pr_closers -> the open pull requests as JSON [{number, author, issues}] (JQ_PR_CLOSERS). An API
# error fails: `next` and `take` must not hand out an issue from a partial view.
open_pr_closers() {
  local pages
  # shellcheck disable=SC2016
  pages=$(gh api graphql --paginate -f o="$OWNER" -f n="$NAME" -f query='
    query($o:String!,$n:String!,$endCursor:String){repository(owner:$o,name:$n){
      pullRequests(states:OPEN,first:50,after:$endCursor){pageInfo{hasNextPage endCursor}
        nodes{number body author{login} closingIssuesReferences(first:25){nodes{number repository{nameWithOwner}}}}}}}' | jq -s .) || return 1
  jq -c --arg repo "$REPO" "$JQ_PR_CLOSERS" <<<"$pages"
}

# pr_conflicts CLOSERS N HUMAN [PREV_HUMAN] -> "#P by @author" for each open pull request that closes
# N and is by neither HUMAN nor PREV_HUMAN (the human of a dead claim being taken over), one per line.
pr_conflicts() {
  jq -r --argjson n "$2" --arg h "$(lower "$3")" --arg p "$(lower "${4:-}")" '
    .[] | select(.issues | index($n)) | select((.author | ascii_downcase) as $a | $a != $h and ($p == "" or $a != $p))
    | "#\(.number) by @\(.author)"' <<<"$1"
}

# own_pr CLOSERS N HUMAN -> the first open pull request by HUMAN that closes N (empty if none).
own_pr() {
  jq -r --argjson n "$2" --arg h "$(lower "$3")" '
    [.[] | select((.issues | index($n)) and (.author | ascii_downcase) == $h) | .number] | first // empty' <<<"$1"
}

# other_assignees ISSUE_JSON HUMAN [PREV_HUMAN] -> the assignees that are neither, space separated.
other_assignees() {
  jq -r --arg h "$(lower "$2")" --arg p "$(lower "${3:-}")" '
    [(.assignees // [])[] | select((ascii_downcase) as $a | $a != $h and ($p == "" or $a != $p)) | "@" + .] | join(" ")' <<<"$1"
}

# open_prs_closing N -> every open PR number that closes issue N.
open_prs_closing() {
  gh pr list -R "$REPO" --state open --limit 100 --json number,body |
    jq -r --arg n "$1" '.[] | select(.body // "" | test("(?i)\\b(close[sd]?|fix(e[sd])?|resolve[sd]?)[: ]+#" + $n + "\\b")) | .number'
}

# claim_liveness CLAIM_JSON: the one liveness rule, shared by take --takeover and the sweeper.
# Sets LIVE (epoch: the later of the lease and the last human PR commit or comment plus one lease),
# PR_NUM and PR_STATE (none | draft | ready | closed). Bot comments and commits are not liveness.
# Returns 1 for an unparsable lease, 4 for an API error.
claim_liveness() {
  local c=$1 lease_s n holder pr j last act
  lease_s=$(parse_lease "$(jq -r .lease_until <<<"$c")") || return 1
  n=$(jq -r .issue <<<"$c"); holder=$(jq -r .holder <<<"$c")
  LIVE=$lease_s; PR_STATE=none; PR_NUM=
  pr=$(jq -r .pr <<<"$c")
  # Only a pull request of the claim's own human is the claim's activity.
  if [[ -z $pr ]]; then pr=$(find_pr "$n" "$(jq -r .human <<<"$c")") || return 4; fi
  [[ -n $pr ]] || return 0
  PR_NUM=$pr
  gh_try "repos/$REPO/pulls/$pr"
  if ((TRY_RC != 0)); then
    [[ $TRY_OUT == *"HTTP 404"* ]] && return 0
    return 4
  fi
  case $(jq -r 'if .merged then "closed" elif .state == "closed" then "closed" elif .draft then "draft" else "ready" end' <<<"$TRY_OUT") in
    closed) PR_STATE=closed; return 0 ;;
    draft) PR_STATE=draft ;;
    *) PR_STATE=ready ;;
  esac
  j=$(gh pr view "$pr" -R "$REPO" --json commits,comments 2>/dev/null) || return 4
  last=$(jq -r '[(.commits[] | select([.authors[].login] | any(test("\\[bot\\]$|-bot$")) | not) | .committedDate),
                 (.comments[] | select((.author.login // "") | test("\\[bot\\]$|-bot$|^app/") | not) | .createdAt)] | max // empty' <<<"$j") || return 4
  if [[ -n $last ]]; then
    act=$(iso_to_epoch "$last")
    if ((act + $(lease_for_holder "$holder") > LIVE)); then LIVE=$((act + $(lease_for_holder "$holder"))); fi
  fi
  return 0
}

# ---- board mirrors (best effort) ----

PROJECT_META=
# board_unreadable: the project could not be read. In Actions that is the expected state (GITHUB_TOKEN
# cannot read or write an organization Project v2; ADR 0094 decision 3): one log line, no warning, and
# the board's own workflows set Done. Elsewhere it is a missing token scope.
board_unreadable() {
  if [[ ${GITHUB_ACTIONS:-} == true ]]; then
    echo "claim: board update skipped: the workflow token cannot write the organization project (the board workflows set Done)"
  else
    warn "cannot read project $PROJECT_OWNER/$PROJECT (gh auth refresh -s project?)"
  fi
}
project_meta() {
  [[ $PROJECT != none ]] || return 1
  if [[ -z $PROJECT_META ]]; then
    # shellcheck disable=SC2016
    PROJECT_META=$(gh api graphql -f o="$PROJECT_OWNER" -F n="$PROJECT" -f query='
      query($o:String!,$n:Int!){organization(login:$o){projectV2(number:$n){id fields(first:50){nodes{
        ... on ProjectV2SingleSelectField{id name options{id name}} ... on ProjectV2Field{id name}}}}}}' \
      --jq .data.organization.projectV2 2>/dev/null) || { board_unreadable; PROJECT=none; return 1; }
  fi
}

# set_status N OPTION: set the Project Status single-select; warns and returns 1 on any problem.
set_status() {
  project_meta || return 1
  local pid fid oid item cid res
  pid=$(jq -r .id <<<"$PROJECT_META")
  fid=$(jq -r '.fields.nodes[] | select(.name == "Status") | .id' <<<"$PROJECT_META")
  oid=$(jq -r --arg o "$2" '.fields.nodes[] | select(.name == "Status") | .options[]? | select(.name == $o) | .id' <<<"$PROJECT_META")
  if [[ -z $fid || -z $oid ]]; then warn "project has no Status option '$2'"; return 1; fi
  # shellcheck disable=SC2016
  local q='query($o:String!,$n:String!,$i:Int!){repository(owner:$o,name:$n){issue(number:$i){id projectItems(first:20){nodes{id project{id}}}}}}'
  res=$(gh api graphql -f o="$OWNER" -f n="$NAME" -F i="$1" -f query="$q" --jq .data.repository.issue 2>/dev/null) ||
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
unassign() { gh api -X DELETE "repos/$REPO/issues/$1/assignees" -f "assignees[]=$2" >/dev/null 2>&1 || write_failed "cannot unassign $2 from #$1"; }
add_label() { gh api -X POST "repos/$REPO/issues/$1/labels" -f "labels[]=$2" >/dev/null 2>&1 || write_failed "cannot label #$1 $2"; }
drop_label() { gh api -X DELETE "repos/$REPO/issues/$1/labels/$2" >/dev/null 2>&1 || true; }

# upsert_comment N BODY: the one marked comment on the issue, edited in place.
upsert_comment() {
  local id
  id=$(gh api --paginate "repos/$REPO/issues/$1/comments" --jq ".[] | select(.body | startswith(\"$MARKER\")) | .id" 2>/dev/null | head -1 || true)
  if [[ -n $id ]]; then
    gh api -X PATCH "repos/$REPO/issues/comments/$id" -f "body=$2" >/dev/null 2>&1 || write_failed "cannot edit the claim comment on #$1"
  else
    gh api -X POST "repos/$REPO/issues/$1/comments" -f "body=$2" >/dev/null 2>&1 || write_failed "cannot comment on #$1"
  fi
}

# claim_comment STATE HOLDER HUMAN LEASE PR NOTE [EXTRA]
claim_comment() {
  local pr=${5:-none}
  [[ $pr == "" ]] && pr=none
  # shellcheck disable=SC2016
  printf '%s\n**Work claim: %s** by `%s`\n\n- holder: `%s`\n- lease until: %s\n- pull request: %s\n- note: %s\n%s\n_Managed by `hack/claim.sh` (ADR 0082); the claim ref `claim/<issue>` is the source of truth._\n' \
    "$MARKER" "$1" "$3" "$2" "${4:-n/a}" "$pr" "${6:-none}" "${7:+$'\n'$7$'\n'}"
}

# rerun_claim_check PR: after a take, re-run the failed `claim` check of that pull request (best effort).
rerun_claim_check() {
  local branch id
  branch=$(gh pr view "$1" -R "$REPO" --json headRefName --jq .headRefName 2>/dev/null) || return 0
  id=$(gh run list -R "$REPO" -w claims.yml --branch "$branch" --limit 10 --json databaseId,conclusion,event \
    --jq '[.[] | select(.conclusion == "failure" and (.event | startswith("pull_request")))][0].databaseId // empty' 2>/dev/null) || return 0
  [[ -z $id ]] || gh run rerun "$id" -R "$REPO" --failed >/dev/null 2>&1 || true
}

# rerun_checks_for_issue N: after a claim changed (released, claimed by comment), re-run the failed
# `claim` check of every open pull request that closes N (best effort; needs actions: write in CI).
rerun_checks_for_issue() {
  local p
  for p in $(open_prs_closing "$1" 2>/dev/null || true); do rerun_claim_check "$p"; done
  return 0
}

# ---------------------------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------------------------

active_counts() { # CLAIMS_JSON NOW HUMAN KEY -> "human_count key_count"; KEY is session[#lane] (holder_key)
  jq -r --argjson now "$2" --arg h "$3" --arg s "$4" '
    [.[] | select(.state == "held" and (.lease_until | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z$")))] as $held
    | ($held | map(select((.lease_until | fromdateiso8601) >= $now))) as $act
    | "\($act | map(select(.human == $h)) | length) \($act | map(select((.holder | split("/") | last) == $s and ($s | IN("human", "outsider") | not))) | length)"' <<<"$1"
}

# held_by_key CLAIMS_JSON NOW KEY -> the active claims of that session and lane, one per line:
# "#12 (PR koinon-private#34)" or "#12" when no pull request is known.
held_by_key() {
  jq -r --argjson now "$2" --arg s "$3" '
    .[] | select(.state == "held" and (.lease_until | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z$"))
                 and (.lease_until | fromdateiso8601) >= $now
                 and (.holder | split("/") | last) == $s and ($s | IN("human", "outsider") | not))
    | "#\(.issue)" + (if .pr != "" then " (PR #\(.pr))" else "" end)' <<<"$1"
}

# live_claims_of CLAIMS_JSON NOW LOGIN [CLASS] -> "#N" for each held claim of human LOGIN whose lease
# has not run out, only of holder class CLASS when given; one per line (the comment caps).
live_claims_of() {
  jq -r --argjson now "$2" --arg l "$(lower "$3")" --arg cls "${4:-}" '
    .[] | select(.state == "held" and (.human | ascii_downcase) == $l
                 and (.lease_until | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z$")) and (.lease_until | fromdateiso8601) >= $now
                 and ($cls == "" or ((.holder | split("/") | last) as $k
                      | if $cls == "outsider" then $k == "outsider" elif $cls == "person" then $k == "human" else ($k != "human" and $k != "outsider") end)))
    | "#\(.issue)"' <<<"$1"
}

cmd_limits() {
  local claims now human sess counts
  claims=$(list_claims); now=$(server_now); human=$(human_login); sess=$(session_id)
  counts=$(active_counts "$claims" "$now" "$human" "$sess")
  echo "human:    $human (${counts% *} of $HUMAN_CAP held claims)"
  echo "session:  ${sess%%#*} (${counts#* } of $SESSION_CAP${LANE:+ in lane $LANE})"
  echo "lease:    agent ${LEASE}s, heartbeat every 1800s; human claim (--human) ${HUMAN_LEASE}s, heartbeat daily"
  echo "takeover: after lease + ${GRACE}s grace, and never while the linked PR is open and not draft"
  echo "sweeper:  stale mark after the lease + ${GRACE}s; release ${RELEASE_AFTER}s after the lease ran out"
}

cmd_mine() {
  local claims now human
  claims=$(list_claims); now=$(server_now); human=$(human_login)
  jq -r --argjson now "$now" --arg h "$human" --arg lane "$LANE" '
    .[] | select(.state == "held" and .human == $h and (.lease_until | test("^[0-9]{4}-"))
                 and ($lane == "" or ((.holder | split("/") | last | split("#") | .[1:] | first // "") == $lane)))
    | "#\(.issue)\t\(if (.lease_until | fromdateiso8601) >= $now then "active" else "expired" end)\tlease \(.lease_until)\tpr \(.pr)\t\(.holder)"' <<<"$claims"
}

cmd_show() {
  local rc=0 now status lease
  load_claim "$1" || rc=$?
  if ((rc == 1)); then echo "#$1: no claim"; return 0; fi
  now=$(server_now)
  lease=0
  parse_lease "$(jq -r .lease_until <<<"$C")" >/dev/null && lease=$(parse_lease "$(jq -r .lease_until <<<"$C")")
  status=$(claim_status "$(jq -r .state <<<"$C")" "$lease" "$now")
  jq -r --arg st "$status" '"#\(.issue): \($st)\n  state:       \(.state)\n  holder:      \(.holder)\n  human:       \(.human)\n  lease_until: \(.lease_until)\n  pr:          \(.pr)\n  note:        \(.note)\n  ref sha:     \(.sha)"' <<<"$C"
}

cmd_next() {
  local label="" claims ms json held
  if [[ ${1:-} == --label ]]; then label=${2:-}; [[ -n $label ]] || die "usage: next [--label L]"; fi
  [[ $PROJECT != none ]] || die "next needs the project board (KOINON_CLAIM_PROJECT)"
  ms=${KOINON_CLAIM_MILESTONE:-}
  if [[ -z $ms ]]; then
    ms=$(gh api "repos/$REPO/milestones?state=open" --jq 'sort_by(.due_on // "9999", .number) | .[0].title // empty')
  fi
  local human closers prs
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
      --arg human "$human" "$JQ_RANK$JQ_NEXT")
  local n p c t blockers
  while IFS=$'\037' read -r n p c t; do
    blockers=$(gh api "repos/$REPO/issues/$n/dependencies/blocked_by" --jq '[.[] | select(.state == "open")] | length' 2>/dev/null || echo 0)
    [[ $blockers == 0 ]] || continue
    printf '#%s\t%s\t%s\t%s\n' "$n" "${p:--}" "${c:--}" "$t"
  done < <(jq -r '.[] | [.n, .p, .c, .t] | map(tostring) | join("\u001f")' <<<"$json")
}

warn_area_overlap() { # N CLAIMS_JSON NOW
  local labels l other
  labels=$(gh api "repos/$REPO/issues/$1/labels" --jq '.[].name | select(startswith("area:"))' 2>/dev/null || true)
  for l in $labels; do
    for other in $(jq -r --argjson now "$3" --argjson me "$1" '.[] | select(.state == "held" and .issue != $me and (.lease_until | test("^[0-9]{4}-")) and (.lease_until | fromdateiso8601) >= $now) | .issue' <<<"$2"); do
      if gh api "repos/$REPO/issues/$other/labels" --jq '.[].name' 2>/dev/null | grep -qxF "$l"; then
        warn "#$other is also claimed and shares $l; coordinate on files you both touch"
      fi
    done
  done
}

# holder_desc CLAIM_JSON CALLER_HUMAN -> who holds a claim, for messages: the full holder for an agent
# claim of the caller's own human (its sessions), else the human and the class, never the session.
holder_desc() {
  local holder human cls
  holder=$(jq -r .holder <<<"$1"); human=$(jq -r .human <<<"$1"); cls=$(holder_class "$holder")
  if [[ $cls == agent && $(lower "$human") == "$(lower "$2")" ]]; then echo "$holder"; else echo "@$human ($cls claim)"; fi
}

# dead_holder_check N CLAIM_JSON NOW [CALLER_HUMAN]: the one dead-holder rule of `take --takeover`,
# `adopt` and a /claim over a dead claim, with the lease of the holder's class (ADR 0094 decision 5).
# 0: the holder is dead and the claim may be taken over (LIVE is set); 2: refused, reason printed
# (the holder is alive or in grace, its liveness is unknown, or a pull request is ready for review).
dead_holder_check() {
  local n=$1 existing=$2 now=$3 who
  who=$(holder_desc "$existing" "${4:-}")
  if ! claim_liveness "$existing"; then
    echo "claim: #$n: cannot judge liveness of $who (unparsable lease or API error); not taking it" >&2; return 2
  fi
  case $(claim_status held "$LIVE" "$now") in
    active | grace)
      echo "claim: #$n is held by $who, alive until $(epoch_to_iso "$LIVE") (takeover after +${GRACE}s grace). Pick another issue." >&2; return 2 ;;
  esac
  if [[ $PR_STATE == ready ]]; then
    echo "claim: #$n has an open pull request #$PR_NUM that is ready for review; ask $(jq -r .human <<<"$existing") to release it" >&2; return 2
  fi
  return 0
}

# take_one N TAKEOVER HOLDER HUMAN CLAIMS NOW: 0 taken, 2 refused or lost the race (reason printed).
take_one() {
  local n=$1 takeover=$2 holder=$3 human=$4 claims=$5 now=$6
  local issue existing="" parent="" rc=0 lease_until pr prev_human=""
  issue=$(issue_json "$n") || { echo "claim: cannot read issue #$n" >&2; return 1; }
  if [[ $(jq -r .state <<<"$issue") != open ]]; then echo "claim: #$n is closed" >&2; return 2; fi
  if [[ $(jq -r .pull <<<"$issue") != false ]]; then echo "claim: #$n is a pull request" >&2; return 2; fi
  load_claim "$n" || rc=$?
  if ((rc == 0)); then
    existing=$C
    parent=$(jq -r .sha <<<"$existing")
    if [[ $(jq -r .state <<<"$existing") == held ]]; then
      if same_holder "$(jq -r .holder <<<"$existing")" "$holder"; then
        echo "#$n is already yours; extending the lease"
        beat_one "$n" ""; return
      fi
      dead_holder_check "$n" "$existing" "$now" "$human" || return 2
      if ((!takeover)); then echo "claim: #$n's holder is dead (since $(epoch_to_iso "$LIVE")); use: take $n --takeover" >&2; return 2; fi
      prev_human=$(jq -r .human <<<"$existing")
    fi
  elif ((takeover)); then
    echo "#$n has no claim; taking it normally"
  fi
  # ADR 0094 decision 5: an open pull request by another author, or another assignee, is someone's
  # work in flight (a takeover excuses those of the dead claim's own human).
  local conflicts others
  conflicts=$(pr_conflicts "$CLOSERS" "$n" "$human" "$prev_human" | paste -sd, - | sed 's/,/, /g')
  if [[ -n $conflicts ]]; then
    echo "claim: #$n has an open pull request by someone else: $conflicts. Pick another issue." >&2; return 2
  fi
  others=$(other_assignees "$issue" "$human" "$prev_human")
  if [[ -n $others ]]; then
    echo "claim: #$n is assigned to $others (an assignee is a person's claim). Pick another issue." >&2; return 2
  fi
  lease_until=$(epoch_to_iso $((now + $(lease_for_holder "$holder"))))
  pr=$(own_pr "$CLOSERS" "$n" "$human")
  if ! write_claim "$n" held "$holder" "$human" "$lease_until" "$pr" "" "$parent"; then
    echo "claim: lost the race for #$n: another session claimed it first. Pick another issue." >&2; return 2
  fi
  echo "claimed #$n as $holder until $lease_until"
  warn_area_overlap "$n" "$claims" "$now"
  jq -e '.labels | index("agent-ok")' <<<"$issue" >/dev/null || warn "#$n has no agent-ok label"
  [[ $(gh api "repos/$REPO/issues/$n/dependencies/blocked_by" --jq '[.[] | select(.state == "open")] | length' 2>/dev/null || echo 0) == 0 ]] || warn "#$n has open blockers"
  if [[ -n $prev_human && $prev_human != "$human" ]]; then unassign "$n" "$prev_human"; fi
  mirror_held "$n" "$human" "$holder" "$lease_until" "$pr" "" "In Progress"
  [[ -z $pr ]] || rerun_claim_check "$pr"
  return 0
}

cmd_take() {
  local takeover=0 issues=() a
  for a in "$@"; do
    case $a in
      --takeover) takeover=1 ;;
      [0-9]*) [[ $a =~ ^[0-9]+$ ]] || die "bad issue number $a"; issues+=("$a") ;;
      *) die "usage: take <N>... [--takeover] [--human] [--lane L]" ;;
    esac
  done
  ((${#issues[@]})) || die "usage: take <N>... [--takeover] [--human] [--lane L]"
  local human holder sess claims now counts taken=() n rc t held
  human=$(human_login); holder=$(my_holder); sess=$(holder_key "$holder")
  claims=$(list_claims); now=$(server_now)
  counts=$(active_counts "$claims" "$now" "$human" "$sess")
  if ((!HUMAN_MODE)) && ((${counts#* } >= SESSION_CAP)); then
    held=$(held_by_key "$claims" "$now" "$sess" | paste -sd, - | sed 's/,/, /g')
    CLAIM_EXIT=2 die "this session already holds ${counts#* } claim(s) in $(lane_label "$holder") (cap $SESSION_CAP): ${held:-none listed}. Finish or \`release\` one first, or take this issue in its own lane with --lane <name> (or KOINON_CLAIM_LANE); the human cap of $HUMAN_CAP still applies."
  fi
  if ((${counts% *} + ${#issues[@]} > HUMAN_CAP)); then
    CLAIM_EXIT=2 die "$human holds ${counts% *} claims; ${#issues[@]} more would pass the cap of $HUMAN_CAP."
  fi
  CLOSERS=$(open_pr_closers) || die "cannot list the open pull requests (API error); retry, nothing was claimed"
  for n in "${issues[@]}"; do
    rc=0
    take_one "$n" "$takeover" "$holder" "$human" "$claims" "$now" || rc=$?
    if ((rc == 0)); then taken+=("$n"); continue; fi
    # All or nothing: a pull request closing several issues needs all of them.
    if ((${#taken[@]})); then
      for t in "${taken[@]}"; do
        if load_claim "$t"; then
          write_claim "$t" released "$holder" "$human" "$(epoch_to_iso "$now")" "" "rolled back: #$n was not available" "$(jq -r .sha <<<"$C")" "$holder" || true
          echo "rolled back #$t" >&2
        fi
      done
    fi
    exit "$rc"
  done
}

mirror_held() { # N HUMAN HOLDER LEASE PR NOTE [STATUS]
  assign "$1" "$2"
  [[ -z ${7:-} ]] || set_status "$1" "$7" || true
  drop_label "$1" claim:stale
  upsert_comment "$1" "$(claim_comment held "$3" "$2" "$4" "$5" "$6")"
}

# require_mine N -> sets C, HOLDER, HUMAN, NOW; exit 3 unless the head claim is held by us, exit 1
# (not 3) when the claim could not be read.
require_mine() {
  local rc=0
  HOLDER=$(my_holder); HUMAN=$(human_login)
  load_claim "$1" || rc=$?
  if ((rc == 1)); then CLAIM_EXIT=3 die "no claim on #$1: stop and tell your human"; fi
  if [[ $(jq -r .state <<<"$C") != held ]] || ! same_holder "$(jq -r .holder <<<"$C")" "$HOLDER"; then
    CLAIM_EXIT=3 die "#$1 is not yours any more ($(jq -r '.state + " by " + .holder' <<<"$C")): stop work on it and tell your human"
  fi
  NOW=$(server_now)
}

# pr_is_open P -> exit 0 when pull request P is open.
pr_is_open() { [[ $(gh api "repos/$REPO/pulls/$1" --jq .state 2>/dev/null) == open ]]; }

beat_one() { # N PR_OVERRIDE
  local n=$1 pr=${2:-} lease_until note
  require_mine "$n"
  [[ -n $pr ]] || pr=$(jq -r .pr <<<"$C")
  if [[ -n $pr ]] && ! pr_is_open "$pr"; then pr=""; fi
  [[ -n $pr ]] || pr=$(find_pr "$n" "$HUMAN")
  lease_until=$(epoch_to_iso $((NOW + $(lease_for_holder "$HOLDER"))))
  note=$(jq -r .note <<<"$C")
  if ! write_claim "$n" held "$HOLDER" "$HUMAN" "$lease_until" "$pr" "$note" "$(jq -r .sha <<<"$C")"; then
    CLAIM_EXIT=3 die "lost #$n while renewing it: stop work and tell your human"
  fi
  echo "#$n lease extended to $lease_until"
  drop_label "$n" claim:stale
  upsert_comment "$n" "$(claim_comment held "$HOLDER" "$HUMAN" "$lease_until" "$pr" "$note")"
}

# parse_duration D -> seconds: 90, 90s, 30m or 2h (more than zero).
parse_duration() {
  [[ $1 =~ ^([0-9]+)([smh]?)$ ]] || return 1
  local v=$((10#${BASH_REMATCH[1]}))
  ((v > 0)) || return 1
  case ${BASH_REMATCH[2]} in m) echo $((v * 60)) ;; h) echo $((v * 3600)) ;; *) echo "$v" ;; esac
}

# mine_claims CLAIMS_JSON LOGIN SESSION -> "<issue><TAB><holder>" for every held claim of that
# session, lanes included (only this invocation's lane when one is set, like `mine`).
mine_claims() {
  jq -r --arg l "$2" --arg s "$3" --arg lane "$LANE" '
    .[] | select(.state == "held" and (.holder | split("/") | first) == $l
                 and (.holder | split("/") | last | split("#") | first) == $s
                 and ($lane == "" or ((.holder | split("/") | last | split("#") | .[1:] | first // "") == $lane)))
    | "\(.issue)\t\(.holder)"' <<<"$1"
}

join_semi() { local IFS=';'; local out="$*"; echo "${out//;/; }"; }
join_or_none() { if (($#)); then local IFS=' '; echo "$*"; else echo none; fi; }

# beat_identity: sets BEAT_LOGIN and BEAT_SESS once per run (three tries; a gh blip must not end a
# long loop later, and a start that cannot resolve them is an error, exit 1).
beat_identity() {
  local i
  for i in 1 2 3; do
    if BEAT_LOGIN=$(human_login) && BEAT_SESS=$(holder_session "$(my_base_holder)") && [[ -n $BEAT_LOGIN && -n $BEAT_SESS ]]; then return 0; fi
    ((i == 3)) || sleep 2
  done
  die "cannot resolve your login and session after 3 tries"
}

# beat_ended CLAIM_JSON LOGIN SESSION: how a tracked claim that is no longer ours ended (ADR 0094
# decision 7). Prints "drop" (our own session released it, in any lane, or its pull request closed: not
# a loss) or "lost <who ended it>". CLAIM_JSON "" is a claim that is gone. Only a held claim under
# another holder (a takeover) or a release by someone else is lost. A release commit without
# released_by (written before the field) keeps the old rule: only the pull request close note drops it.
beat_ended() {
  local c=$1 login=$2 sess=$3 state holder by note
  if [[ -z $c ]]; then echo "lost it is gone"; return; fi
  state=$(jq -r .state <<<"$c"); holder=$(jq -r .holder <<<"$c")
  by=$(jq -r '.released_by // ""' <<<"$c"); note=$(jq -r '.note // ""' <<<"$c")
  if [[ $state != released ]]; then echo "lost taken over by $holder"; return; fi
  if [[ -z $by ]]; then
    if [[ $note =~ ^pull\ request\ \#[0-9]+\ closed$ ]]; then echo drop; else echo "lost released by someone else (no released_by: $note)"; fi
    return
  fi
  if [[ $by == pr-close ]]; then echo drop; return; fi
  if [[ $by == */* && $(holder_login "$by") == "$login" && $(holder_session "$by") == "$sess" ]]; then echo drop; return; fi
  echo "lost released by $by"
}

# beat_round: renew every claim of this session once. Sets BEAT_OK, BEAT_LOST (exit 3: the claim is
# not ours any more; BEAT_WHY says who ended each), BEAT_ERR (exit 1 or anything else: retry next
# round; no gh failure of a round ends the loop) and BEAT_DROP (released by this session or because
# its pull request merged or closed). TRACKED holds the issues renewed or failed in earlier rounds, so
# a claim that left our hands is noticed instead of silently forgotten. The release commit records
# who released it (released_by), see beat_ended. This process makes no releases of its own.
TRACKED=()
beat_round() {
  local claims lines n holder rc t e v
  BEAT_OK=(); BEAT_LOST=(); BEAT_ERR=(); BEAT_DROP=(); BEAT_WHY=()
  if ! claims=$(list_claims); then BEAT_ERR+=("(listing the claims)"); return 0; fi
  if ! lines=$(mine_claims "$claims" "$BEAT_LOGIN" "$BEAT_SESS"); then BEAT_ERR+=("(reading the claims)"); return 0; fi
  local mine=()
  while IFS=$'\t' read -r n holder; do
    [[ -n $n ]] || continue
    mine+=("$n")
    rc=0
    # A subshell: beat_one dies with exit 3 (lost) or 1 (error), and each claim beats under its own lane.
    (LANE=$(holder_lane "$holder"); beat_one "$n" "" >/dev/null) || rc=$?
    case $rc in
      0) BEAT_OK+=("#$n") ;;
      3) BEAT_LOST+=("#$n") ;;
      *) BEAT_ERR+=("#$n") ;;
    esac
  done <<<"$lines"
  for t in ${TRACKED[@]+"${TRACKED[@]}"}; do
    [[ " ${mine[*]-} " != *" $t "* ]] || continue
    e=$(jq -c --argjson n "$t" '.[] | select(.issue == $n)' <<<"$claims")
    v=$(beat_ended "$e" "$BEAT_LOGIN" "$BEAT_SESS")
    if [[ $v == drop ]]; then BEAT_DROP+=("#$t"); else BEAT_LOST+=("#$t"); BEAT_WHY+=("#$t ${v#lost }"); fi
  done
  TRACKED=(${mine[@]+"${mine[@]}"})
}

# beat_mine EVERY FOR WHILE_PID. EVERY "" = one round. Exit 3 when a claim is lost (the loop stops and
# names it); exit 1 on errors in a single round; in a loop an error only repeats next round. A loop
# ends with exit 0 after FOR seconds, or when WHILE_PID is gone (so a background loop cannot outlive
# its session).
beat_clock() { date +%s; }
beat_mine() {
  local every=$1 for_s=$2 pid=$3 deadline
  TRACKED=()
  beat_identity
  deadline=$(($(beat_clock) + for_s))
  while :; do
    if [[ -n $pid ]] && ! kill -0 "$pid" 2>/dev/null; then echo "beat --mine: process $pid is gone"; return 0; fi
    beat_round
    echo "beat $(date -u +%Y-%m-%dT%H:%M:%SZ): renewed $(join_or_none ${BEAT_OK[@]+"${BEAT_OK[@]}"}); lost $(join_or_none ${BEAT_LOST[@]+"${BEAT_LOST[@]}"}); errors $(join_or_none ${BEAT_ERR[@]+"${BEAT_ERR[@]}"})$( ((${#BEAT_DROP[@]})) && echo "; released $(join_or_none "${BEAT_DROP[@]}")" || true)"
    if ((${#BEAT_LOST[@]})); then
      CLAIM_EXIT=3 die "lost ${BEAT_LOST[*]}: stop work on it and tell your human${BEAT_WHY[*]+ ($(join_semi "${BEAT_WHY[@]}"))}"
    fi
    if [[ -z $every ]]; then
      if ((${#BEAT_ERR[@]})); then exit 1; fi
      return 0
    fi
    sleep "$every"
    if (($(beat_clock) >= deadline)); then echo "beat --mine: --for elapsed"; return 0; fi
  done
}

cmd_beat() {
  local pr="" ns=() a own=0 every="" for_s=43200 for_given=0 pid=""
  while (($#)); do
    case $1 in
      --pr) pr=${2:-}; [[ $pr =~ ^[0-9]+$ ]] || die "--pr needs a pull request number"; shift 2 ;;
      --mine) own=1; shift ;;
      --every) every=$(parse_duration "${2:-}") || die "--every needs a duration: 90, 90s, 30m or 2h"; shift 2 ;;
      --for) for_given=1; for_s=$(parse_duration "${2:-}") || die "--for needs a duration: 90, 90s, 30m or 2h"; shift 2 ;;
      --while-pid) pid=${2:-}; [[ $pid =~ ^[0-9]+$ ]] || die "--while-pid needs a process id"; shift 2 ;;
      *) [[ $1 =~ ^[0-9]+$ ]] || die "usage: beat <N>... [--pr P] [--lane L] | beat --mine [--every D] [--for D] [--while-pid PID]"; ns+=("$1"); shift ;;
    esac
  done
  if ((own)); then
    ((${#ns[@]} == 0)) && [[ -z $pr ]] || die "beat --mine renews every claim you hold: no issue numbers and no --pr"
    beat_mine "$every" "$for_s" "$pid"
    return
  fi
  [[ -z $every && -z $pid && $for_given == 0 ]] || die "--every, --for and --while-pid need --mine"
  ((${#ns[@]})) || die "usage: beat <N>... [--pr P] [--lane L] | beat --mine [--every DURATION]"
  for a in "${ns[@]}"; do beat_one "$a" "$pr"; done
}

# cmd_adopt --from SESSION [N...]: re-key the claims of a dead session of this human to the current
# session. Each claim keeps its lane. The dead-holder rule is take --takeover's (dead_holder_check);
# the human and session caps count the claims as taken. The checks are all-or-nothing: no write
# happens unless every claim passes. The writes are not: if one fails (the holder woke up, another
# session adopted it first) the claims written before it stay adopted (each was echoed), the rest not.
cmd_adopt() {
  local from="" ns=() a
  while (($#)); do
    case $1 in
      --from) from=${2:-}; shift 2 ;;
      *) [[ $1 =~ ^[0-9]+$ ]] || die "usage: adopt --from <session> [N...]"; ns+=("$1"); shift ;;
    esac
  done
  [[ -n $from ]] || die "usage: adopt --from <session> [N...]"
  ((!HUMAN_MODE)) || die "adopt re-keys agent claims; --human claims are not adopted"
  from=${from%%#*}; from=${from//[^A-Za-z0-9._-]/-}
  local human base mysess claims now cand n c old lane newh key cnt iss
  human=$(human_login); base=$(my_base_holder); mysess=$(holder_session "$base")
  [[ $from != "$mysess" ]] || die "$from is this session: nothing to adopt"
  claims=$(list_claims); now=$(server_now)
  cand=$(jq -c --arg l "$human" --arg s "$from" '
    [.[] | select(.state == "held" and (.holder | split("/") | first) == $l and .human == $l
                  and (.holder | split("/") | last | split("#") | first) == $s)]' <<<"$claims")
  if ((${#ns[@]} == 0)); then
    while read -r n; do ns+=("$n"); done < <(jq -r '.[].issue' <<<"$cand")
    ((${#ns[@]})) || CLAIM_EXIT=2 die "session $from holds no claims of $human"
  else
    local uniq=()
    while read -r n; do uniq+=("$n"); done < <(printf '%s\n' "${ns[@]}" | awk '!seen[$0]++')
    ns=("${uniq[@]}")
  fi
  local counts hcount
  counts=$(active_counts "$claims" "$now" "$human" "$mysess")
  hcount=${counts% *}
  if ((hcount + ${#ns[@]} > HUMAN_CAP)); then
    CLAIM_EXIT=2 die "$human holds $hcount claims; adopting ${#ns[@]} more would pass the cap of $HUMAN_CAP."
  fi
  local plan_n=() plan_c=() plan_h=() planned_keys=()
  for n in "${ns[@]}"; do
    c=$(jq -c --argjson n "$n" '.[] | select(.issue == $n)' <<<"$cand")
    [[ -n $c ]] || CLAIM_EXIT=2 die "#$n is not a held claim of session $from"
    iss=$(issue_json "$n") || die "cannot read issue #$n (API error); retry"
    if [[ $(jq -r .state <<<"$iss") != open ]]; then CLAIM_EXIT=2 die "#$n is closed"; fi
    dead_holder_check "$n" "$c" "$now" || exit 2
    old=$(jq -r .holder <<<"$c"); lane=$(holder_lane "$old")
    newh="$base${lane:+#$lane}"; key=$(holder_key "$newh")
    cnt=$(active_counts "$claims" "$now" "$human" "$key"); cnt=${cnt#* }
    for a in ${planned_keys[@]+"${planned_keys[@]}"}; do if [[ $a == "$key" ]]; then cnt=$((cnt + 1)); fi; done
    if ((cnt >= SESSION_CAP)); then
      CLAIM_EXIT=2 die "adopting #$n would give this session $((cnt + 1)) claims in $(lane_label "$newh") (cap $SESSION_CAP)"
    fi
    planned_keys+=("$key"); plan_n+=("$n"); plan_c+=("$c"); plan_h+=("$newh")
  done
  local i lease_until pr
  for i in "${!plan_n[@]}"; do
    n=${plan_n[$i]}; c=${plan_c[$i]}
    lease_until=$(epoch_to_iso $((now + $(lease_for_holder "${plan_h[$i]}"))))
    pr=$(jq -r .pr <<<"$c"); [[ -n $pr ]] || pr=$(find_pr "$n" "$human")
    if ! write_claim "$n" held "${plan_h[$i]}" "$human" "$lease_until" "$pr" "adopted from $(jq -r .holder <<<"$c")" "$(jq -r .sha <<<"$c")"; then
      CLAIM_EXIT=2 die "#$n changed while adopting it (the holder woke up or another session adopted it first); re-read with show"
    fi
    echo "adopted #$n as ${plan_h[$i]} until $lease_until"
    mirror_held "$n" "$human" "${plan_h[$i]}" "$lease_until" "$pr" "adopted from $(jq -r .holder <<<"$c")" "In Progress"
  done
}

cmd_review() {
  local n=${1:-}
  [[ $n =~ ^[0-9]+$ ]] || die "usage: review <N> [--lane L]"
  require_mine "$n"
  local pr lease_until
  pr=$(jq -r .pr <<<"$C")
  if [[ -n $pr ]] && ! pr_is_open "$pr"; then pr=""; fi
  [[ -n $pr ]] || pr=$(find_pr "$n" "$HUMAN")
  [[ -n $pr ]] || warn "no open pull request closes #$n"
  lease_until=$(epoch_to_iso $((NOW + $(lease_for_holder "$HOLDER"))))
  write_claim "$n" held "$HOLDER" "$HUMAN" "$lease_until" "$pr" "in review" "$(jq -r .sha <<<"$C")" ||
    CLAIM_EXIT=3 die "lost #$n: stop work and tell your human"
  echo "#$n is in review (pull request ${pr:-none}); the claim is released when it merges or closes"
  set_status "$n" "In Review" || true
  upsert_comment "$n" "$(claim_comment held "$HOLDER" "$HUMAN" "$lease_until" "$pr" "in review")"
}

# do_release_claim CLAIM_JSON NOTE MERGED RELEASED_BY: write state released on top of the given claim's
# sha (so a claim that moved since the snapshot makes the write fail: returns 1, nothing released).
# RELEASED_BY is recorded in the release commit (ADR 0094 decision 7): the releasing holder, `sweeper`,
# `pr-close` or `comment:<login>`. The commit keeps the claim's holder.
do_release_claim() {
  local c=$1 note=$2 merged=${3:-0} by=${4:-} n human now
  n=$(jq -r .issue <<<"$c")
  if [[ $(jq -r .state <<<"$c") == released ]]; then echo "#$n is already released"; return 0; fi
  human=$(jq -r .human <<<"$c"); now=$(server_now)
  local wrc=0
  write_claim "$n" released "$(jq -r .holder <<<"$c")" "$human" "$(epoch_to_iso "$now")" "$(jq -r .pr <<<"$c")" "$note" "$(jq -r .sha <<<"$c")" "$by" || wrc=$?
  if ((wrc == 5)); then return 5; fi
  if ((wrc != 0)); then echo "claim: #$n changed since it was read; not released" >&2; return 1; fi
  echo "released #$n"
  drop_label "$n" claim:stale
  if ((!merged)) && [[ $(gh api "repos/$REPO/issues/$n" --jq .state 2>/dev/null) == open ]]; then
    unassign "$n" "$human"
    set_status "$n" Ready || true
  fi
  upsert_comment "$n" "$(claim_comment released "$(jq -r .holder <<<"$c")" "$human" "" "$(jq -r .pr <<<"$c")" "$note")"
  # A pull request whose `claim` check failed on this claim passes now (ADR 0094 decision 2).
  rerun_checks_for_issue "$n"
}

cmd_release() {
  local n=${1:-} note="" force=0
  [[ $n =~ ^[0-9]+$ ]] || die "usage: release <N> --note TEXT [--lane L]"
  shift
  while (($#)); do
    case $1 in
      --note) note=${2:-}; shift 2 ;;
      --force) force=1; shift ;;
      *) die "unknown option $1" ;;
    esac
  done
  [[ -n $note ]] || die "release needs --note \"<state, branch, next step>\""
  if ((force)); then
    load_claim "$n" || { CLAIM_EXIT=3 die "no claim on #$n"; }
  else
    require_mine "$n"
  fi
  do_release_claim "$C" "$note" 0 "$(my_holder)" || { CLAIM_EXIT=3 die "#$n changed while releasing it: re-read with show"; }
}

# wait-release N: poll until the claim is released or its PR is closed. An API error dies (load_claim),
# it is never read as "free". Exit 1 on timeout; exit 3 is not used (that is a lost claim).
cmd_wait_release() {
  local n=${1:-} timeout=7200 interval=${KOINON_CLAIM_WAIT_INTERVAL:-30} deadline rc pr
  [[ $n =~ ^[0-9]+$ ]] || die "usage: wait-release <N> [--timeout SECONDS]"
  shift
  while (($#)); do
    case $1 in
      --timeout) timeout=${2:-}; shift 2 ;;
      *) die "unknown option $1" ;;
    esac
  done
  [[ $timeout =~ ^[0-9]+$ ]] || die "--timeout needs a number of seconds"
  deadline=$(($(date +%s) + timeout))
  while :; do
    rc=0
    load_claim "$n" || rc=$?
    if ((rc == 1)); then echo "#$n: no claim"; return 0; fi
    if [[ $(jq -r .state <<<"$C") != held ]]; then echo "#$n: claim released"; return 0; fi
    pr=$(jq -r .pr <<<"$C")
    if [[ -n $pr && $(gh api "repos/$REPO/pulls/$pr" --jq .state 2>/dev/null) == closed ]]; then
      echo "#$n: PR #$pr merged or closed"; return 0
    fi
    if (($(date +%s) >= deadline)); then die "timed out after ${timeout}s: #$n is still claimed"; fi
    sleep "$interval"
  done
}

cmd_block() {
  local n=${1:-} m=${2:-} id
  [[ $n =~ ^[0-9]+$ && $m =~ ^[0-9]+$ ]] || die "usage: block <N> <M>"
  require_mine "$n"
  id=$(gh api "repos/$REPO/issues/$m" --jq .id) || die "cannot read #$m"
  gh_try -X POST "repos/$REPO/issues/$n/dependencies/blocked_by" -F "issue_id=$id"
  if ((TRY_RC != 0)) && [[ $TRY_OUT != *"already"* && $TRY_OUT != *"HTTP 422"* ]]; then die "cannot add the dependency: $TRY_OUT"; fi
  echo "#$n is now blocked by #$m"
  do_release_claim "$C" "blocked by #$m" 0 "$(my_holder)" || { CLAIM_EXIT=3 die "#$n changed while releasing it"; }
}

# ---- workflow commands ----

# sweep_one CLAIM NOW: stale-mark or release one claim. Never dies; a failure only skips the claim.
sweep_one() {
  local c=$1 now=$2 n holder human labels rc=0
  n=$(jq -r .issue <<<"$c"); holder=$(jq -r .holder <<<"$c"); human=$(jq -r .human <<<"$c")
  claim_liveness "$c" || rc=$?
  if ((rc == 1)); then warn "#$n: unparsable lease_until; skipping"; return 0; fi
  if ((rc != 0)); then warn "#$n: API error judging liveness; skipping"; return 0; fi
  case $PR_STATE in
    closed) echo "#$n: pull request #$PR_NUM is closed; releasing"
      do_release_claim "$c" "pull request #$PR_NUM closed" 1 pr-close || true; return 0 ;;
    ready) echo "#$n: pull request #$PR_NUM is in review; skipping"; return 0 ;;
  esac
  labels=$(gh api "repos/$REPO/issues/$n/labels" --jq '.[].name' 2>/dev/null || true)
  local cls
  cls=$(holder_class "$holder")
  if ((now > LIVE + RELEASE_AFTER)); then
    echo "#$n: no activity since $(epoch_to_iso "$LIVE"); releasing"
    do_release_claim "$c" "released by the sweeper: no heartbeat or pull request activity" 0 sweeper || true
  elif ((now > LIVE + $(stale_after "$holder"))); then
    if ! grep -qxF claim:stale <<<"$labels"; then
      echo "#$n: stale; marking"
      add_label "$n" claim:stale
      upsert_comment "$n" "$(claim_comment held "$holder" "$human" "$(epoch_to_iso "$LIVE")" "$PR_NUM" "" \
        "@$human the claim ran out at $(epoch_to_iso "$LIVE") with no heartbeat or pull request activity. $(renew_hint "$cls" "$n") The sweeper releases it at $(epoch_to_iso $((LIVE + RELEASE_AFTER))) and keeps the draft pull request.")"
    fi
  else
    if [[ $cls == outsider ]] && ((now > LIVE - OUTSIDER_WARN)); then
      remind_once "$n" "$(jq -r .sha <<<"$c")" "$(reminder_text "$human" "$n" "$(epoch_to_iso "$LIVE")" "$(epoch_to_iso $((LIVE + RELEASE_AFTER)))")"
    fi
    if grep -qxF claim:stale <<<"$labels"; then drop_label "$n" claim:stale; fi
  fi
  return 0
}

# reminder_text LOGIN N LIVE RELEASE_AT: the day-10 reminder of an outsider claim (ADR 0094 decision 3).
# shellcheck disable=SC2016
reminder_text() {
  printf '@%s your claim on #%s runs out at %s (14 days without a push or comment on your pull request). Comment `/claim` to keep it, or `/release` to hand it back. Otherwise the sweeper releases it at %s; your pull request stays open.\n' "$1" "$2" "$3" "$4"
}

# remind_once N SHA BODY: post BODY as a new comment (a mention in an edited comment notifies no one),
# once per claim commit: a renewal is a new commit and earns a new reminder.
remind_once() {
  local tag="$REMINDER_MARKER $2 -->" seen
  seen=$(gh api --paginate "repos/$REPO/issues/$1/comments" --jq ".[] | select(.body | startswith(\"$tag\")) | .id" 2>/dev/null | head -1) ||
    { warn "#$1: cannot read the comments; no reminder"; return 0; }
  [[ -z $seen ]] || return 0
  echo "#$1: outsider claim runs out within $((OUTSIDER_WARN / 86400)) days; reminding"
  gh api -X POST "repos/$REPO/issues/$1/comments" -f "body=$tag"$'\n'"$3" >/dev/null 2>&1 || write_failed "cannot comment on #$1"
  return 0
}

cmd_sweep() {
  local claims now n c failed=0
  claims=$(list_claims); now=$(server_now)
  for n in $(jq -r '.[] | select(.state == "held") | .issue' <<<"$claims"); do
    c=$(jq -c --argjson n "$n" '.[] | select(.issue == $n)' <<<"$claims")
    ( sweep_one "$c" "$now" ) || { warn "#$n: sweep failed"; failed=$((failed + 1)); }
  done
  # Keep sweeping the other claims, but end red: a missing permission must not look like a clean run.
  ((failed == 0)) || { echo "claim: $failed claim(s) failed to sweep" >&2; return 1; }
}

# release-pr P: release the held claims of the issues pull request P closes, but only claims that
# point at P (or that point at nothing while no other open pull request closes the issue).
cmd_release_pr() {
  local p=${1:-} body merged n rc others
  [[ $p =~ ^[0-9]+$ ]] || die "usage: release-pr <PR>"
  body=$(gh pr view "$p" -R "$REPO" --json body --jq '.body // ""')
  merged=0
  [[ -z $(gh pr view "$p" -R "$REPO" --json mergedAt --jq '.mergedAt // empty') ]] || merged=1
  for n in $(closing_issues "$body"); do
    rc=0
    load_claim "$n" || rc=$?
    ((rc == 0)) || continue
    [[ $(jq -r .state <<<"$C") == held ]] || continue
    if [[ $(jq -r .pr <<<"$C") == "$p" ]]; then :
    elif [[ -z $(jq -r .pr <<<"$C") ]]; then
      others=$(open_prs_closing "$n" | grep -vxF "$p" || true)
      [[ -z $others ]] || { echo "#$n: another open pull request closes it; keeping the claim"; continue; }
    else
      echo "#$n: claimed for pull request #$(jq -r .pr <<<"$C"), not #$p; keeping the claim"; continue
    fi
    do_release_claim "$C" "pull request #$p closed" "$merged" pr-close || true
  done
}

# pr_meta P -> {author, body} of pull request P (REST: the check runs on GITHUB_TOKEN).
# pr_meta P -> {author, body, closers}: closers are the issues of this repository GitHub links pull
# request P to as closed by it (closingIssuesReferences: a closing keyword in the body in any form,
# owner/repo#N and URLs included, and the sidebar's links), so the check sees what a merge closes.
pr_meta() {
  local out
  # shellcheck disable=SC2016
  out=$(gh api graphql -f o="$OWNER" -f n="$NAME" -F p="$1" -f query='
    query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){
      author{login} body closingIssuesReferences(first:100){nodes{number repository{nameWithOwner}}}}}}') || return 1
  jq -ce --arg repo "$REPO" '.data.repository.pullRequest | select(. != null)
    | {author: (.author.login // ""), body: (.body // ""),
       closers: [(.closingIssuesReferences.nodes // [])[] | select(.repository.nameWithOwner == $repo) | .number]}' <<<"$out"
}

# pr_added_adr_files P -> the docs/adr/NNNN-*.md files pull request P adds (added, renamed or copied),
# one per line. An API error fails: the check must not pass on a partial view.
pr_added_adr_files() {
  local files
  files=$(gh api --paginate "repos/$REPO/pulls/$1/files" \
    --jq '.[] | select(.status == "added" or .status == "renamed" or .status == "copied") | .filename') || return 1
  grep -E '^docs/adr/[0-9]{4}-[^/]*\.md$' <<<"$files" || true
}

# check-pr P: the required `claim` check is a conflict check (ADR 0094 decision 2). It fails only when
# someone else's human holds a live claim on an issue the pull request closes, or reserved
# (claim/adr-NNNN, lease not run out) an ADR number the pull request adds a file under. Free, released,
# dead and own claims pass, and so does a pull request that closes no issue. Read-only: opening a pull
# request creates no claim; `next` and `take` respect the open pull request itself.
cmd_check_pr() {
  local p=${1:-} pr author body n c fail=0 issues now rc human path num lease verdict
  [[ $p =~ ^[0-9]+$ ]] || die "usage: check-pr <PR>"
  pr=$(pr_meta "$p") || die "cannot read pull request #$p (API error); re-run the check"
  author=$(jq -r .author <<<"$pr"); body=$(jq -r .body <<<"$pr")
  now=$(server_now)
  # GitHub's links and the body's keywords: a body keyword alone misses the sidebar, owner/repo#N and URLs.
  issues=$({ jq -r '.closers[]?' <<<"$pr"; closing_issues "$body"; } | sort -un)
  if [[ -z $issues ]]; then
    echo "::notice::this pull request names no issue; say 'Closes #N' in its description so agents and this check see what it takes"
    echo "claim check: no Closes #N; nothing to check"
  fi
  for n in $issues; do
    rc=0
    load_claim "$n" || rc=$?
    if ((rc == 1)); then echo "claim check: #$n is free"; continue; fi
    c=$C; human=$(jq -r .human <<<"$c")
    if [[ $(jq -r .state <<<"$c") != held ]]; then echo "claim check: #$n is free (the claim was released)"; continue; fi
    if [[ $(lower "$human") == "$(lower "$author")" ]]; then echo "claim check: #$n is claimed by @$human (the author)"; continue; fi
    rc=0
    claim_liveness "$c" || rc=$?
    # The sweeper skips a claim with an unparsable lease, so it would block for ever: not a conflict.
    if ((rc == 1)); then echo "claim check: #$n is free (the claim of @$human has an unparsable lease)"; continue; fi
    ((rc == 0)) || die "cannot judge the liveness of the claim on #$n (API error); re-run the check"
    verdict=$(claim_verdict held "$human" "$author" "$now" "$LIVE" "$PR_STATE")
    case $verdict in
      stale) echo "claim check: #$n is free (the claim of @$human went stale at $(epoch_to_iso "$LIVE"))" ;;
      *)
        conflict_issue_text "$n" "$human" "$(epoch_to_iso "$LIVE")" "$PR_NUM" "$author" "$(epoch_to_iso $((LIVE + RELEASE_AFTER)))"
        fail=1 ;;
    esac
  done
  local added
  added=$(pr_added_adr_files "$p") || die "cannot list the files of pull request #$p (API error); re-run the check"
  while IFS= read -r path; do
    [[ -n $path ]] || continue
    num=${path:9:4}
    rc=0
    load_claim "adr-$num" || rc=$?
    if ((rc == 1)); then echo "claim check: $path: ADR $num is not reserved"; continue; fi
    c=$C; human=$(jq -r .human <<<"$c"); lease=$(jq -r .lease_until <<<"$c")
    if [[ $(jq -r .state <<<"$c") != held ]]; then echo "claim check: $path: the reservation of ADR $num was released"; continue; fi
    if [[ $(lower "$human") == "$(lower "$author")" ]]; then echo "claim check: $path: ADR $num is reserved by @$human (the author)"; continue; fi
    if ! parse_lease "$lease" >/dev/null || (($(parse_lease "$lease") < now)); then
      echo "claim check: $path: the reservation of ADR $num by @$human ran out at $lease"; continue
    fi
    conflict_adr_text "$path" "$num" "$human" "$lease"
    fail=1
  done <<<"$added"
  return "$fail"
}

# ---- claims by comment (ADR 0094 decision 1; claim-comment.yml) ----

# commenter_class LOGIN -> person (write, maintain or admin on the repository) or outsider. A login
# GitHub does not know as a collaborator (404) is an outsider; any other API error fails.
commenter_class() {
  gh_try "repos/$REPO/collaborators/$1/permission" --jq .permission
  if ((TRY_RC == 0)); then perm_class "$TRY_OUT"; return 0; fi
  if [[ $TRY_OUT == *"HTTP 404"* ]]; then echo outsider; return 0; fi
  echo "claim: cannot read the permission of $1: $TRY_OUT" >&2
  return 1
}

# reply N BODY: answer on the issue. Returns 5 when GitHub refuses the write (the fallback's trigger).
reply() {
  gh_try -X POST "repos/$REPO/issues/$1/comments" -f "body=$2"
  if ((TRY_RC == 0)); then echo "answered on #$1: ${2%%$'\n'*}"; return 0; fi
  if is_denied "$TRY_OUT"; then return 5; fi
  warn "cannot answer on #$1: $TRY_OUT"
  return 0
}

# assign_checked N LOGIN -> prints the marker note when GitHub left LOGIN out of the assignees it
# returned (Prow's rule: compare the answer with the request); the claim never depends on it.
assign_checked() {
  gh_try -X POST "repos/$REPO/issues/$1/assignees" -f "assignees[]=$2" --jq '[.assignees[]?.login | ascii_downcase]'
  if ((TRY_RC == 0)) && jq -e --arg l "$(lower "$2")" 'index($l)' <<<"$TRY_OUT" >/dev/null 2>&1; then return 0; fi
  text_unassignable "$2" "$1"
}

# write_refused_fallback N LOGIN: GitHub refused a write of the comment command (a non-collaborator's
# run may get a read-only token; probe 3 of ADR 0094 was skipped). Point the commenter to a pull
# request, open one Backlog issue for the maintainers (deduplicated by its fixed title), never fail.
write_refused_fallback() {
  local n=$1 login=$2 open
  echo "::warning::GitHub refused a claim write for @$login on #$n; answering with the fallback"
  gh api -X POST "repos/$REPO/issues/$n/comments" -f "body=$(text_fallback "$login" "$n")" >/dev/null 2>&1 ||
    warn "cannot answer on #$n either"
  open=$(gh api --paginate "repos/$REPO/issues?state=open&per_page=100" --jq '.[] | select(.pull_request == null) | [.number, .title] | @tsv' 2>/dev/null |
    awk -F'\t' -v t="$FALLBACK_TITLE" '$2 == t {print $1; exit}') || open=""
  if [[ -n $open ]]; then
    echo "the maintainers' issue #$open already tracks this"
  else
    gh api -X POST "repos/$REPO/issues" -f "title=$FALLBACK_TITLE" -f "body=A \`/claim\` or \`/release\` comment by an outside contributor (first seen: @$login on #$n) could not write: GitHub refused the claim-comment workflow's token (401/403). Comment claims stay unavailable for outside contributors until the workflow gets a write token for their comments; they are told to open a pull request that says Closes #N, which \`claim.sh next\` and \`take\` already respect. See ADR 0094 decision 1 (fallback) and the run log of claim-comment.yml." \
      >/dev/null 2>&1 || warn "cannot open the maintainers' issue"
  fi
  return 0
}

# comment_claim N LOGIN CLASS: a /claim comment. Returns 5 when GitHub refused a write.
comment_claim() {
  local n=$1 login=$2 class=$3 holder issue now claims rc=0 existing="" parent="" renew=0 lease_until pr wrc=0 held cap note
  holder=$(comment_holder "$login" "$class")
  issue=$(issue_json "$n") || die "cannot read issue #$n (API error)"
  if [[ $(jq -r .pull <<<"$issue") != false ]]; then echo "#$n is a pull request; ignoring"; return 0; fi
  if [[ $(jq -r .state <<<"$issue") != open ]]; then reply "$n" "$(text_closed "$login" "$n")"; return; fi
  if [[ $class == outsider ]] && ! jq -e --argjson ok "$OUTSIDER_LABELS" '.labels | any(IN($ok[]))' <<<"$issue" >/dev/null; then
    reply "$n" "$(text_label "$login" "$n")"; return
  fi
  now=$(server_now); claims=$(list_claims)
  load_claim "$n" || rc=$?
  if ((rc == 0)); then
    existing=$C; parent=$(jq -r .sha <<<"$existing")
    if [[ $(jq -r .state <<<"$existing") == held ]]; then
      if same_holder "$(jq -r .holder <<<"$existing")" "$holder"; then
        renew=1
      else
        LIVE=$(parse_lease "$(jq -r .lease_until <<<"$existing")" 2>/dev/null) || LIVE=$now
        if ! dead_holder_check "$n" "$existing" "$now" >/dev/null 2>&1; then
          reply "$n" "$(text_held "$login" "$n" "$(jq -r .human <<<"$existing")" "$(epoch_to_iso "$LIVE")")"; return
        fi
        echo "#$n: the claim of @$(jq -r .human <<<"$existing") is dead since $(epoch_to_iso "$LIVE"); taking it over"
      fi
    fi
  fi
  if ((!renew)); then
    if [[ $class == outsider ]]; then cap=$OUTSIDER_CAP; held=$(live_claims_of "$claims" "$now" "$login" outsider)
    else cap=$HUMAN_CAP; held=$(live_claims_of "$claims" "$now" "$login"); fi
    if (($(grep -c . <<<"$held" || true) >= cap)); then
      reply "$n" "$(text_cap "$login" "$(grep -c . <<<"$held")" "$cap" "$(paste -sd, - <<<"$held" | sed 's/,/, /g')")"; return
    fi
  fi
  lease_until=$(epoch_to_iso $((now + $(lease_for_holder "$holder"))))
  pr=$(find_pr "$n" "$login")
  write_claim "$n" held "$holder" "$login" "$lease_until" "$pr" "claimed by comment" "$parent" || wrc=$?
  if ((wrc == 5)); then return 5; fi
  if ((wrc != 0)); then reply "$n" "$(text_race "$login" "$n")"; return; fi
  if ((renew)); then echo "renewed #$n for $holder until $lease_until"; else echo "claimed #$n as $holder until $lease_until"; fi
  if [[ -n $existing && $(jq -r .state <<<"$existing") == held && $(lower "$(jq -r .human <<<"$existing")") != "$(lower "$login")" ]]; then
    unassign "$n" "$(jq -r .human <<<"$existing")"
  fi
  note=$(assign_checked "$n" "$login")
  drop_label "$n" claim:stale
  upsert_comment "$n" "$(claim_comment held "$holder" "$login" "$lease_until" "$pr" "claimed by comment" "$note")"
  rerun_checks_for_issue "$n"
  return 0
}

# comment_release N LOGIN CLASS: a /release comment, by the claim's human or any person.
comment_release() {
  local n=$1 login=$2 class=$3 rc=0 human by wrc=0
  load_claim "$n" || rc=$?
  if ((rc == 1)) || [[ $(jq -r .state <<<"$C") != held ]]; then reply "$n" "$(text_no_claim "$login" "$n")"; return; fi
  human=$(jq -r .human <<<"$C")
  if [[ $(lower "$login") != "$(lower "$human")" && $class != person ]]; then
    reply "$n" "$(text_release_refused "$login" "$human" "$n")"; return
  fi
  # released_by (ADR 0094 decision 7): the holder when the holder itself comments, else comment:<login>.
  if same_holder "$(jq -r .holder <<<"$C")" "$(comment_holder "$login" "$class")"; then by=$(jq -r .holder <<<"$C"); else by="comment:$login"; fi
  do_release_claim "$C" "released by a /release comment from @$login" 0 "$by" || wrc=$?
  if ((wrc == 5)); then return 5; fi
  if ((wrc != 0)); then reply "$n" "$(text_race "$login" "$n")"; return; fi
  return 0
}

# comment N: the entry point of claim-comment.yml. The comment's body and author come in through
# KOINON_CLAIM_COMMENT_BODY and KOINON_CLAIM_COMMENT_LOGIN (never through the command line).
cmd_comment() {
  local n=${1:-} body=${KOINON_CLAIM_COMMENT_BODY:-} login=${KOINON_CLAIM_COMMENT_LOGIN:-} cmd class rc=0
  [[ $n =~ ^[0-9]+$ && -n $login ]] || die "usage: KOINON_CLAIM_COMMENT_BODY=... KOINON_CLAIM_COMMENT_LOGIN=... comment <N>"
  cmd=$(comment_command "$body")
  if [[ -z $cmd ]]; then echo "#$n: the comment is not /claim or /release; nothing to do"; return 0; fi
  if [[ $login == *"[bot]" ]]; then echo "#$n: ignoring a bot's comment"; return 0; fi
  class=$(commenter_class "$login") || die "cannot tell whether @$login is a maintainer; nothing changed"
  echo "#$n: /$cmd by @$login ($class)"
  CLAIM_SOFT_DENY=1
  case $cmd in
    claim) comment_claim "$n" "$login" "$class" || rc=$? ;;
    release) comment_release "$n" "$login" "$class" || rc=$? ;;
  esac
  if ((rc == 5)); then write_refused_fallback "$n" "$login"; return 0; fi
  return "$rc"
}

# ADR_PR_JQ: the ADR numbers that open pull requests ADD (added, renamed or copied files), from the pages of open_pr_adr_numbers (slurped).
# shellcheck disable=SC2016
ADR_PR_JQ='[.[].data.repository.pullRequests.nodes[] | .files.nodes[]
  | select(.changeType == "ADDED" or .changeType == "RENAMED" or .changeType == "COPIED") | .path | capture("^docs/adr/(?<n>[0-9]{4})-[^/]*\\.md$").n] | unique | .[]'

# open_pr_adr_numbers -> the four-digit ADR numbers that open pull requests add (ADR 0094 decision 6),
# one per line. An API error dies: a number handed out from a partial view is how 0093 was duplicated.
# A pull request with more than 100 files is read file by file through the REST list.
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

# adr: reserve the next free ADR number. claim/adr-NNNN is created like a claim (create fails when it
# exists), so two sessions cannot take one number. A number is free when no docs/adr file on the
# default branch, no claim/adr-* ref and no file an open pull request adds uses it (ADR 0094
# decision 6).
cmd_adr() {
  local files refs prs nums n holder human lease_until tries=0
  holder=$(my_holder); human=$(human_login)
  while ((tries < 5)); do
    tries=$((tries + 1))
    files=$(gh api "repos/$REPO/contents/docs/adr" --jq '.[].name' | grep -oE '^[0-9]{4}' || true)
    refs=$(gh api "repos/$REPO/git/matching-refs/heads/claim/adr-" --jq '.[].ref' | grep -oE '[0-9]{4}$' || true)
    prs=$(open_pr_adr_numbers)
    # shellcheck disable=SC2086
    nums=$(next_adr_number $files $refs $prs)
    n=$nums
    lease_until=$(epoch_to_iso $(($(server_now) + HUMAN_LEASE)))
    if write_adr_claim "$n" "$holder" "$human" "$lease_until"; then
      echo "$n"
      echo "reserved ADR $n (ref claim/adr-$n); name your file docs/adr/$n-<title>.md" >&2
      return 0
    fi
  done
  die "could not reserve an ADR number"
}

write_adr_claim() { # NUMBER HOLDER HUMAN LEASE
  local msg sha
  msg=$(claim_format "adr-$1" held "$2" "$3" "$4" "" "ADR number reservation")
  sha=$(gh api -X POST "repos/$REPO/git/commits" -f "message=$msg" -f "tree=$EMPTY_TREE" --jq .sha) || die "cannot create the commit"
  gh_try -X POST "repos/$REPO/git/refs" -f "ref=refs/heads/claim/adr-$1" -f "sha=$sha"
  if ((TRY_RC == 0)); then return 0; fi
  if [[ $TRY_OUT == *"HTTP 422"* ]] && is_lost_race "$TRY_OUT"; then return 1; fi
  die "reservation failed: $TRY_OUT"
}

# open_prs prints the open pull requests in the shape of `gh pr list --json` (number, createdAt,
# isDraft, headRefOid, labels, autoMergeRequest, mergeStateStatus, statusCheckRollup). It asks
# GraphQL for the check fields it uses only: `gh pr list --json statusCheckRollup` also reads each
# check's workflow run, which an App token without Actions read is refused.
open_prs() {
  # shellcheck disable=SC2016
  gh api graphql -f o="$OWNER" -f n="$NAME" -f query='
query($o: String!, $n: String!) {
  repository(owner: $o, name: $n) {
    pullRequests(states: OPEN, first: 100, orderBy: {field: CREATED_AT, direction: ASC}) {
      nodes {
        number createdAt isDraft headRefOid mergeStateStatus isInMergeQueue
        autoMergeRequest { enabledAt }
        labels(first: 50) { nodes { name } }
        commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100) { nodes {
          __typename
          ... on CheckRun { name status conclusion startedAt }
          ... on StatusContext { context state createdAt }
        } } } } } }
      }
    }
  }
}' --jq '[.data.repository.pullRequests.nodes[] | {number, createdAt, isDraft, headRefOid, mergeStateStatus, isInMergeQueue,
    autoMergeRequest, labels: .labels.nodes,
    statusCheckRollup: (.commits.nodes[0].commit.statusCheckRollup.contexts.nodes // [])}]'
}

# required_contexts [FILE]: the required check names of hack/ci-required.txt as a JSON array (the
# repository's source of truth; `advisory` lines are not required). The updater runs on a checkout
# of the default branch, so the file is there.
required_contexts() {
  local f=${1:-${KOINON_CLAIM_REQUIRED:-$(cd "$(dirname "$0")/.." && pwd)/hack/ci-required.txt}}
  [[ -r $f ]] || die "cannot read $f"
  sed -e '/^[[:space:]]*#/d' -e '/^[[:space:]]*$/d' -e '/^advisory /d' -e 's/^aggregator //' -e 's/^external //' "$f" | jq -R . | jq -s .
}

# JQ_LATEST: jq definitions shared by the updater's picks. A rollup keeps every run of a check name
# on the head (a draft run, a cancelled run, a rerun), so only the newest run per name counts, and
# only the required names: an advisory check, a third-party check or the updater's own run says
# nothing about whether the pull request can merge. A queued run without a start time is the newest.
# shellcheck disable=SC2016
JQ_LATEST='
def nm: .name // .context;
def ts: if (.__typename == "CheckRun") and (.startedAt == null) and (.status != "COMPLETED") then "9999" else (.startedAt // .createdAt // "") end;
def latest($req): [.statusCheckRollup[]? | select(nm | IN($req[]))] | group_by(nm) | map(max_by(ts));
def green: if .__typename == "StatusContext" then .state == "SUCCESS"
  else .status == "COMPLETED" and (.conclusion | IN("SUCCESS", "SKIPPED", "NEUTRAL")) end;
'

# update_target REQUIRED_JSON: stdin is open_prs output; prints the number of the pull request to
# bring up to date, or nothing. The oldest auto-merge pull request that is behind main, not a draft,
# and green on its own head: every required check has reported and its newest run succeeded.
# Updating any other one only starts CI runs that are thrown away (koinon-private#596): a draft is not asking to
# merge, a red or running head changes anyway, and a conflicting one (DIRTY) is labelled needs-update.
update_target() {
  jq -r --argjson req "${1:?required contexts}" "$JQ_LATEST"'
    [.[] | select(.autoMergeRequest != null and .mergeStateStatus == "BEHIND" and (.isDraft | not) and ((.isInMergeQueue // false) | not)
      and ($req | length > 0)
      and (latest($req) as $l | all($req[]; . as $r | any($l[]; nm == $r)) and all($l[]; green)))]
    | sort_by(.createdAt) | first.number // empty'
}

cmd_update_oldest() {
  local prs inflight target head tries=0 now req
  # With a merge queue on main (#225, ADR 0094 decision 4) the queue builds main plus the entry, so
  # nothing here is needed, and a push to a queued PR's branch would drop it from the queue. Read at run time;
  # an unreadable answer means "no queue" (the pre-queue behaviour). Exit condition for removing this command
  # and updater.yml: #227 (also linked from docs/dev/ci.md "The merge queue").
  if [[ $(gh api "repos/$REPO/rules/branches/${KOINON_BASE_BRANCH:-main}" --jq 'any(.[]; .type == "merge_queue")' 2>/dev/null || true) == true ]]; then
    echo "::notice::main has a merge queue; the updater has nothing to do"; return 0
  fi
  now=$(server_now)
  req=$(required_contexts)
  while :; do
    prs=$(open_prs)
    if ((tries < 3)) && jq -e '[.[] | select(.autoMergeRequest != null and .mergeStateStatus == "UNKNOWN")] | length > 0' <<<"$prs" >/dev/null; then
      tries=$((tries + 1)); sleep 5; continue
    fi
    break
  done
  # In flight: an up-to-date auto-merge PR whose required checks are still running, started within the last hour
  # (a stuck check does not block the others for ever).
  inflight=$(jq -r --argjson now "$now" --argjson req "$req" "$JQ_LATEST"'
    [.[] | select(.autoMergeRequest != null and .mergeStateStatus != "BEHIND" and ((.isInMergeQueue // false) | not)
    and ([latest($req)[] | select(((.status // "COMPLETED") != "COMPLETED" or .state == "PENDING")
         and (((.startedAt // "1970-01-01T00:00:00Z") | fromdateiso8601) > ($now - 3600)))] | length > 0)) | .number] | first // empty' <<<"$prs")
  if [[ -n $inflight ]]; then echo "::notice::#$inflight is up to date and its checks are running; not updating another branch"; return 0; fi
  # A conflicting auto-merge pull request is labelled for its author and skipped; the bot never
  # pushes code to a pull request branch. The label goes when the conflict does.
  local n
  for n in $(jq -r '.[] | select(.autoMergeRequest != null and .mergeStateStatus == "DIRTY" and ((.isInMergeQueue // false) | not)) | .number' <<<"$prs"); do
    if ! jq -e --argjson n "$n" '.[] | select(.number == $n) | .labels[]? | select(.name == "needs-update")' <<<"$prs" >/dev/null; then
      echo "#$n conflicts with main; labelling needs-update"
      add_label "$n" needs-update
    fi
  done
  for n in $(jq -r '.[] | select(.mergeStateStatus != "DIRTY" and ([.labels[]? | select(.name == "needs-update")] | length > 0)) | .number' <<<"$prs"); do
    drop_label "$n" needs-update
  done
  target=$(update_target "$req" <<<"$prs")
  if [[ -z $target ]]; then echo "no ready, green auto-merge pull request is behind main"; return 0; fi
  head=$(jq -r --argjson n "$target" '.[] | select(.number == $n) | .headRefOid' <<<"$prs")
  gh_try -X PUT "repos/$REPO/pulls/$target/update-branch" -f "expected_head_sha=$head"
  if ((TRY_RC == 0)); then echo "updated #$target with main"
  elif [[ $TRY_OUT == *"onflict"* ]]; then echo "#$target conflicts with main; labelling needs-update"; add_label "$target" needs-update
  else die "update-branch failed for #$target: $TRY_OUT"; fi
}

usage() { sed -n '4,/^# Needs bash 5/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; }

main() {
  command -v gh >/dev/null || die "gh is required"
  command -v jq >/dev/null || die "jq is required"
  local cmd=${1:-}
  [[ -n $cmd ]] || { usage; exit 1; }
  shift || true
  case $cmd in
    -h | --help | help) usage; exit 0 ;;
  esac
  split_globals "$@" || exit 1
  if ((${#REST[@]})); then set -- "${REST[@]}"; else set --; fi
  init_repo
  case $cmd in
    next) cmd_next "$@" ;;
    take) cmd_take "$@" ;;
    beat) cmd_beat "$@" ;;
    adopt) cmd_adopt "$@" ;;
    review) cmd_review "$@" ;;
    release) cmd_release "$@" ;;
    wait-release) cmd_wait_release "$@" ;;
    block) cmd_block "$@" ;;
    show) cmd_show "${1:?usage: show <N>}" ;;
    mine) cmd_mine ;;
    limits) cmd_limits ;;
    sweep) cmd_sweep ;;
    check-pr) cmd_check_pr "$@" ;;
    release-pr) cmd_release_pr "$@" ;;
    comment) cmd_comment "$@" ;;
    update-oldest) cmd_update_oldest ;;
    adr) cmd_adr ;;
    sensitive-paths) sensitive_paths "${KOINON_CLAIM_CODEOWNERS:-$(cd "$(dirname "$0")/.." && pwd)/.github/CODEOWNERS}" ;;
    *) usage; exit 1 ;;
  esac
}

if [[ -z ${CLAIM_LIB_ONLY:-} ]]; then main "$@"; fi
