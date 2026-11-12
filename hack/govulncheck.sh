#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# govulncheck over the module, at the version pinned in hack/versions.env (GOVULNCHECK_VERSION). The one
# way to run it: the `go-vuln` job of ci.yml (pull requests that change go.mod or go.sum, #579 part 2),
# the nightly govulncheck.yml and the preflight gate `vuln` all call this script (ADR 0079, ADR 0111, rule X3).
#
#   hack/govulncheck.sh [package pattern...]     default ./...
#
# Exit codes:
#   0  no reachable known vulnerability
#   1  a reachable known vulnerability (govulncheck exit 3): the report is above; this is a real finding
#   2  govulncheck could not decide: the tool did not install, or the vulnerability database
#      (vuln.go.dev) could not be fetched or read. This is NOT a pass and there is no automatic retry
#      (rule T1 of docs/dev/ci-rules.md): re-run the job by hand once the network is back.
#
# `go run` is not used: it turns the tool's exit code 3 into 1, and a finding could not be told from a
# failed fetch. Needs bash 5, go and network access to proxy.golang.org, sum.golang.org and vuln.go.dev.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

if [[ -z ${GOVULNCHECK_VERSION:-} ]]; then
	# shellcheck source=/dev/null
	. "$(dirname "$0")/versions.env"
fi
[[ -n ${GOVULNCHECK_VERSION:-} ]] || { echo "govulncheck: GOVULNCHECK_VERSION is not set (hack/versions.env)" >&2; exit 2; }

patterns=("$@")
[[ ${#patterns[@]} -gt 0 ]] || patterns=(./...)

# Scan the standard library of the Go version in go.mod, as CI does (setup-go reads the same line), also in a
# local run with another Go on the PATH. If that toolchain cannot be had (a download the sandbox or network
# refuses), keep the default toolchain and say so: the standard library part of the result may then differ.
if [[ -z ${GOTOOLCHAIN:-} && -f go.mod ]]; then
	modgo=$(awk '$1 == "go" { print $2; exit }' go.mod)
	if [[ -n $modgo ]]; then
		if GOTOOLCHAIN="go$modgo" go version >/dev/null 2>&1; then
			export GOTOOLCHAIN="go$modgo"
		else
			echo "govulncheck: NOTICE: the Go toolchain go$modgo of go.mod is not available here; using the default toolchain, so the standard library part of the result can differ from CI" >&2
		fi
	fi
fi

# On GitHub Actions the cause goes on the run page as an annotation (hack/ci-fail.sh classifies the text).
annotate() { [[ -z ${GITHUB_ACTIONS:-} ]] || echo "::error title=govulncheck::$1"; }

bindir=$(mktemp -d "${TMPDIR:-/tmp}/govulncheck.XXXXXX")
trap 'rm -rf "$bindir"' EXIT

if ! GOBIN=$bindir go install "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}"; then
	msg="COULD NOT DECIDE: FAILED to install govulncheck ${GOVULNCHECK_VERSION} (module proxy or checksum database unreachable?). This is not a pass; no retry (rule T1). Re-run by hand."
	annotate "$msg"
	echo "govulncheck: $msg" >&2
	exit 2
fi

rc=0
"$bindir/govulncheck" "${patterns[@]}" || rc=$?
case $rc in
0) echo "govulncheck ${GOVULNCHECK_VERSION}: no reachable known vulnerability" ;;
3)
	annotate "reachable vulnerability: a known vulnerability is reachable from this module (govulncheck ${GOVULNCHECK_VERSION}; report in the log above)"
	echo "govulncheck ${GOVULNCHECK_VERSION}: FAIL, a known vulnerability is reachable from this module (report above)" >&2
	exit 1
	;;
*)
	msg="COULD NOT DECIDE (exit $rc): the vulnerability database could not be fetched or read, or the packages did not load. This is not a pass; no retry (rule T1). Re-run by hand."
	annotate "$msg"
	echo "govulncheck ${GOVULNCHECK_VERSION}: $msg" >&2
	exit 2
	;;
esac
