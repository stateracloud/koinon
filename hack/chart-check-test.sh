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
# Default stub reports helm 4 so the version guard passes; FAKE_HELM_VERSION overrides it.
cat >"$tmp/bin/helm" <<'S'
#!/usr/bin/env bash
if [ "$1" = version ]; then
	echo "${FAKE_HELM_VERSION:-v4.3.1}"
	exit 0
fi
[ "$1" = template ] && echo "kind: ConfigMap"
[ "$1" = install ] && { echo "NOTES"; exit 0; }
exit 0
S
cat >"$tmp/bin/go" <<'S'
#!/usr/bin/env bash
exit 0
S
chmod +x "$tmp/bin/helm" "$tmp/bin/go"

fail=0
# run_case <name> <want: pass|fail> <assertion file body> [extra env assignments...]
run_case() {
	local name=$1 want=$2 body=$3 d rc=0
	shift 3
	d="$tmp/$name.d"
	mkdir -p "$d"
	printf '%s\n' "$body" >"$d/10-case.sh"
	# shellcheck disable=SC2086 # intentional: caller may pass FAKE_HELM_VERSION=...
	env "$@" CHART_CHECK_D="$d" HELM="$tmp/bin/helm" PATH="$tmp/bin:$PATH" hack/chart-check.sh >"$tmp/$name.log" 2>&1 || rc=$?
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
# helm 3 (and anything below 4) is refused up front (#349).
run_case helm-v3-refused fail 'true' FAKE_HELM_VERSION=v3.19.0
grep -q 'need helm 4 or newer' "$tmp/helm-v3-refused.log" || {
	echo "FAIL: helm-v3-refused: message missing"
	sed 's/^/    /' "$tmp/helm-v3-refused.log"
	fail=1
}
# A failing helm install for NOTES is a FAIL naming helm, not a silent missing-text (#349).
cat >"$tmp/bin/helm-fail-install" <<'S'
#!/usr/bin/env bash
if [ "$1" = version ]; then echo "v4.3.1"; exit 0; fi
[ "$1" = template ] && echo "kind: ConfigMap" && exit 0
[ "$1" = install ] && { echo "Error: INSTALLATION FAILED: Kubernetes cluster unreachable"; exit 1; }
exit 0
S
chmod +x "$tmp/bin/helm-fail-install"
d="$tmp/notes-install-fails.d"
mkdir -p "$d"
# The planted assertion expands $(head ...) when chart-check sources it, not here.
# shellcheck disable=SC2016
printf '%s\n' '
notes=""
if ! notes=$("$HELM" install koinon "$CHART" -n koinon --dry-run=client 2>&1); then
	fail "helm install --dry-run=client for NOTES failed: $(head -1 <<<"$notes")"
else
	grep -q NEVER <<<"$notes" || fail "missing NOTES"
fi
' >"$d/10-case.sh"
rc=0
CHART_CHECK_D="$d" HELM="$tmp/bin/helm-fail-install" PATH="$tmp/bin:$PATH" hack/chart-check.sh >"$tmp/notes-install-fails.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]] && grep -q 'helm install --dry-run=client for NOTES failed' "$tmp/notes-install-fails.log"; then
	echo "ok: notes-install-fails (rc=$rc)"
else
	echo "FAIL: notes-install-fails: want fail with helm install message, got rc=$rc"
	sed 's/^/    /' "$tmp/notes-install-fails.log"
	fail=1
fi

exit "$fail"
