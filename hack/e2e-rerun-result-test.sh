#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline test of hack/e2e-rerun-result.sh (old issue 675): a re-run must not cancel a live run of a newer head.
# gh is a stub; nothing talks to GitHub.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }
set -uo pipefail
repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/e2e-rerun-result-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
export FAKE_LOG=$tmp/log

cat >"$tmp/gh" <<'SH'
#!/usr/bin/env bash
echo "gh $*" >>"$FAKE_LOG"
case "$1 $2" in
"run list")
	if [[ " $* " == *" --commit "* ]]; then
		echo -e "101\tfeat/1-x"
	else
		echo "${FAKE_LIVE_JSON:-[]}"
	fi
	;;
"api repos/o/r/actions/runs/101/jobs?per_page=100") echo 555 ;;
esac
exit 0
SH
chmod +x "$tmp/gh"

pass=0 fail=0
check() { local d=$1; shift; if "$@" >/dev/null 2>&1; then echo "ok: $d"; pass=$((pass + 1)); else echo "FAIL: $d" >&2; fail=$((fail + 1)); fi; }
# shellcheck disable=SC2329 # called through check
has() { grep -q -- "$1" "$2"; }
# shellcheck disable=SC2329
hasnt() { ! grep -q -- "$1" "$2"; }

sha=$(printf 'a%.0s' {1..40}) other=$(printf 'b%.0s' {1..40})
export GH=$tmp/gh GH_REPO=o/r

: >"$FAKE_LOG"
unset FAKE_LIVE_JSON
"$repo/hack/e2e-rerun-result.sh" "$sha" >"$tmp/o1" 2>&1
check "no live run: re-runs the result job" has 'gh run rerun 101 --job 555' "$FAKE_LOG"

: >"$FAKE_LOG"
FAKE_LIVE_JSON='[{"status":"in_progress","headSha":"'$other'"}]' "$repo/hack/e2e-rerun-result.sh" "$sha" >"$tmp/o2" 2>&1
check "live run on a newer head: no re-run" hasnt 'gh run rerun' "$FAKE_LOG"
check "live run on a newer head: says why" has 'would cancel it' "$tmp/o2"

: >"$FAKE_LOG"
FAKE_LIVE_JSON='[{"status":"in_progress","headSha":"'$sha'"}]' "$repo/hack/e2e-rerun-result.sh" "$sha" >"$tmp/o3" 2>&1
check "live run on the same head is not a reason to skip" has 'gh run rerun 101' "$FAKE_LOG"

echo "$pass passed, $fail failed"
exit $((fail > 0))
