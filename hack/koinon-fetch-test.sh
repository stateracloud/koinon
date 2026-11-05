#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline test of flavors/common/koinon-fetch and of how the Dockerfiles use it (#145): GitHub
# release assets are downloaded with the optional github_token BuildKit secret, anonymously
# without it, never with the token in argv, and always checked against the pinned sha256. A fake
# curl on PATH records how it was called.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
fetch=$root/flavors/common/koinon-fetch

tmp=$(mktemp -d "${TMPDIR:-/tmp}/koinon-fetch-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"

# Fake curl: logs argv and stdin, writes fixed content to the -o file.
cat >"$tmp/bin/curl" <<'EOF'
#!/bin/sh
out=
args="$*"
while [ "$#" -gt 0 ]; do
  [ "$1" = -o ] && out=$2
  shift
done
printf '%s\n' "$args" >>"$FAKE_CURL_LOG"
if printf '%s' "$args" | grep -q -- ' -K '; then cat >>"$FAKE_CURL_STDIN"; fi
printf 'koinon_test_payload' >"$out"
EOF
chmod +x "$tmp/bin/curl"
export PATH="$tmp/bin:$PATH" FAKE_CURL_LOG=$tmp/curl.log FAKE_CURL_STDIN=$tmp/curl.stdin

good=$(printf 'koinon_test_payload' | sha256sum | cut -d' ' -f1)
fail=0
check() { # check <name> <condition result 0/1>
  if [ "$2" -eq 0 ]; then echo "ok   $1"; else echo "FAIL $1"; fail=1; fi
}
reset() { : >"$FAKE_CURL_LOG"; : >"$FAKE_CURL_STDIN"; rm -f "$tmp/out"; }

gh=https://github.com/o/r/releases/download/v1/asset.tgz

# 1. With a token, a github.com URL gets the header on stdin and not in argv.
reset
printf 'koinon_test_not-a-real-token\n' >"$tmp/token"
KOINON_FETCH_TOKEN_FILE=$tmp/token "$fetch" "$good" "$gh" "$tmp/out" >/dev/null
check "token goes to curl through stdin" "$(grep -q 'Authorization: Bearer koinon_test_not-a-real-token' "$FAKE_CURL_STDIN" && echo 0 || echo 1)"
check "token is not in argv" "$(grep -q 'not-a-real-token' "$FAKE_CURL_LOG" && echo 1 || echo 0)"

# 2. No token file: anonymous.
reset
KOINON_FETCH_TOKEN_FILE=$tmp/missing "$fetch" "$good" "$gh" "$tmp/out" >/dev/null
check "no secret: anonymous request" "$(grep -q -- ' -K ' "$FAKE_CURL_LOG" && echo 1 || echo 0)"

# 3. Empty secret: anonymous.
reset
: >"$tmp/empty"
KOINON_FETCH_TOKEN_FILE=$tmp/empty "$fetch" "$good" "$gh" "$tmp/out" >/dev/null
check "empty secret: anonymous request" "$(grep -q -- ' -K ' "$FAKE_CURL_LOG" && echo 1 || echo 0)"

# 4. A non-GitHub host never gets the token.
reset
KOINON_FETCH_TOKEN_FILE=$tmp/token "$fetch" "$good" https://example.com/x.tgz "$tmp/out" >/dev/null
check "token is not sent to other hosts" "$(grep -q -- ' -K ' "$FAKE_CURL_LOG" && echo 1 || echo 0)"

# 5. A wrong checksum fails.
reset
if KOINON_FETCH_TOKEN_FILE=$tmp/missing "$fetch" "$(printf '0%.0s' {1..64})" "$gh" "$tmp/out" >/dev/null 2>&1; then
  check "checksum mismatch fails" 1
else
  check "checksum mismatch fails" 0
fi

# 6. Dockerfiles: no ADD of a GitHub release asset (it cannot carry the token), the token is only
# a secret mount, never an ARG or ENV.
bad=0
for f in "$root"/flavors/*/Dockerfile "$root"/hack/e2e-toolchain/*Dockerfile; do
  if grep -En '^ADD .*github\.com/[^ ]+/releases/download/' "$f" >/dev/null; then
    echo "  $f: ADD of a GitHub release asset; use koinon-fetch with the github_token secret"
    bad=1
  fi
  if grep -Eni '^(ARG|ENV) .*(github_token|GITHUB_TOKEN)' "$f" >/dev/null; then
    echo "  $f: the token must be a secret mount, not an ARG or ENV"
    bad=1
  fi
done
check "Dockerfiles use the secret, not ADD, ARG or ENV" "$bad"

exit "$fail"
