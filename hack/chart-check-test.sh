#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline test of hack/chart-check.sh's failure gate (#323): a FAIL from any
# assertion file, in any shell level, or an assertion file that exits non-zero, must fail the
# script. Helm and Go are stubbed, so this needs only bash.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

cd "$(dirname "$0")/.."
tmp=$(mktemp -d "${TMPDIR:-/tmp}/chart-check-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/bin"
cat >"$tmp/bin/helm" <<'S'
#!/usr/bin/env bash
[ "$1" = template ] && echo "kind: ConfigMap"
exit 0
S
cat >"$tmp/bin/go" <<'S'
#!/usr/bin/env bash
exit 0
S
chmod +x "$tmp/bin/helm" "$tmp/bin/go"

fail=0
# run_case <name> <want: pass|fail> <assertion file body>
run_case() {
	local name=$1 want=$2 body=$3 d rc=0
	d="$tmp/$name.d"
	mkdir -p "$d"
	printf '%s\n' "$body" >"$d/10-case.sh"
	CHART_CHECK_D="$d" HELM="$tmp/bin/helm" PATH="$tmp/bin:$PATH" hack/chart-check.sh >"$tmp/$name.log" 2>&1 || rc=$?
	if { [ "$want" = pass ] && [ "$rc" -eq 0 ]; } || { [ "$want" = fail ] && [ "$rc" -ne 0 ]; }; then
		echo "ok: $name (rc=$rc)"
	else
		echo "FAIL: $name: want $want, got rc=$rc"
		sed 's/^/    /' "$tmp/$name.log"
		fail=1
	fi
}

run_case passing-assertion pass 'has "renders a ConfigMap" "kind: ConfigMap"'
run_case failing-assertion fail 'has "planted: never matches" "no-such-text"'
run_case failing-lacks fail 'lacks "planted: always matches" "kind: ConfigMap"'
run_case direct-fail fail 'fail "planted direct FAIL"'
run_case subshell-fail fail '( fail "planted FAIL in a subshell" )'
run_case pipe-fail fail 'echo x | { fail "planted FAIL in a pipe"; }'
run_case file-exits-nonzero fail 'exit 1'

exit "$fail"
