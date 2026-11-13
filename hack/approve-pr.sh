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
#   repo        [github:|gitea:]owner/name; the pending approval whose target is <connector>:<owner>/<name>
#               (the target the code-host connector shows; a bare owner/name means github:)
# The login comes from the environment, never from an argument (an argument shows in the process list):
#   KOINON_APPROVER_USER and KOINON_APPROVER_PASSWORD.
# Nothing secret is on a command line: the login body goes to curl on stdin and the Cookie header through a 0600 file.
# The approval list is read as one page. Exit: 0 approved, 1 an API refusal, 2 usage, 3 no pending approval.
# With KOINON_APPROVER_WAIT=<seconds> the script waits that long for the approval to appear (the run parks on it
# some time after the task starts); default 0 (look once).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: ${0##*/} <base-url> <cookie-jar> <org-id> <owner/name>" >&2
  exit 2
fi
# shellcheck disable=SC2034 # base and jar are read by hack/lib/approver-session.sh
base=${1%/} jar=$2 org=$3 target=$4
case $target in
  github:* | gitea:*) ;;
  *:*) echo "the connector of the target must be github or gitea, got $target" >&2; exit 2 ;;
  *) target=github:$target ;;
esac
repo=${target#*:}
[[ $repo =~ ^[^/:[:space:]]+/[^/:[:space:]]+$ ]] || { echo "repo must be owner/name, got $repo" >&2; exit 2; }

# shellcheck source=/dev/null
source "$(dirname "${BASH_SOURCE[0]}")/lib/approver-session.sh"
approver_check_env
wait_s=$(approver_wait_seconds)

approver_login

svc=koinon.approval.v1alpha1.ApprovalService

deadline=$((SECONDS + wait_s))
id=
while :; do
  list=$(rpc "$svc" ListApprovals "$(jq -n --arg o "$org" '{orgId: $o}')") || exit 1
  id=$(jq -r --arg t "$target" '[.approvals[]? | select(.target == $t and (.status == "STATUS_PENDING"))] | first | .id // empty' <<<"$list")
  [[ -n $id ]] && break
  if ((SECONDS >= deadline)); then
    echo "no pending approval for $target in org $org" >&2
    exit 3
  fi
  sleep 3
done

got=$(rpc "$svc" GetApproval "$(jq -n --arg o "$org" --arg a "$id" '{orgId: $o, approvalId: $a}')") || exit 1
sha=$(jq -r '.approval.argsSha256 // empty' <<<"$got")
[[ -n $sha ]] || { echo "approval $id has no args_sha256" >&2; exit 1; }
done_=$(rpc "$svc" Approve "$(jq -n --arg o "$org" --arg a "$id" --arg s "$sha" '{orgId: $o, approvalId: $a, argsSha256: $s, note: "approved by hack/approve-pr.sh"}')") || exit 1
echo "approved $id ($target): $(jq -r '.approval.status // "?"' <<<"$done_")"
