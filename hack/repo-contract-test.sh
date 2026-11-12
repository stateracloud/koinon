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
for t in test-go test-go-suite test-admission test-chart-smoke chart-check e2e-commands-check; do
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
export KOINON_GO_SLOTS_DIR=$tmp/slots KOINON_DOCKER=koinon-no-such-docker
REPORT_DIR=$tmp/reports KOINON_RC_FILE=$tmp/rc bash hack/go-test.sh ./internal/contract-missing-package >/dev/null 2>&1 || true
if [[ -s $tmp/rc && $(<"$tmp/rc") =~ ^[0-9]+$ ]]; then ok "hack/go-test.sh writes the status file on exit"; else bad "hack/go-test.sh does not write KOINON_RC_FILE on exit"; fi

# (#846) The maintainer scripts are labelled and name no org: hack/README.md lists them, each carries the
# label in its header, and the repository and board come from hack/repo-lib.sh alone.
listed=$(sed -n 's/^| `\([^`]*\.sh\)` |.*/\1/p' hack/README.md | sort)
labelled=$(cd hack && grep -rl --include='*.sh' -m1 '^# Maintainer script (hack/README.md): ' . | sed 's|^\./||' | sort)
[[ -n $listed ]] || bad "hack/README.md lists no maintainer scripts"
if [[ $listed == "$labelled" ]]; then ok "hack/README.md lists exactly the labelled maintainer scripts"; else
	bad "hack/README.md and the 'Maintainer script' headers disagree: $(diff <(echo "$listed") <(echo "$labelled") | grep '^[<>]' | tr '\n' ' ')"
fi
for s in $listed; do
	[[ -L hack/$s ]] && continue
	sed -n '1,8p' "hack/$s" | grep -q '^# Maintainer script (hack/README.md): ' || bad "hack/$s: the label is not in its header"
	if grep -n 'stateracloud' "hack/$s" | grep -v '^[0-9]*:#' >/dev/null; then bad "hack/$s names the org in code; use hack/repo-lib.sh"; fi
done
ok "maintainer scripts name no org outside comments"

# shellcheck source=repo-lib.sh
(
	source hack/repo-lib.sh
	unset KOINON_REPO GH_REPO KOINON_UPSTREAM KOINON_BOARD KOINON_BOARD_OWNER
	up=$(sed -n 's|^module github\.com/||p' go.mod)
	chk() { if [[ $2 == "$3" ]]; then ok "repo-lib: $1"; else bad "repo-lib: $1 (got '$2', want '$3')"; fi; }
	chk "upstream from go.mod" "$(koinon_upstream)" "$up"
	chk "https remote" "$(koinon_github_slug https://github.com/someone/koinon.git)" someone/koinon
	chk "scp remote" "$(koinon_github_slug git@github.com:someone/koinon.git)" someone/koinon
	chk "ssh remote" "$(koinon_github_slug ssh://git@github.com/someone/koinon)" someone/koinon
	chk "proxy remote is not GitHub" "$(koinon_github_slug http://127.0.0.1:1234/git/someone/koinon)" ""
	chk "fork repo" "$(KOINON_REPO=someone/koinon koinon_repo)" someone/koinon
	chk "GH_REPO" "$(GH_REPO=other/koinon koinon_repo)" other/koinon
	chk "fork has no board" "$(KOINON_REPO=someone/koinon koinon_board)" ""
	chk "upstream board" "$(KOINON_REPO=$up koinon_board)" "${up%%/*} 1"
	chk "fork board override" "$(KOINON_REPO=someone/koinon KOINON_BOARD=3 koinon_board)" "someone 3"
	chk "board owner override" "$(KOINON_REPO=$up KOINON_BOARD_OWNER=acme koinon_board)" "acme 1"
	chk "board none" "$(KOINON_REPO=$up KOINON_BOARD=none koinon_board)" ""
	if (KOINON_BOARD=x koinon_board 2>/dev/null); then bad "repo-lib: a bad KOINON_BOARD is refused"; else ok "repo-lib: a bad KOINON_BOARD is refused"; fi
	exit "$fail"
) || fail=1

if [[ $fail -ne 0 ]]; then exit 1; fi
echo "repo contract ok"
