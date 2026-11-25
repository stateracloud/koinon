#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/release-base-mirror.sh with a stubbed crane: the pinned gcr.io base is copied to the release registry
# by digest, a short registry fault on the copy is retried (#1224: HTTP 500 and "Too many connections" on a blob
# GET), and any other failure stops at once.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=$here/release-base-mirror.sh
tmp=$(mktemp -d "${TMPDIR:-/tmp}/release-base-mirror-test.XXXXXX") || exit 1
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"

# crane: records the call; the first CR_FAIL_N copies fail with CR_FAIL_MSG on stderr.
cat >"$tmp/bin/crane" <<'STUB'
#!/usr/bin/env bash
echo "crane $*" >>"$TMP/calls"
n=$(($(cat "$TMP/cr-n" 2>/dev/null || echo 0) + 1))
echo "$n" >"$TMP/cr-n"
if ((n <= ${CR_FAIL_N:-0})); then echo "${CR_FAIL_MSG:-}" >&2; exit 1; fi
STUB
chmod +x "$tmp/bin/crane"

fails=0
check() { # name got want
	if [[ $2 == "$3" ]]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fails=$((fails + 1)); fi
}
run() { # prints the exit code; stdout in $tmp/stdout, stderr in $tmp/err
	local rc=0
	: >"$tmp/calls"
	rm -f "$tmp/cr-n"
	PUSH_RETRY_DELAY=0 TMP=$tmp PATH=$tmp/bin:$PATH bash "$script" ghcr.io/o/r >"$tmp/stdout" 2>"$tmp/err" || rc=$?
	echo "$rc"
}

pinned=$(grep -oE 'gcr\.io/distroless/static:nonroot@sha256:[0-9a-f]{64}' "$here/../.goreleaser.yaml" | head -1)
digest=${pinned#*@}

check "a clean copy succeeds" "$(run)" 0
check "...copies the pinned base by digest" "$(cat "$tmp/calls")" "crane copy $pinned ghcr.io/o/r/base/distroless-static@$digest"
check "...prints the copy, with the same digest" "$(cat "$tmp/stdout")" "ghcr.io/o/r/base/distroless-static@$digest"

# The two faults of #1224: a manifest answer and a blob GET.
fault500='GET https://gcr.io/v2/distroless/static/manifests/sha256:x: INTERNAL_SERVER_ERROR: tag lookup unavailable'
fault1040='GET https://gcr.io/v2/distroless/static/blobs/sha256:x: INTERNAL_SERVER_ERROR: Error 1040: Too many connections'
check "a short fault, then success, copies" "$(CR_FAIL_N=2 CR_FAIL_MSG=$fault500 run)" 0
check "...three attempts" "$(cat "$tmp/cr-n")" 3
check "...and said why it retried" "$(grep -c 'push retry: a registry fault' "$tmp/err")" 2
check "...and still printed the copy once" "$(cat "$tmp/stdout")" "ghcr.io/o/r/base/distroless-static@$digest"
check "a blob GET fault (Too many connections) is retried" "$(CR_FAIL_N=1 CR_FAIL_MSG=$fault1040 run)" 0
check "...two attempts" "$(cat "$tmp/cr-n")" 2
check "a fault on every attempt fails" "$(CR_FAIL_N=99 CR_FAIL_MSG=$fault500 run)" 1
check "...after exactly five attempts" "$(cat "$tmp/cr-n")" 5
check "...and prints no reference" "$(cat "$tmp/stdout")" ""
check "another error fails at once" "$(CR_FAIL_N=99 CR_FAIL_MSG='GET: UNAUTHORIZED: authentication required' run)" 1
check "...with no retry" "$(cat "$tmp/cr-n")" 1

((fails == 0)) || { echo "$fails failed"; exit 1; }
echo "release-base-mirror-test: all ok"
