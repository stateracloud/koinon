#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The escape suite's leak guard exfiltration case (test/escape/exfil_test.go, label leakguard) against the
# quickstart install that test/e2e/run.sh made. The Go driver calls it from step 6b (KOINON_E2E_ESCAPE_HOOK,
# ESCAPE_MCP=1) while write_item and fetch_url are auto, with KOINON_ESCAPE_MCP_HOST, _PORT, _KEY and
# _RECEIVER_URL set; the case fails when the guard lets either tool reach the fake server (#348).
# test/escape/kind.sh supplies the OpenShell endpoint, client certificate and probe image.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

: "${CLUSTER:?CLUSTER names the kind cluster of the install}"
for v in KOINON_ESCAPE_MCP_HOST KOINON_ESCAPE_MCP_KEY KOINON_ESCAPE_MCP_RECEIVER_URL; do
  [[ -n ${!v:-} ]] || { echo "$v is not set" >&2; exit 2; }
done
root="$(cd "$(dirname "$0")/../.." && pwd)"

# The probe sandbox is deleted by the Go suite; the workspace it ran in (KOINON_OPENSHELL_WORKSPACE, default
# koinon-escape) and its namespace stay. Remove them when the hook ends, success or failure, and wait until the
# namespace is gone, so no later step of the run finds the probe's leftovers.
ws="${KOINON_OPENSHELL_WORKSPACE:-koinon-escape}"
cleanup() {
  local rc=$? kc=(kubectl --context "kind-$CLUSTER")
  "${kc[@]}" delete ns -l "openshell.ai/managed-by=openshell,openshell.ai/sandbox-workspace=$ws" \
    --wait=true --timeout=120s --ignore-not-found >&2 || echo "could not remove the namespace of workspace $ws" >&2
  exit "$rc"
}
trap cleanup EXIT

# No stand-ins: the guarded gateway and the fake server are the install's own.
CLUSTER="$CLUSTER" KOINON_ESCAPE_STANDIN=0 KOINON_ESCAPE_LABEL=leakguard KOINON_REQUIRE=escape-mcp-gateway \
  "$root/test/escape/kind.sh"
