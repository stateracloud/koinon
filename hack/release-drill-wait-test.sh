#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/release-drill-wait.sh with a stubbed gh: it waits for the tag push's e2e-kind run to finish,
# gives up (exit 0, the gate decides) when no run appears or the run outlasts the limit, and does nothing
# for a final tag.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
wait_script=$here/release-drill-wait.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/release-drill-wait-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"

# gh api <path> --jq <expr>: serves $tmp/runs.json (the API shape) through jq; $tmp/calls counts the looks.
# $tmp/runs-after-N.json replaces it from the N-th look on.
cat >"$tmp/bin/gh" <<'EOF'
#!/usr/bin/env bash
shift # api
expr=.
while (($#)); do
	case $1 in
	--jq) expr=$2; shift ;;
	esac
	shift
done
n=$(($(cat "$TMP/calls" 2>/dev/null || echo 0) + 1))
echo "$n" >"$TMP/calls"
f=$TMP/runs.json
for a in "$TMP"/runs-after-*.json; do
	[[ -e $a ]] || continue
	k=${a##*-}; k=${k%.json}
	((n >= k)) && f=$a
done
[[ -e $TMP/api-error ]] && exit 1
jq -r "$expr" "$f"
EOF
chmod +x "$tmp/bin/gh"

fails=0
check() { # name got want
	if [[ $2 == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fails=$((fails + 1)); fi
}
run_json() { # status [tag]
	echo '{"workflow_runs":[{"id":7,"status":"'"$1"'","head_branch":"'"${2:-v1.2.3-rc.1}"'","created_at":"2026-11-13T10:00:00Z"}]}'
}
wait_rc() { # env as given: exit code; output in $tmp/out
	local rc=0
	TMP=$tmp PATH=$tmp/bin:$PATH TAG=${TAG:-v1.2.3-rc.1} SHA=aaaa111 GITHUB_REPOSITORY=o/r GH_TOKEN=koinon_test_not-a-real-token \
		DRILL_WAIT_POLL=1 bash "$wait_script" >"$tmp/out" 2>&1 || rc=$?
	echo "$rc"
}
reset() { rm -f "$tmp"/calls "$tmp"/runs*.json "$tmp/api-error"; }

reset
run_json completed >"$tmp/runs.json"
check "a finished drill run returns at once" "$(wait_rc)" 0
check "...after one look" "$(cat "$tmp/calls")" 1

reset
run_json in_progress >"$tmp/runs.json"
run_json completed >"$tmp/runs-after-3.json"
check "a running drill is waited for" "$(wait_rc)" 0
check "...until the third look" "$(cat "$tmp/calls")" 3
check "...and says it waited" "$(grep -c 'finished after' "$tmp/out")" 1

reset
run_json in_progress >"$tmp/runs.json"
check "a run that outlasts the limit ends the wait with exit 0 (the gate decides)" "$(DRILL_WAIT_MAX=2 wait_rc)" 0
check "...and says it gave up" "$(grep -c 'still going after 2 s' "$tmp/out")" 1

reset
echo '{"workflow_runs":[]}' >"$tmp/runs.json"
check "no run for the tag ends the wait with exit 0 (the gate decides)" "$(DRILL_WAIT_APPEAR=2 wait_rc)" 0
check "...and says so" "$(grep -c 'no e2e-kind run for v1.2.3-rc.1' "$tmp/out")" 1

reset
run_json completed v1.2.3-rc.2 >"$tmp/runs.json"
check "a run of another tag does not count" "$(DRILL_WAIT_APPEAR=2 wait_rc)" 0
check "...it was waited out" "$(grep -c 'no e2e-kind run' "$tmp/out")" 1

reset
: >"$tmp/api-error"
echo '{}' >"$tmp/runs.json"
check "an API error is waited out, not fatal (the gate decides)" "$(DRILL_WAIT_APPEAR=2 wait_rc)" 0

reset
run_json in_progress >"$tmp/runs.json"
echo '{"workflow_runs":[]}' >"$tmp/runs-after-2.json"
run_json completed >"$tmp/runs-after-5.json"
check "a run seen once, then missing from a look, is still waited for (appear < max)" "$(DRILL_WAIT_APPEAR=1 DRILL_WAIT_MAX=60 wait_rc)" 0
check "...until it finished" "$(cat "$tmp/calls")" 5

reset
check "a final tag waits for nothing" "$(TAG=v1.2.3 wait_rc)" 0
check "...and makes no API call" "$(cat "$tmp/calls" 2>/dev/null || echo 0)" 0

reset
check "a limit that is not a number is refused" "$(DRILL_WAIT_MAX=soon wait_rc)" 1

((fails == 0)) || { echo "$fails failed" >&2; exit 1; }
echo "all passed"
