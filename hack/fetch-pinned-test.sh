#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# hack/fetch-pinned.sh (#431): downloads, checks the sha256, skips a file that is already there,
# retries on HTTP 504, and never keeps a file with the wrong sha256.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
fp="$root/hack/fetch-pinned.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/fetch-pinned-test.XXXXXX")
srv=
cleanup() { [ -z "$srv" ] || kill "$srv" 2>/dev/null || true; rm -rf "$tmp"; }
trap cleanup EXIT
fail() { echo "fetch-pinned-test: $*" >&2; exit 1; }

printf 'hello pinned\n' >"$tmp/src"
if command -v sha256sum >/dev/null 2>&1; then good=$(sha256sum "$tmp/src" | cut -d' ' -f1); else good=$(shasum -a 256 "$tmp/src" | cut -d' ' -f1); fi
bad=$(printf '0%.0s' {1..64})
export FETCH_PINNED_RETRY_DELAY=0

# 1. fetch and verify
"$fp" "file://$tmp/src" "$good" "$tmp/out/a" >/dev/null
cmp -s "$tmp/src" "$tmp/out/a" || fail "file not written"

# 2. already present with the right sha256: the URL is not used
"$fp" "file://$tmp/does-not-exist" "$good" "$tmp/out/a" >/dev/null || fail "present file was fetched again"

# 3. wrong sha256: fails, keeps nothing, leaves no temp file
if "$fp" "file://$tmp/src" "$bad" "$tmp/out/b" 2>/dev/null; then fail "wrong sha256 accepted"; fi
[ ! -e "$tmp/out/b" ] || fail "wrong-sha256 download kept a file"
[ "$(find "$tmp/out" -type f | wc -l)" -eq 1 ] || fail "wrong-sha256 download left a temp file"

# 4. a stale file with the wrong content is replaced
printf 'stale\n' >"$tmp/out/c"
"$fp" "file://$tmp/src" "$good" "$tmp/out/c" >/dev/null
cmp -s "$tmp/src" "$tmp/out/c" || fail "stale file not replaced"

# 5. HTTP 504 twice, then 200: the retry gets through
if command -v python3 >/dev/null 2>&1; then
	cat >"$tmp/srv.py" <<'PY'
import http.server, sys
hits = {"n": 0}
body = open(sys.argv[1], "rb").read()
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        hits["n"] += 1
        if hits["n"] <= 2:
            self.send_response(504); self.send_header("Content-Length", "0"); self.end_headers(); return
        self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
s = http.server.HTTPServer(("127.0.0.1", 0), H)
print(s.server_port, flush=True)
s.serve_forever()
PY
	python3 -I "$tmp/srv.py" "$tmp/src" >"$tmp/port" &
	srv=$!
	for _ in $(seq 50); do [ -s "$tmp/port" ] && break; sleep 0.1; done
	"$fp" "http://127.0.0.1:$(cat "$tmp/port")/x" "$good" "$tmp/out/d" >/dev/null || fail "retry on 504 did not recover"
	cmp -s "$tmp/src" "$tmp/out/d" || fail "retried file wrong"
else
	echo "fetch-pinned-test: python3 missing, skipped the 504 retry case" >&2
fi
echo "fetch-pinned-test: ok"
