#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Spike Gitea setup (docs/spikes/openshell.md, "Gitea setup"): org koinon-spike,
# repo koinon-spike/hello with a protected main, and a non-admin bot with write
# access to that repo only. The bot's token goes straight into OpenBao at
# secret/koinon-spike/gitea-bot and is never printed. Safe to re-run: a re-run
# replaces the bot's token.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

GITEA=https://git.example.invalid/api/v1
ORG=koinon-spike
REPO=hello
BOT=koinon-spike-bot
export BAO_ADDR=https://secrets.example.invalid
BAO_TOKEN=${BAO_TOKEN:?set BAO_TOKEN}
export BAO_TOKEN
ADMIN=${GITEA_ADMIN_PASSWORD:?set GITEA_ADMIN_PASSWORD}

# api METHOD PATH [JSON] prints the HTTP status; the body goes to $body. Statuses are
# captured in $st first: bash 3.2 (macOS) mangles escaped quotes inside "$(...)".
body=$(mktemp "${TMPDIR:-/tmp}/gitea-setup.XXXXXX")
trap 'rm -f "$body"' EXIT
api() {
  local data=()
  if [[ $# -ge 3 ]]; then data=(--data "$3"); fi
  curl -sS -o "$body" -w '%{http_code}' -X "$1" -H "Authorization: token $ADMIN" \
    -H 'Content-Type: application/json' "$GITEA$2" "${data[@]}"
}
expect() { # expect DESC STATUS OK...
  local desc=$1 got=$2 ok
  shift 2
  for ok in "$@"; do
    if [[ $got == "$ok" ]]; then echo "ok   $desc ($got)"; return; fi
  done
  echo "FAIL $desc ($got): $(head -c 300 "$body")" >&2
  exit 1
}

st=$(api POST /orgs "{\"username\":\"$ORG\",\"visibility\":\"private\"}")
expect "org $ORG" "$st" 201 422
st=$(api POST "/orgs/$ORG/repos" "{\"name\":\"$REPO\",\"private\":true,\"auto_init\":true,\"default_branch\":\"main\"}")
expect "repo $ORG/$REPO" "$st" 201 409
st=$(api POST "/repos/$ORG/$REPO/branch_protections" '{"rule_name":"main","enable_push":false}')
expect "protect main" "$st" 201 403 422

password=$(openssl rand -base64 32)
st=$(api POST /admin/users "{\"username\":\"$BOT\",\"email\":\"$BOT@koinon.invalid\",\"password\":\"$password\",\"must_change_password\":false,\"visibility\":\"private\"}")
expect "user $BOT" "$st" 201 409 422
st=$(api PATCH "/admin/users/$BOT" "{\"login_name\":\"$BOT\",\"source_id\":0,\"password\":\"$password\",\"must_change_password\":false,\"admin\":false,\"restricted\":true}")
expect "reset $BOT password, restricted, not admin" "$st" 200
st=$(api PUT "/repos/$ORG/$REPO/collaborators/$BOT" '{"permission":"write"}')
expect "$BOT writes $ORG/$REPO" "$st" 204

# Replace the bot's spike token. Token management needs the bot's own basic auth.
curl -sS -o /dev/null -u "$BOT:$password" -X DELETE "$GITEA/users/$BOT/tokens/spike" || true
curl -sS -f -u "$BOT:$password" -H 'Content-Type: application/json' -X POST "$GITEA/users/$BOT/tokens" \
  --data '{"name":"spike","scopes":["write:repository","read:user"]}' |
  python3 -c 'import json,sys; sys.stdout.write(json.load(sys.stdin)["sha1"])' |
  bao kv put -mount=secret koinon-spike/gitea-bot token=- >/dev/null
echo "ok   bot token stored at secret/koinon-spike/gitea-bot"
