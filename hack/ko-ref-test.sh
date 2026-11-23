#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/ko-ref.sh on the forms of ko's stdout: with a tag (the rc.28 line of #1219), without one,
# several lines, a registry port, and the inputs it must refuse.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."

h=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
R=ghcr.io/stateracloud/koinon/ate-env-guest

# ok <name> <input> <want>
ok() {
	local got
	got=$(hack/ko-ref.sh <<<"$2" 2>&1) || { echo "FAIL: $1: exit $?: $got" >&2; exit 1; }
	[[ $got == "$3" ]] || { echo "FAIL: $1: got '$got', want '$3'" >&2; exit 1; }
	echo "ok: $1"
}
# bad <name> <input> <message part>
bad() {
	local got rc=0
	got=$(hack/ko-ref.sh <<<"$2" 2>&1) || rc=$?
	[[ $rc -eq 1 && $got == *"$3"* ]] || { echo "FAIL: $1: exit $rc, output '$got'" >&2; exit 1; }
	echo "ok: $1"
}

ok "tag and digest (rc.28 line of #1219)" \
	"ghcr.io/stateracloud/koinon/ate-env-guest:0.1.0-rc.28@sha256:a7b70ee309eb3a9bb4dad9b8daa5be6f4fe5712c62172c8362c8ef7253553866" \
	"$R@sha256:a7b70ee309eb3a9bb4dad9b8daa5be6f4fe5712c62172c8362c8ef7253553866"
ok "digest only" "$R@sha256:$h" "$R@sha256:$h"
ok "registry with a port and a tag" "localhost:5000/ate-env-guest:0.1.0@sha256:$h" "localhost:5000/ate-env-guest@sha256:$h"
ok "registry with a port, no tag" "localhost:5000/ate-env-guest@sha256:$h" "localhost:5000/ate-env-guest@sha256:$h"
ok "last matching line wins" "$R:1@sha256:${h//0/1}
$R:2@sha256:$h" "$R@sha256:$h"
ok "log noise around the line" "something else
$R:0.1.0@sha256:$h
" "$R@sha256:$h"

bad "empty" "" "no '<repository>"
bad "no digest" "$R:0.1.0" "no '<repository>"
bad "short digest" "$R@sha256:${h:0:63}" "no '<repository>"
bad "long digest" "$R@sha256:${h}0" "no '<repository>"
bad "upper-case digest" "$R@sha256:${h^^}" "no '<repository>"
bad "upper-case repository" "GHCR.io/Koinon/x@sha256:$h" "no '<repository>"
bad "two @" "$R@x@sha256:$h" "no '<repository>"
bad "another algorithm" "$R@sha512:$h" "no '<repository>"
echo "ko-ref-test: all passed"
