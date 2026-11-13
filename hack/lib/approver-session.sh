#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Sourced by hack/approve-pr.sh and hack/answer-question.sh: the session of a scripted approver (#927, #967).
# The caller sets base (the API URL without a trailing slash) and jar (the cookie jar file), then calls
# approver_login and rpc. The login comes from KOINON_APPROVER_USER and KOINON_APPROVER_PASSWORD, never from
# an argument (an argument shows in the process list); the login body goes to curl on stdin and the Cookie
# header through a 0600 file.

# shellcheck disable=SC2154 # base and jar are set by the sourcing script
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

# approver_check_env exits 2 when the login variables or a tool are missing.
approver_check_env() {
  [[ -n ${KOINON_APPROVER_USER:-} && -n ${KOINON_APPROVER_PASSWORD:-} ]] || { echo "set KOINON_APPROVER_USER and KOINON_APPROVER_PASSWORD" >&2; exit 2; }
  local tool
  for tool in curl jq; do command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 2; }; done
}

# approver_wait_seconds prints KOINON_APPROVER_WAIT (default 0) and exits 2 when it is not a number.
approver_wait_seconds() {
  local w=${KOINON_APPROVER_WAIT:-0}
  [[ $w =~ ^[0-9]+$ ]] || { echo "KOINON_APPROVER_WAIT must be a number of seconds" >&2; exit 2; }
  echo "$w"
}

# cookie_header builds the Cookie header from the jar. curl's own jar handling would not send the Secure
# __Host- session cookie over the plain-HTTP port-forward the drill uses. A blank line of the jar must not
# add a separator: a header that starts with "; " is not one cookie list, and the real server ignores it (#967).
cookie_header() {
  awk -F'\t' 'BEGIN { printf "Cookie: " } /^#HttpOnly_/ || !/^#/ { if (NF >= 7) { printf "%s%s=%s", sep, $6, $7; sep="; " } }' "$jar"
}

# rpc <Service> <Method> <json>: a Connect unary call with the session, for example
# `rpc koinon.approval.v1alpha1.ApprovalService GetApproval '{}'`; prints the body. The Connect-Protocol-Version
# header is the CSRF guard of cookie sessions (ADR 0022). A non-200 answer prints the body and fails.
rpc() {
  local out hdr status
  out=$(mktemp "${TMPDIR:-/tmp}/approver-rpc.XXXXXX")
  hdr=$(mktemp "${TMPDIR:-/tmp}/approver-rpc-hdr.XXXXXX") # the session cookie, not on the command line
  cookie_header >"$hdr"
  status=$(curl -sS -o "$out" -w '%{http_code}' -X POST "$base/$1/$2" \
    -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' -H "@$hdr" --data-binary @- <<<"$3")
  rm -f "$hdr"
  if [[ $status != 200 ]]; then
    echo "$2: HTTP $status: $(cat "$out")" >&2
    rm -f "$out"
    return 1
  fi
  cat "$out"
  rm -f "$out"
}

# approver_login signs in and writes the session cookie to the jar (mode 0600). Exit 1 on a refusal.
approver_login() {
  umask 077
  : >"$jar"
  local status
  # jq reads the login from its environment and curl reads the body from stdin: neither has it in its arguments.
  status=$(jq -n '{username: env.KOINON_APPROVER_USER, password: env.KOINON_APPROVER_PASSWORD}' |
    curl -sS -o /dev/null -w '%{http_code}' -c "$jar" -X POST "$base/auth/login" -H 'Content-Type: application/json' --data-binary @-)
  [[ $status == 200 ]] || { echo "login as $KOINON_APPROVER_USER: HTTP $status" >&2; exit 1; }
  grep -q 'session' "$jar" || { echo "login answered 200 but set no session cookie" >&2; exit 1; }
}
