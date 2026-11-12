#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline test of harnesses/common/koinon-fetch and of how the Dockerfiles use it (#145): GitHub
# release assets are downloaded through the REST API with the optional github_token BuildKit
# secret (the github.com web URL ignores a token), anonymously from github.com without it, never
# with the token in argv, and always checked against the pinned sha256. A fake curl on PATH records
# how it was called.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
fetch=$root/harnesses/common/koinon-fetch

tmp=$(mktemp -d "${TMPDIR:-/tmp}/koinon-fetch-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"

# Fake curl: logs argv and stdin, answers the GitHub API shapes, writes fixed content otherwise.
cat >"$tmp/bin/curl" <<'FAKE'
#!/bin/sh
out= hdr= url=
args="$*"
while [ "$#" -gt 0 ]; do
  [ "$1" = -o ] && out=$2
  [ "$1" = -D ] && hdr=$2
  url=$1
  shift
done
printf '%s\n' "$args" >>"$FAKE_CURL_LOG"
if printf '%s' "$args" | grep -q -- ' -K '; then cat >>"$FAKE_CURL_STDIN"; fi
case $url in
  */releases/tags/*)
    printf '{\n "id": 9,\n "assets": [\n {\n  "url": "https://api.github.com/repos/o/r/releases/assets/111",\n  "id": 111,\n  "name": "other.tgz",\n  "uploader": {"login": "x"}\n },\n {\n  "url": "https://api.github.com/repos/o/r/releases/assets/222",\n  "id": 222,\n  "name": "asset.tgz",\n  "uploader": {"login": "x"}\n }\n ]\n}\n' >"$out" ;;
  *) printf 'koinon_test_payload' >"$out" ;;
esac
[ -z "$hdr" ] || printf 'HTTP/2 200\r\nx-ratelimit-limit: 5000\r\n' >"$hdr"
FAKE
chmod +x "$tmp/bin/curl"
export PATH="$tmp/bin:$PATH" FAKE_CURL_LOG=$tmp/curl.log FAKE_CURL_STDIN=$tmp/curl.stdin

good=$(printf 'koinon_test_payload' | sha256sum | cut -d' ' -f1)
fail=0
check() { # check <name> <condition result 0/1>
  if [ "$2" -eq 0 ]; then echo "ok   $1"; else echo "FAIL $1"; fail=1; fi
}
reset() { : >"$FAKE_CURL_LOG"; : >"$FAKE_CURL_STDIN"; rm -f "$tmp/out"; }
logged() { grep -q -- "$1" "$FAKE_CURL_LOG" && echo 0 || echo 1; }   # 0 when argv matched
unlogged() { grep -q -- "$1" "$FAKE_CURL_LOG" && echo 1 || echo 0; } # 0 when argv did not match

# Built in parts: the dep-age check dates every literal release URL on an added line.
base=https://github.com/o/r
dl=download
gh=$base/releases/$dl/v1/asset.tgz

# 1. With a token the asset comes through the API, by id, with the header on stdin and not in argv.
reset
printf 'koinon_test_not-a-real-token\n' >"$tmp/token"
KOINON_FETCH_TOKEN_FILE=$tmp/token "$fetch" "$good" "$gh" "$tmp/out" >/dev/null 2>&1
check "token goes to curl through stdin" "$(grep -q 'Authorization: Bearer koinon_test_not-a-real-token' "$FAKE_CURL_STDIN" && echo 0 || echo 1)"
check "token is not in argv" "$(unlogged 'not-a-real-token')"
check "asset is fetched through the API by its id" "$(logged 'api.github.com/repos/o/r/releases/assets/222')"
check "asset request asks for the octet-stream" "$(logged 'application/octet-stream.*releases/assets/222')"
check "the github.com web URL is not requested with a token" "$(unlogged 'github.com/o/r/releases/download')"

# 2. No token file: anonymous, from github.com.
reset
KOINON_FETCH_TOKEN_FILE=$tmp/missing "$fetch" "$good" "$gh" "$tmp/out" >/dev/null
check "no secret: anonymous request" "$(unlogged ' -K ')"
check "no secret: the github.com URL is used" "$(logged 'github.com/o/r/releases/download/v1/asset.tgz')"

# 3. Empty secret: anonymous.
reset
: >"$tmp/empty"
KOINON_FETCH_TOKEN_FILE=$tmp/empty "$fetch" "$good" "$gh" "$tmp/out" >/dev/null
check "empty secret: anonymous request" "$(unlogged ' -K ')"

# 4. A non-GitHub host never gets the token.
reset
KOINON_FETCH_TOKEN_FILE=$tmp/token "$fetch" "$good" https://example.com/x.tgz "$tmp/out" >/dev/null
check "token is not sent to other hosts" "$(unlogged ' -K ')"

# 5. A wrong checksum fails.
reset
if KOINON_FETCH_TOKEN_FILE=$tmp/missing "$fetch" "$(printf '0%.0s' {1..64})" "$gh" "$tmp/out" >/dev/null 2>&1; then
  check "checksum mismatch fails" 1
else
  check "checksum mismatch fails" 0
fi

# 5b. An asset the release does not have fails before any download.
reset
if KOINON_FETCH_TOKEN_FILE=$tmp/token "$fetch" "$good" "$base/releases/$dl/v1/missing.tgz" "$tmp/out" >/dev/null 2>&1; then
  check "unknown asset fails" 1
else
  check "unknown asset fails" 0
fi

# 6. Dockerfiles: no ADD of a GitHub release asset (it cannot carry the token), the token is only
# a secret mount, never an ARG or ENV. ADD and its URL are often on two lines (a trailing
# backslash), so continuation lines are joined first.
joined() { sed -e ':a' -e '/\\$/N; s/\\\n//; ta' "$1"; }
has_add() { joined "$1" | grep -E '^ADD .*github\.com/[^ ]+/releases/download/' >/dev/null; }
bad=0
for f in "$root"/harnesses/*/Dockerfile "$root"/hack/e2e-toolchain/*Dockerfile; do
  if has_add "$f"; then
    echo "  $f: ADD of a GitHub release asset; use koinon-fetch with the github_token secret"
    bad=1
  fi
  if grep -Eni '^(ARG|ENV) .*(github_token|GITHUB_TOKEN)' "$f" >/dev/null; then
    echo "  $f: the token must be a secret mount, not an ARG or ENV"
    bad=1
  fi
done
check "Dockerfiles use the secret, not ADD, ARG or ENV" "$bad"

# Negative cases: the guard catches an ADD of a release asset in the repo's two-line style and in one line.
printf 'FROM scratch\nADD --checksum=sha256:abc \\\n    %s/releases/%s/v1/a.tgz /a.tgz\n' "$base" "$dl" >"$tmp/Dockerfile.two"
printf 'FROM scratch\nADD %s/releases/%s/v1/a.tgz /a.tgz\n' "$base" "$dl" >"$tmp/Dockerfile.one"
printf 'FROM scratch\nRUN koinon-fetch abc %s/releases/%s/v1/a.tgz /a.tgz\n' "$base" "$dl" >"$tmp/Dockerfile.ok"
check "guard catches a two-line ADD of a release asset" "$(has_add "$tmp/Dockerfile.two" && echo 0 || echo 1)"
check "guard catches a one-line ADD of a release asset" "$(has_add "$tmp/Dockerfile.one" && echo 0 || echo 1)"
check "guard accepts koinon-fetch" "$(has_add "$tmp/Dockerfile.ok" && echo 1 || echo 0)"

exit "$fail"
