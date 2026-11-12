#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline test of hack/board.sh (#654) with a fake gh: two GraphQL pages and two REST pages from
# hack/testdata/board. Checks the join (an issue missing from the board is "none", a pull request is
# dropped), the cache (TTL, --refresh, a failed fetch keeps the old cache) and the commands.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -uo pipefail
cd "$(dirname "$0")/.." || exit 2
script=$PWD/hack/board.sh
fixtures=$PWD/hack/testdata/board
tmp=$(mktemp -d "${TMPDIR:-/tmp}/board-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/gh" <<'EOS'
#!/usr/bin/env bash
echo "$1 $2 $3" >>"$FAKE_GH_LOG"
[ -z "${FAKE_GH_FAIL:-}" ] || { echo "gh: boom" >&2; exit 1; }
case $* in
"api graphql"*) cat "$FIXTURES/graphql-page1.json" "$FIXTURES/graphql-page2.json" ;;
"api -X GET repos/"*) cat "$FIXTURES/issues-page1.json" "$FIXTURES/issues-page2.json" ;;
*) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac
EOS
chmod +x "$tmp/gh"
export KOINON_BOARD_GH=$tmp/gh FAKE_GH_LOG=$tmp/gh.log FIXTURES=$fixtures KOINON_STATE_DIR=$tmp/state
: >"$FAKE_GH_LOG"

fails=0 total=0
ok() { total=$((total + 1)); }
bad() { total=$((total + 1)); echo "FAIL: $1"; fails=$((fails + 1)); }
eq() { if [[ $2 == "$3" ]]; then ok; else bad "$1: got '$2', want '$3'"; fi; }
calls() { wc -l <"$FAKE_GH_LOG" | tr -d ' '; }

out=$("$script" backlog 2>&1)
eq "backlog lists Backlog and off-board issues" "$out" $'#11 Backlog [] Backlog one\n#14 none [] Off the board'
eq "one fetch is two gh calls (board, issues)" "$(calls)" 2
out=$("$script" ready)
eq "ready lists Ready issues with labels" "$out" $'#10 Ready [agent-ok] Ready one\n#12 Ready [human-only,bug] Ready two'
eq "the second command is served from the cache" "$(calls)" 2
eq "the snapshot has no pull request and no closed item" "$("$script" snapshot | jq -c '[.[].number]')" "[10,11,12,14]"
eq "issue N is one record" "$("$script" issue 12 | jq -r '.status + " " + .url')" "Ready https://example.invalid/i/12"
"$script" issue 13 >/dev/null 2>&1; eq "a closed or unknown issue is exit 1" "$?" 1
"$script" issue x >/dev/null 2>&1; eq "a bad number is a usage error" "$?" 2
"$script" nonsense >/dev/null 2>&1; eq "an unknown command is a usage error" "$?" 2

"$script" --refresh ready >/dev/null
eq "--refresh fetches again" "$(calls)" 4
touch -d '20 minutes ago' "$tmp/state/board.json" 2>/dev/null || touch -t "$(date -v-20M +%Y%m%d%H%M)" "$tmp/state/board.json"
"$script" ready >/dev/null
eq "a cache older than the TTL is fetched again" "$(calls)" 6
KOINON_BOARD_TTL=0 "$script" ready >/dev/null
eq "TTL 0 never serves the cache" "$(calls)" 8

cp "$tmp/state/board.json" "$tmp/board.keep"
FAKE_GH_FAIL=1 "$script" --refresh ready >/dev/null 2>"$tmp/err"; eq "a failed fetch exits 1" "$?" 1
grep -q 'board query failed' "$tmp/err" && ok || bad "a failed fetch says why"
cmp -s "$tmp/state/board.json" "$tmp/board.keep" && ok || bad "a failed fetch keeps the old cache"
if compgen -G "$tmp/state/board.*" | grep -qv 'board.json$'; then bad "no temp dir is left behind"; else ok; fi

rm -rf "$tmp/state"
FAKE_GH_FAIL=1 "$script" ready >/dev/null 2>&1; eq "no cache and a failed fetch is exit 1" "$?" 1

if [[ $fails -gt 0 ]]; then echo "board-test: $fails of $total cases failed"; exit 1; fi
echo "board-test: $total cases ok"
