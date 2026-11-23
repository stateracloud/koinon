#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline test (#1198): every way the repository runs buf uses the one pinned tool of hack/tools/buf (the
# `go tool` of hack/buf.sh), never a buf from the PATH and never bufbuild/buf-action. A stub `go` logs its
# arguments and a poisoned `buf` on the PATH fails the test when it runs.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/buf-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
fail() { echo "buf-test: $*" >&2; exit 1; }

mkdir -p "$tmp/bin"
printf '#!/usr/bin/env bash\necho "go $*" >>"$FAKE_LOG"\n' >"$tmp/bin/go"
printf '#!/usr/bin/env bash\necho "a global buf ran: $*" >>"$FAKE_LOG"\nexit 97\n' >"$tmp/bin/buf"
printf '#!/usr/bin/env bash\nexit 0\n' >"$tmp/bin/true-tool"
chmod +x "$tmp/bin/go" "$tmp/bin/buf" "$tmp/bin/true-tool"
export FAKE_LOG="$tmp/log"

want_tool() { # <name> <buf args...>: the log has exactly the pinned-tool call and no global buf
	local name=$1
	shift
	grep -qF -- "go tool -modfile=$root/hack/tools/buf/go.mod buf $*" "$FAKE_LOG" || fail "$name: no pinned-tool call 'buf $*': $(cat "$FAKE_LOG")"
	! grep -q 'a global buf ran' "$FAKE_LOG" || fail "$name: a global buf ran"
	echo "ok: $name"
}

# 1. The script, from another directory (buf reads buf.yaml of the current directory).
: >"$FAKE_LOG"
(cd "$tmp" && PATH="$tmp/bin:$PATH" "$root/hack/buf.sh" lint --path x)
want_tool "hack/buf.sh" lint --path x
: >"$FAKE_LOG"
PATH="$tmp/bin:$PATH" GO=$tmp/bin/go KOINON_TEST_BUF=$tmp/bin/true-tool "$root/hack/buf.sh" lint
[[ ! -s $FAKE_LOG ]] || fail "KOINON_TEST_BUF did not replace the tool"
echo "ok: the test hook replaces the tool"

# 2. The make targets.
command -v make >/dev/null || fail "make is not installed"
: >"$FAKE_LOG"
PATH="$tmp/bin:$PATH" make -s lint-proto
want_tool "make lint-proto" lint
: >"$FAKE_LOG"
PATH="$tmp/bin:$PATH" make -s breaking
want_tool "make breaking" breaking --against '.git#branch=main'

# 3. The preflight gate `proto` and `make generate` (hack/generate.sh): no bare buf call, they go through hack/buf.sh.
bare='(^|[[:space:];&|])buf (lint|breaking|generate|build|format|dep|export|convert|config|push|curl|registry|--)'
! grep -nE "^[^#]*$bare" hack/preflight.sh || fail "hack/preflight.sh calls a bare buf"
grep -q 'hack/buf.sh lint' hack/preflight.sh || fail "preflight gate proto does not run hack/buf.sh lint"
grep -q 'hack/buf.sh breaking' hack/preflight.sh || fail "preflight gate proto does not run hack/buf.sh breaking"
# generate.sh calls a shell function `buf`; that function must be the one-line delegate to hack/buf.sh.
grep -qE '^buf\(\) \{ GO=\$GO "\$buf_sh" "\$@"; \}$' hack/generate.sh || fail "the buf function of hack/generate.sh is not the delegate to hack/buf.sh"
grep -q '^buf_sh=.*/buf.sh$' hack/generate.sh || fail "hack/generate.sh does not name hack/buf.sh"
echo "ok: preflight and generate use hack/buf.sh"

# 4. No script, Makefile or workflow runs a global buf or installs one with buf-action.
for f in Makefile hack/*.sh .github/workflows/*.y*ml .github/actions/*/action.yml; do
	[[ -f $f && $f != hack/buf-test.sh && $f != hack/buf.sh && $f != hack/generate.sh && $f != hack/versions-check.sh && $f != hack/versions-check-test.sh ]] || continue
	! grep -nE "^[^#]*$bare" "$f" | grep -vE 'echo|printf' || fail "$f calls a global buf"
	! grep -nE 'uses:[[:space:]]*bufbuild/buf-(setup-)?action' "$f" || fail "$f installs buf with bufbuild/buf-action"
done
echo "ok: no global buf, no buf-action"

# 5. The modfile pins BUF_VERSION.
ver=$(sed -n 's/^BUF_VERSION=//p' hack/versions.env)
grep -qE "^[[:space:]]*github.com/bufbuild/buf v$ver( |$)" hack/tools/buf/go.mod || fail "hack/tools/buf/go.mod does not require buf v$ver (BUF_VERSION)"
echo "ok: the modfile pins BUF_VERSION $ver"
echo "buf-test ok"
