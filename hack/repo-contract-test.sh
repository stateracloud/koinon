#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline contract test: the entry points that CI, hack/preflight.sh and contributors on kind call by name
# stay in place. Fails when a make target goes away, or when hack/go-test.sh stops honouring the
# environment variables its callers set. Nothing here needs a cluster, Docker or the network.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

fail=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1" >&2; fail=1; }

# 1. make targets, as `make -n` sees them (a target without a recipe or a rule is an error).
for t in test-go test-go-suite test-admission test-chart-smoke chart-check docs-check; do
	if grep -qE "^${t}:" Makefile; then ok "make target $t"; else bad "Makefile has no target $t"; fi
	if make -n -s "$t" SUITE=./internal/x >/dev/null 2>&1; then ok "make -n $t"; else bad "make -n $t fails"; fi
done

# 2. hack/go-test.sh reads these variables.
for v in KOINON_RC_FILE REPORT_DIR PROCS; do
	if grep -qE "\\$\{$v(:-|\})" hack/go-test.sh; then ok "hack/go-test.sh honours $v"; else bad "hack/go-test.sh no longer reads $v"; fi
done

# 3. KOINON_RC_FILE is written on exit even when the run does no work. --list cannot be used for this:
# it exits before the EXIT trap is set, so it never writes the file. A package that does not exist makes
# `go list` fail before any test starts, and that exit still goes through the trap.
tmp=$(mktemp -d "${TMPDIR:-/tmp}/rc.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
REPORT_DIR=$tmp/reports KOINON_RC_FILE=$tmp/rc bash hack/go-test.sh ./internal/contract-missing-package >/dev/null 2>&1 || true
if [[ -s $tmp/rc && $(<"$tmp/rc") =~ ^[0-9]+$ ]]; then ok "hack/go-test.sh writes the status file on exit"; else bad "hack/go-test.sh does not write KOINON_RC_FILE on exit"; fi

if [[ $fail -ne 0 ]]; then exit 1; fi
echo "repo contract ok"
