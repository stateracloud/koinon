#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Tests hack/answer-question.sh against a fake API (python3): the login, the open question of the right run and ID,
# a choice that must be one of the offered choices, free text, and the refusals (no such question, a question of
# another run, a decided question, a wrong choice, a missing answer, a wrong login).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/answer-question-test.XXXXXX")
server_pid=
trap '[[ -z $server_pid ]] || kill "$server_pid" 2>/dev/null || true; rm -rf "$tmp"' EXIT

cat >"$tmp/fake.py" <<'PY'
import http.server, json, sys

LOG = sys.argv[2]
QUESTIONS = json.loads(sys.argv[3])

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
        if self.path == "/koinon.question.v1alpha1.QuestionService/ListQuestions":
            return self.reply(200, {"questions": QUESTIONS})
        if self.path == "/koinon.question.v1alpha1.QuestionService/AnswerQuestion":
            for q in QUESTIONS:
                if q["id"] == body["questionId"] and q["state"] == "QUESTION_STATE_OPEN":
                    return self.reply(200, {"question": {**q, "state": "QUESTION_STATE_ANSWERED"}})
            return self.reply(409, {"code": "aborted"})
        return self.reply(404, {})

srv = http.server.HTTPServer(("127.0.0.1", 0), H)
open(sys.argv[1], "w").write(str(srv.server_address[1]))
srv.serve_forever()
PY

start() { # start <questions-json>
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
answerer() { # answerer <expected-exit> <run-id> <question-id>; the output is in $out
  local want=$1 rc=0
  out=$("$repo/hack/answer-question.sh" "http://127.0.0.1:$(cat "$tmp/port")" "$tmp/jar" org1 "$2" "$3" 2>&1) || rc=$?
  [[ $rc == "$want" ]] || { echo "FAIL: exit $rc, want $want: $out" >&2; fail=1; }
}

export KOINON_APPROVER_USER=drill-admin KOINON_APPROVER_PASSWORD=koinon_test_not-a-real-password
qs='[{"id":"q-other","runId":"run-2","state":"QUESTION_STATE_OPEN","choices":["Yes","No"]},
     {"id":"q-done","runId":"run-1","state":"QUESTION_STATE_ANSWERED","choices":["Yes","No"]},
     {"id":"q-want","runId":"run-1","state":"QUESTION_STATE_OPEN","choices":["Open the pull request","Skip"]}]'

start "$qs"
KOINON_ANSWER_CHOICE="Open the pull request" answerer 0 run-1 q-want
check "$out" "answered q-want of run run-1: QUESTION_STATE_ANSWERED" "the open question of the run is answered"
check "$(cat "$tmp/calls")" '"choice": "Open the pull request"' "the plan's choice is what is sent"
nocheck "$(cat "$tmp/calls")" 'q-other' "another run's question is not touched"
check "$(stat -c %a "$tmp/jar" 2>/dev/null || stat -f %Lp "$tmp/jar")" "^600$" "the cookie jar is private (0600)"
stop

start "$qs"
KOINON_ANSWER_TEXT="Use the lockfile only." answerer 0 run-1 q-want
check "$(cat "$tmp/calls")" '"text": "Use the lockfile only."' "free text is sent as text"
stop

start "$qs"
KOINON_ANSWER_CHOICE="Maybe" answerer 1 run-1 q-want
check "$out" "has no such choice" "a text that is not a choice is refused"
nocheck "$(cat "$tmp/calls")" 'AnswerQuestion' "a refused choice sends no answer"
stop

start "$qs"
KOINON_ANSWER_CHOICE="Yes" answerer 3 run-1 q-other
check "$out" "no open question q-other of run run-1" "a question of another run is exit 3"
KOINON_ANSWER_CHOICE="Yes" answerer 3 run-1 q-done
check "$out" "no open question q-done" "a decided question is exit 3"
nocheck "$(cat "$tmp/calls")" 'AnswerQuestion' "no answer is sent for a question that is not open"
stop

start "$qs"
KOINON_ANSWER_CHOICE=Yes KOINON_APPROVER_PASSWORD=koinon_test_wrong answerer 1 run-1 q-want
check "$out" "login as drill-admin: HTTP 401" "a refused login is exit 1"
stop

rc=0
usage=$("$repo/hack/answer-question.sh" only-one-arg 2>&1) || rc=$?
[[ $rc == 2 ]] || { echo "FAIL: the wrong argument count exits $rc, want 2" >&2; fail=1; }
check "$usage" "usage:" "the wrong argument count prints the usage"
rc=0
noans=$("$repo/hack/answer-question.sh" http://x j o r q 2>&1) || rc=$?
[[ $rc == 2 ]] || { echo "FAIL: no answer exits $rc, want 2" >&2; fail=1; }
check "$noans" "exactly one of" "a missing answer is named"
rc=0
twoans=$(KOINON_ANSWER_CHOICE=a KOINON_ANSWER_TEXT=b "$repo/hack/answer-question.sh" http://x j o r q 2>&1) || rc=$?
[[ $rc == 2 ]] || { echo "FAIL: two answers exit $rc, want 2" >&2; fail=1; }
check "$twoans" "exactly one of" "two answers are refused"
rc=0
noenv=$(env -u KOINON_APPROVER_USER KOINON_ANSWER_TEXT=b "$repo/hack/answer-question.sh" http://x j o r q 2>&1) || rc=$?
[[ $rc == 2 ]] || { echo "FAIL: a missing login variable exits $rc, want 2" >&2; fail=1; }
check "$noenv" "KOINON_APPROVER_USER" "a missing login variable is named"

exit "$fail"
