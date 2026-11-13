#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The org board as one cached snapshot (#654). `gh project item-list --limit 1000` stops at 1000 items,
# so open issues past that fell off the list, and every triage pass paid for a full GraphQL pull.
#
#   hack/board.sh snapshot            the whole snapshot as JSON (an array, one object per open issue)
#   hack/board.sh backlog             open issues in Backlog or not on the board at all (they fell off), one line each
#   hack/board.sh ready               open issues in Ready, one line each
#   hack/board.sh issue <N>           one open issue's record as JSON
#   --refresh (before the command)    ignore the cache and fetch again
#
# One fetch: the board's items through GraphQL, 100 per page with a cursor (no 1000 cap), joined with
# the REST list of open issues (pull requests removed). Only items of $KOINON_BOARD_REPO count (an org
# board can hold other repositories' issues). An issue not on the board has status "none"; one on the board
# without a Status value has "no-status".
# Record: {number,title,status,labels[],url}. Lines: "#<N> <status> [label,label] <title>".
# Cache: $KOINON_STATE_DIR/board.json (default ${XDG_CONFIG_HOME:-~/.config}/koinon/state, the dir of
# hack/agent-state.sh), fresh for $KOINON_BOARD_TTL seconds (default 600). A failed fetch keeps the old
# cache and exits 1; a stale cache is never served silently.
# Board: $KOINON_BOARD_ORG (stateracloud), $KOINON_BOARD_NUMBER (1), repository $KOINON_BOARD_REPO
# (stateracloud/koinon). Test hook: KOINON_BOARD_GH replaces gh (hack/board-test.sh).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail

usage() { sed -n '4,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2; exit 2; }

refresh=0
[[ ${1:-} == --refresh ]] && { refresh=1; shift; }
cmd=${1:-}
[[ -n $cmd ]] || usage
shift
case $cmd in
snapshot | backlog | ready) [[ $# -eq 0 ]] || usage ;;
issue) [[ $# -eq 1 && $1 =~ ^[0-9]+$ ]] || usage ;;
-h | --help) usage ;;
*) usage ;;
esac

gh_cmd=${KOINON_BOARD_GH:-gh}
org=${KOINON_BOARD_ORG:-stateracloud}
number=${KOINON_BOARD_NUMBER:-1}
repo=${KOINON_BOARD_REPO:-stateracloud/koinon}
ttl=${KOINON_BOARD_TTL:-600}
[[ $ttl =~ ^[0-9]+$ ]] || { echo "board: KOINON_BOARD_TTL must be a number of seconds" >&2; exit 2; }
state_dir=${KOINON_STATE_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/koinon/state}
cache=$state_dir/board.json

# The mtime in seconds since the epoch (GNU and BSD stat).
mtime() { stat -c %Y "$1" 2>/dev/null || stat -f %m "$1"; }

fresh() { # the cache exists, parses, and is younger than the TTL
	[[ -s $cache ]] || return 1
	jq -e 'type == "array"' "$cache" >/dev/null 2>&1 || return 1
	local age=$(($(date +%s) - $(mtime "$cache")))
	[[ $age -ge 0 && $age -lt $ttl ]]
}

# shellcheck disable=SC2016 # the GraphQL variables are not shell variables
query='query($org: String!, $number: Int!, $endCursor: String) {
  organization(login: $org) { projectV2(number: $number) {
    items(first: 100, after: $endCursor) {
      pageInfo { hasNextPage endCursor }
      nodes {
        status: fieldValueByName(name: "Status") { ... on ProjectV2ItemFieldSingleSelectValue { name } }
        content { ... on Issue { number state repository { nameWithOwner } } }
      }
    }
  } }
}'

fetch() {
	local tmp
	mkdir -p "$state_dir" || return 1
	tmp=$(mktemp -d "$state_dir/board.XXXXXX") || return 1
	# shellcheck disable=SC2064
	trap "rm -rf '$tmp'" RETURN
	"$gh_cmd" api graphql --paginate -f query="$query" -f org="$org" -F number="$number" >"$tmp/items.raw" || { echo "board: the board query failed" >&2; return 1; }
	"$gh_cmd" api -X GET "repos/$repo/issues" -f state=open -F per_page=100 --paginate >"$tmp/issues.raw" || { echo "board: the issue list failed" >&2; return 1; }
	jq -s --arg repo "$repo" '[.[] | .data.organization.projectV2.items.nodes[]? | select(.content.number != null and .content.repository.nameWithOwner == $repo) | {key: (.content.number | tostring), value: (.status.name // "no-status")}] | from_entries' "$tmp/items.raw" >"$tmp/items.json" || { echo "board: unreadable board page" >&2; return 1; }
	jq -s --slurpfile st "$tmp/items.json" '[.[][] | select(has("pull_request") | not) | {number, title, status: ($st[0][(.number | tostring)] // "none"), labels: [.labels[].name], url: .html_url}] | sort_by(.number)' "$tmp/issues.raw" >"$tmp/board.json" || { echo "board: unreadable issue page" >&2; return 1; }
	mv "$tmp/board.json" "$cache"
}

if [[ $refresh == 1 ]] || ! fresh; then
	fetch || exit 1
fi

line='"#\(.number) \(.status) [\(.labels | join(","))] \(.title)"'
case $cmd in
snapshot) jq . "$cache" ;;
backlog) jq -r ".[] | select(.status == \"Backlog\" or .status == \"none\" or .status == \"no-status\") | $line" "$cache" ;;
ready) jq -r ".[] | select(.status == \"Ready\") | $line" "$cache" ;;
issue)
	jq -e --argjson n "$1" '.[] | select(.number == $n)' "$cache" || { echo "board: #$1 is not an open issue" >&2; exit 1; }
	;;
esac
