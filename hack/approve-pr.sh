#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Scripted approver (#927): sign in as an org admin or owner through /auth/login, find the pending approval for
# a repository, read it (GetApproval) and approve it with the hash it shows (Approve with args_sha256). For the
# runs that need a pull request approved without a person at the web UI (the lab runs of #51 and #74), and for
# any script that stands in for the approver. Approve accepts only admins and owners (ADR 0049).
#
# Usage: hack/approve-pr.sh <base-url> <cookie-jar> <org-id> <repo>
#   base-url    the API, for example http://localhost:8080 (a port-forward) or https://koinon.example.invalid
#   cookie-jar  a file the script writes the session cookie to (mode 0600) and reads it back from
#   org-id      the org's ID (not its slug)
#   repo        owner/name; the pending approval whose target is github:<owner>/<name>
# The login comes from the environment, never from an argument (an argument shows in the process list):
#   KOINON_APPROVER_USER and KOINON_APPROVER_PASSWORD.
# Exit: 0 approved (or already approved with the same hash), 1 an API refusal, 2 usage, 3 no pending approval.
# With KOINON_APPROVER_WAIT=<seconds> the script waits that long for the approval to appear (the run parks on it
# some time after the task starts); default 0 (look once).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: ${0##*/} <base-url> <cookie-jar> <org-id> <owner/name>" >&2
  exit 2
fi
base=${1%/} jar=$2 org=$3 repo=$4
[[ -n ${KOINON_APPROVER_USER:-} && -n ${KOINON_APPROVER_PASSWORD:-} ]] || { echo "set KOINON_APPROVER_USER and KOINON_APPROVER_PASSWORD" >&2; exit 2; }
[[ $repo == */* ]] || { echo "repo must be owner/name, got $repo" >&2; exit 2; }
wait_s=${KOINON_APPROVER_WAIT:-0}
[[ $wait_s =~ ^[0-9]+$ ]] || { echo "KOINON_APPROVER_WAIT must be a number of seconds" >&2; exit 2; }
for tool in curl jq; do command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 2; }; done

svc=$base/koinon.approval.v1alpha1.ApprovalService

# cookie_header builds the Cookie header from the jar. curl's own jar handling would not send the Secure
# __Host- session cookie over the plain-HTTP port-forward the drill uses.
cookie_header() {
  awk -F'\t' '/^#HttpOnly_/ || !/^#/ { if (NF >= 7) printf "%s%s=%s", sep, $6, $7; sep="; " }' "$jar"
}

# rpc <Method> <json>: a Connect unary call with the session; prints the body. The Connect-Protocol-Version
# header is the CSRF guard of cookie sessions (ADR 0022). A non-200 answer prints the body and fails.
rpc() {
  local out status
  out=$(mktemp "${TMPDIR:-/tmp}/approve-pr.XXXXXX")
  status=$(curl -sS -o "$out" -w '%{http_code}' -X POST "$svc/$1" \
    -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' -H "Cookie: $(cookie_header)" -d "$2")
  if [[ $status != 200 ]]; then
    echo "$1: HTTP $status: $(cat "$out")" >&2
    rm -f "$out"
    return 1
  fi
  cat "$out"
  rm -f "$out"
}

umask 077
: >"$jar"
login=$(jq -n --arg u "$KOINON_APPROVER_USER" --arg p "$KOINON_APPROVER_PASSWORD" '{username: $u, password: $p}')
status=$(curl -sS -o /dev/null -w '%{http_code}' -c "$jar" -X POST "$base/auth/login" -H 'Content-Type: application/json' -d "$login")
[[ $status == 200 ]] || { echo "login as $KOINON_APPROVER_USER: HTTP $status" >&2; exit 1; }
grep -q 'session' "$jar" || { echo "login answered 200 but set no session cookie" >&2; exit 1; }

deadline=$((SECONDS + wait_s))
id=
while :; do
  list=$(rpc ListApprovals "$(jq -n --arg o "$org" '{orgId: $o}')") || exit 1
  id=$(jq -r --arg t "github:$repo" '[.approvals[]? | select(.target == $t and (.status == "STATUS_PENDING"))] | first | .id // empty' <<<"$list")
  [[ -n $id ]] && break
  if ((SECONDS >= deadline)); then
    echo "no pending approval for github:$repo in org $org" >&2
    exit 3
  fi
  sleep 3
done

got=$(rpc GetApproval "$(jq -n --arg o "$org" --arg a "$id" '{orgId: $o, approvalId: $a}')") || exit 1
sha=$(jq -r '.approval.argsSha256 // empty' <<<"$got")
[[ -n $sha ]] || { echo "approval $id has no args_sha256" >&2; exit 1; }
done_=$(rpc Approve "$(jq -n --arg o "$org" --arg a "$id" --arg s "$sha" '{orgId: $o, approvalId: $a, argsSha256: $s, note: "approved by hack/approve-pr.sh"}')") || exit 1
echo "approved $id (github:$repo): $(jq -r '.approval.status // "?"' <<<"$done_")"
