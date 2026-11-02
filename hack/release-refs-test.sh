#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/release-refs.sh on the digests.txt lines of a release candidate and of a final release.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/.."

h=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef

# check <name> <registry> <input>: the output must name koinon-api and koinon-agentd in <registry>.
check() {
	local want got
	want="koinon-api $2/koinon-api@sha256:$h"$'\n'"koinon-agentd $2/koinon-agentd@sha256:$h"
	got=$(hack/release-refs.sh <<<"$3")
	if [[ $got == "$want" ]]; then echo "ok: $1"; else echo "FAIL: $1"$'\n'"$got" >&2; exit 1; fi
}

R=ghcr.io/stateracloud
check "release candidate (tagged repository)" $R "$h  $R/koinon-api:0.1.0-rc.1
$h  $R/koinon-agentd:0.1.0-rc.1"
check "final release (bare repository)" $R "$h  $R/koinon-api
$h  $R/koinon-agentd"
check "registry with a port" localhost:5000 "$h  localhost:5000/koinon-api:0.1.0-rc.1
$h  localhost:5000/koinon-agentd"
