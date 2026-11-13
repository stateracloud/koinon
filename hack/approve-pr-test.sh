#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/approve-pr.sh against a fake API (python3): the login, the pending approval of the right repository,
# the args_sha256 it reads from GetApproval, and the refusals (no approval, a wrong login, a missing variable).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/approve-pr-test.XXXXXX")
server_pid=
trap '[[ -z $server_pid ]] || kill "$server_pid" 2>/dev/null || true; rm -rf "$tmp"' EXIT

cat >"$tmp/fake.py" <<'PY'
import http.server, json, sys

LOG = sys.argv[2]
APPROVALS = json.loads(sys.argv[3])

class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def reply(self, code, body, headers=()):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        for k, v in headers: self.send_header(k, v)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(n) or b"{}")
        with open(LOG, "a") as f: f.write(self.path + " " + json.dumps(body, sort_keys=True) + "\n")
        if self.path == "/auth/login":
            if body.get("password") != "koinon_test_not-a-real-password":
                return self.reply(401, {"error": "bad login"})
            return self.reply(200, {"passwordChangeRequired": False},
                              [("Set-Cookie", "__Host-koinon_session=fake-session-token; Path=/; Secure; HttpOnly")])
        # As strict as the server's http.ParseCookie: "a=b; c=d" only, no empty item (the jar has blank lines).
        items = self.headers.get("Cookie", "").split("; ")
        if "__Host-koinon_session=fake-session-token" not in items or "" in items:
            return self.reply(401, {"code": "unauthenticated"})
        if self.headers.get("Connect-Protocol-Version") != "1":
            return self.reply(403, {"code": "csrf"})
        if self.path.endswith("/ListApprovals"):
            return self.reply(200, {"approvals": [{k: v for k, v in a.items() if k != "argsSha256"} for a in APPROVALS]})
        if self.path.endswith("/GetApproval"):
            for a in APPROVALS:
                if a["id"] == body["approvalId"]: return self.reply(200, {"approval": a})
        if self.path.endswith("/Approve"):
            for a in APPROVALS:
                if a["id"] == body["approvalId"] and a["argsSha256"] == body["argsSha256"]:
                    return self.reply(200, {"approval": {**a, "status": "STATUS_EXECUTED"}})
            return self.reply(409, {"code": "aborted"})
        return self.reply(404, {})

srv = http.server.HTTPServer(("127.0.0.1", 0), H)
open(sys.argv[1], "w").write(str(srv.server_address[1]))
srv.serve_forever()
PY

start() { # start <approvals-json>
  : >"$tmp/calls"
  rm -f "$tmp/port"
  python3 "$tmp/fake.py" "$tmp/port" "$tmp/calls" "$1" &
  server_pid=$!
  for _ in $(seq 50); do [[ -s $tmp/port ]] && break; sleep 0.1; done
  [[ -s $tmp/port ]] || { echo "the fake API did not start" >&2; exit 1; }
}
stop() { kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; server_pid=; }

fail=0
check() { if grep -q -- "$2" <<<"$1"; then echo "ok: $3"; else echo "FAIL: $3 (missing '$2')" >&2; fail=1; fi; }
nocheck() { if grep -q -- "$2" <<<"$1"; then echo "FAIL: $3 (found '$2')" >&2; fail=1; else echo "ok: $3"; fi; }
approver() { # approver <expected-exit> <args...>; the output is in $out
  local want=$1 rc=0
  shift
  out=$("$repo/hack/approve-pr.sh" "http://127.0.0.1:$(cat "$tmp/port")" "$tmp/jar" org1 "$@" 2>&1) || rc=$?
  [[ $rc == "$want" ]] || { echo "FAIL: exit $rc, want $want: $out" >&2; fail=1; }
}

export KOINON_APPROVER_USER=drill-admin KOINON_APPROVER_PASSWORD=koinon_test_not-a-real-password
two='[{"id":"a-other","target":"github:acme/other","status":"STATUS_PENDING","argsSha256":"sha-other"},
      {"id":"a-done","target":"github:acme/widgets","status":"STATUS_EXECUTED","argsSha256":"sha-done"},
      {"id":"a-want","target":"github:acme/widgets","status":"STATUS_PENDING","argsSha256":"sha-want"}]'
start "$two"
approver 0 acme/widgets
check "$out" "approved a-want" "the pending approval of the named repository is approved"
check "$(cat "$tmp/calls")" '"argsSha256": "sha-want"' "Approve carries the hash GetApproval showed"
nocheck "$(cat "$tmp/calls")" 'a-other' "another repository's approval is not touched"
nocheck "$(cat "$tmp/calls")" 'a-done' "a decided approval is not touched"
check "$(stat -c %a "$tmp/jar" 2>/dev/null || stat -f %Lp "$tmp/jar")" "^600$" "the cookie jar is private (0600)"
stop

start '[{"id":"a-other","target":"github:acme/other","status":"STATUS_PENDING","argsSha256":"x"}]'
approver 3 acme/widgets
check "$out" "no pending approval for github:acme/widgets" "no pending approval is exit 3, and says which"
stop

start "$two"
KOINON_APPROVER_PASSWORD=koinon_test_wrong approver 1 acme/widgets
check "$out" "login as drill-admin: HTTP 401" "a refused login is exit 1"
stop

# Gitea targets (#967): same checks, and a github approval of the same name is never taken for it.
both='[{"id":"a-gh","target":"github:acme/widgets","status":"STATUS_PENDING","argsSha256":"sha-gh"},
       {"id":"a-gt","target":"gitea:acme/widgets","status":"STATUS_PENDING","argsSha256":"sha-gt"}]'
start "$both"
approver 0 gitea:acme/widgets
check "$out" "approved a-gt (gitea:acme/widgets)" "a gitea target approves the gitea approval"
check "$(cat "$tmp/calls")" '"argsSha256": "sha-gt"' "Approve carries the gitea approval's hash"
nocheck "$(cat "$tmp/calls")" 'a-gh' "the github approval of the same name is not touched"
stop

start "$both"
approver 0 acme/widgets
check "$out" "approved a-gh (github:acme/widgets)" "a bare owner/name still means github"
nocheck "$(cat "$tmp/calls")" 'a-gt' "a bare owner/name does not touch the gitea approval"
stop

start '[{"id":"a-gh","target":"github:acme/widgets","status":"STATUS_PENDING","argsSha256":"x"}]'
approver 3 gitea:acme/widgets
check "$out" "no pending approval for gitea:acme/widgets" "a gitea target without a gitea approval is exit 3"
stop

approver 2 gitlab:acme/widgets
check "$out" "must be github or gitea" "another connector is a usage error"
approver 2 gitea:widgets
check "$out" "repo must be owner/name" "a gitea target without an owner is a usage error"

rc=0
usage=$("$repo/hack/approve-pr.sh" only-one-arg 2>&1) || rc=$?
[[ $rc == 2 ]] || { echo "FAIL: the wrong argument count exits $rc, want 2" >&2; fail=1; }
check "$usage" "usage:" "the wrong argument count prints the usage"
rc=0
noenv=$(env -u KOINON_APPROVER_USER "$repo/hack/approve-pr.sh" http://x j o a/b 2>&1) || rc=$?
[[ $rc == 2 ]] || { echo "FAIL: a missing login variable exits $rc, want 2" >&2; fail=1; }
check "$noenv" "KOINON_APPROVER_USER" "a missing login variable is named"

exit "$fail"
