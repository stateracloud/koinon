#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# hack/govulncheck.sh (#579 part 2): the exit code mapping (0 clean, 1 finding, 2 could not decide),
# the pinned version, and that nothing retries. A stub `go` installs a stub govulncheck, so no network.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/govulncheck-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
fail() { echo "govulncheck-test: $*" >&2; exit 1; }

mkdir -p "$tmp/bin"
# `go install pkg@version` (GOBIN set) writes a govulncheck that exits FAKE_VULN_RC; FAKE_INSTALL_RC fails the install.
cat >"$tmp/bin/go" <<'EOF'
#!/usr/bin/env bash
echo "go $*" >>"$FAKE_LOG"
if [[ $1 == version ]]; then echo "go version $GOTOOLCHAIN [toolchain probe]" >>"$FAKE_LOG"; exit "${FAKE_TOOLCHAIN_RC:-0}"; fi
[[ $1 == install ]] || exit 64
[[ ${FAKE_INSTALL_RC:-0} -eq 0 ]] || { echo "go: fake proxy unreachable" >&2; exit "$FAKE_INSTALL_RC"; }
printf '#!/usr/bin/env bash\necho "govulncheck $* [GOTOOLCHAIN=${GOTOOLCHAIN:-}]" >>"$FAKE_LOG"\nexit "${FAKE_VULN_RC:-0}"\n' >"$GOBIN/govulncheck"
chmod +x "$GOBIN/govulncheck"
EOF
chmod +x "$tmp/bin/go"
export PATH="$tmp/bin:$PATH" FAKE_LOG="$tmp/log"
mkdir -p "$tmp/mod"
printf 'module example.invalid/m\n\ngo 1.27.3\n' >"$tmp/mod/go.mod"
cd "$tmp/mod"
pinned=$(sed -n 's/^GOVULNCHECK_VERSION=//p' "$root/hack/versions.env")
[[ -n $pinned ]] || fail "no GOVULNCHECK_VERSION in hack/versions.env"

run() { # <name> <expected rc> <expected stderr text> [VAR=value...]: runs the script, checks rc and message
	local name=$1 want=$2 text=$3 rc=0
	shift 3
	: >"$FAKE_LOG"
	env "$@" "$root/hack/govulncheck.sh" >"$tmp/out" 2>"$tmp/err" || rc=$?
	[[ $rc -eq $want ]] || fail "$name: exit $rc, want $want ($(cat "$tmp/err"))"
	if [[ -n $text ]]; then grep -qF -- "$text" "$tmp/out" "$tmp/err" || fail "$name: no '$text' in the output"; fi
	echo "ok: $name"
}

run "clean tree passes" 0 "no reachable known vulnerability" FAKE_VULN_RC=0
run "a reachable finding fails with 1" 1 "FAIL, a known vulnerability is reachable" FAKE_VULN_RC=3
run "a failed database fetch fails with 2, not a pass" 2 "COULD NOT DECIDE (exit 1)" FAKE_VULN_RC=1
run "a failed database fetch is named, with no retry" 2 "no retry (rule T1)" FAKE_VULN_RC=1
run "a failed tool install fails with 2" 2 "FAILED to install govulncheck" FAKE_INSTALL_RC=1
[[ $(grep -c '^go install' "$FAKE_LOG") -eq 1 ]] || fail "the install was attempted more than once"

run "the pinned version is installed" 0 "" FAKE_VULN_RC=0
grep -qF "go install golang.org/x/vuln/cmd/govulncheck@$pinned" "$FAKE_LOG" || fail "versions.env pin $pinned not used: $(cat "$FAKE_LOG")"
other=v0.0.0-test
run "the environment overrides the pin" 0 "" FAKE_VULN_RC=0 GOVULNCHECK_VERSION=$other
grep -qF "govulncheck@$other" "$FAKE_LOG" || fail "GOVULNCHECK_VERSION from the environment ignored"

run "the default pattern is ./..." 0 "" FAKE_VULN_RC=0
grep -qx 'govulncheck ./... \[GOTOOLCHAIN=go1.27.3\]' "$FAKE_LOG" || fail "default pattern not ./..."
: >"$FAKE_LOG"
FAKE_VULN_RC=0 "$root/hack/govulncheck.sh" ./cmd/... >/dev/null 2>&1
grep -q '^govulncheck ./cmd/\.\.\. ' "$FAKE_LOG" || fail "a given pattern was not passed on"
echo "ok: a pattern is passed on"

# A finding must never be retried: the tool ran once.
run "a finding runs the tool once" 1 "" FAKE_VULN_RC=3
[[ $(grep -c '^govulncheck ' "$FAKE_LOG") -eq 1 ]] || fail "the tool ran more than once"

# The toolchain of go.mod is used when it is available, else the default one with a notice.
run "the go.mod toolchain is used" 0 "" FAKE_VULN_RC=0
grep -qF '[GOTOOLCHAIN=go1.27.3]' "$FAKE_LOG" || fail "GOTOOLCHAIN not set from go.mod"
run "an unavailable toolchain keeps the default and says so" 0 "using the default toolchain" FAKE_VULN_RC=0 FAKE_TOOLCHAIN_RC=1
grep -qF '[GOTOOLCHAIN=]' "$FAKE_LOG" || fail "GOTOOLCHAIN was set although the toolchain is unavailable"
run "a GOTOOLCHAIN from the caller is kept" 0 "" FAKE_VULN_RC=0 GOTOOLCHAIN=local
grep -qF '[GOTOOLCHAIN=local]' "$FAKE_LOG" || fail "the caller's GOTOOLCHAIN was overridden"

# Annotations on GitHub Actions only (the cause on the run page, classified by hack/ci-fail.sh).
run "a finding is annotated in Actions" 1 "::error title=govulncheck::reachable vulnerability" FAKE_VULN_RC=3 GITHUB_ACTIONS=true
run "could not decide is annotated in Actions" 2 "::error title=govulncheck::COULD NOT DECIDE" FAKE_VULN_RC=1 GITHUB_ACTIONS=true
run "a failed install is annotated in Actions" 2 "::error title=govulncheck::COULD NOT DECIDE" FAKE_INSTALL_RC=1 GITHUB_ACTIONS=true
run "no annotation outside Actions" 1 "" FAKE_VULN_RC=3
! grep -q '^::error' "$tmp/out" "$tmp/err" || fail "an annotation outside Actions"
echo "govulncheck-test ok"
