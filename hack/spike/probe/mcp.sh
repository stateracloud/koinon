#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Minimal MCP Streamable HTTP client for the spike: mcp.sh <method> [params-json]
# Keeps the session id in /tmp/mcp-session. Uses $OPENAI_API_KEY (an OpenShell placeholder).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
URL=${MCP_URL:-http://agentgateway.agentgateway.svc.cluster.local:8080/mcp}
method=$1 params=${2:-"{}"}
id=$RANDOM
hdr=(-H "Authorization: Bearer $OPENAI_API_KEY" -H 'Content-Type: application/json'
     -H 'Accept: application/json, text/event-stream' -H 'MCP-Protocol-Version: 2025-11-25')
if [[ $method == initialize ]]; then
  body='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"spike-probe","version":"0"}}}'
  curl -s -D /tmp/mcp-h -o /tmp/mcp-b "${hdr[@]}" -d "$body" "$URL"
  grep -i '^mcp-session-id:' /tmp/mcp-h | awk '{print $2}' | tr -d '\r' > /tmp/mcp-session
  echo "status $(head -1 /tmp/mcp-h | tr -d '\r') session $(cat /tmp/mcp-session)"
  curl -s -o /dev/null "${hdr[@]}" -H "Mcp-Session-Id: $(cat /tmp/mcp-session)" \
    -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' "$URL"
  exit 0
fi
curl -s -w '\nhttp=%{http_code}\n' "${hdr[@]}" -H "Mcp-Session-Id: $(cat /tmp/mcp-session)" \
  -d "{\"jsonrpc\":\"2.0\",\"id\":$id,\"method\":\"$method\",\"params\":$params}" "$URL" |
  sed -n 's/^data: //p; /^http=/p; /^{/p'
