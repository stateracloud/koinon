#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The escape suite's OpenShell cases (test/escape, label openshell, without the Gitea read grant cases and the leak guard exfil spec, which escape-mcp.sh runs) against
# the quickstart install that test/e2e/run.sh made, so the e2e proves that the chart's real sandbox admission
# policy (charts/koinon/templates/sandbox-policy.yaml, the supervisor ServiceAccount token rules of #220
# included) admits the pods OpenShell and the Agent Sandbox controller create, and that the cases the suite
# tries (egress, platform services, credentials, a sandbox next to a sandbox, admission dry runs as the
# controller and the gateway) are refused (#109). run.sh calls it after the Go driver, for SUITE=go
# (ESCAPE_OPENSHELL=0 leaves it out). Needs the cluster: CLUSTER names the kind cluster.
#
# test/escape/kind.sh does the work: probe image, the gateway stand-in in koinon-fixtures, the port-forward and
# client certificate of the install's OpenShell. Differences from `make test-escape-kind` (a bare OpenShell
# without the chart): the live admission case is on and must not skip, because the namespace of the
# workspace is under the chart's policy, and the Gitea read grant cases are out (kind.sh has the reason;
# `make test-escape-kind` keeps them). A skip of a case this run needs is a failure (KOINON_REQUIRE).
# Added wall clock on the e2e: about 4 to 8 minutes (probe image build, ko stand-in, a dozen sandboxes), and
# the printed `-- escape suite (OpenShell cases)` line says the real number.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

: "${CLUSTER:?CLUSTER names the kind cluster of the install}"
root="$(cd "$(dirname "$0")/../.." && pwd)"
ns=${KOINON_NAMESPACE:-koinon}
kc=(kubectl --context "kind-$CLUSTER")

# The chart's policy must be there before the suite starts: without it every case would pass on a cluster
# that enforces nothing, and the failure would be a vague one in the middle of the Go output.
"${kc[@]}" get validatingadmissionpolicy "$ns-koinon-sandbox-pods" >/dev/null 2>&1 ||
  { echo "escape suite: ValidatingAdmissionPolicy $ns-koinon-sandbox-pods is missing in kind-$CLUSTER; the install must have sandboxPolicy.enabled (test/e2e/kind/koinon-values.yaml)" >&2; exit 1; }
bindings=$("${kc[@]}" get validatingadmissionpolicybindings -o json |
  jq -r '[.items[] | select(.spec.policyName == "'"$ns-koinon-sandbox-pods"'")] | length')
(( bindings > 0 )) ||
  { echo "escape suite: no ValidatingAdmissionPolicyBinding for $ns-koinon-sandbox-pods in kind-$CLUSTER" >&2; exit 1; }

# The probe sandboxes are deleted by the Go suite; the workspace they ran in (KOINON_OPENSHELL_WORKSPACE, default
# koinon-escape-openshell) and its namespace stay, because the live admission case needs the namespace. Remove
# them when the script ends, success or failure, and wait until the namespace is gone, so no later step finds
# them. The workspace is this script's own: escape-mcp.sh (step 6b) deletes the namespace of its workspace, and
# a shared workspace would leave the OpenShell record without a namespace here.
ws="${KOINON_OPENSHELL_WORKSPACE:-koinon-escape-openshell}"
cleanup() {
  local rc=$?
  "${kc[@]}" delete ns -l "openshell.ai/managed-by=openshell,openshell.ai/sandbox-workspace=$ws" \
    --wait=true --timeout=120s --ignore-not-found >&2 || echo "could not remove the namespace of workspace $ws" >&2
  exit "$rc"
}
trap cleanup EXIT

echo "escape suite (OpenShell cases) against the chart install in kind-$CLUSTER: policy $ns-koinon-sandbox-pods, $bindings binding(s)"
CLUSTER="$CLUSTER" \
KOINON_OPENSHELL_WORKSPACE="$ws" \
KOINON_ESCAPE_CHART_NAMESPACE="$ns" \
KOINON_ESCAPE_GITEA_STANDIN=0 \
KOINON_ESCAPE_LABEL="openshell && !gitea && !leakguard" \
KOINON_REQUIRE=openshell,cluster,escape-gateway \
  "$root/test/escape/kind.sh"
