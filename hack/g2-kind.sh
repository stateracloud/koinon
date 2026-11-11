#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# G2 gateway suite on kind (plan item G2): the shared G1 suite and the Kubernetes-mode specs of
# internal/gateway/kubernetes, run against agentgateway's real controller v1.7.0.
#
#   1. a throwaway kind cluster (default name koinon-g2; never touches another context: it writes its
#      own kubeconfig and passes it to the tests);
#   2. the Gateway API standard CRDs v1.4.0 (sha256 pinned) and agentgateway-crds and agentgateway
#      v1.7.0 from cr.agentgateway.dev (pinned chart versions);
#   3. go test ./internal/gateway/kubernetes with KOINON_KIND_KUBECONFIG and KOINON_KIND_HOST (the
#      address pods use to reach the test process: the fakes run in it);
#   4. the cluster is deleted unless KEEP=1.
#
#   CLUSTER=koinon-g2  KEEP=0  OUT=<dir for the kubeconfig and the log>  FOCUS=<Ginkgo focus regexp>

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

CLUSTER=${CLUSTER:-koinon-g2}
KEEP=${KEEP:-0}
GATEWAY_CRDS_VERSION=v1.4.0
GATEWAY_CRDS_SHA256=6a4029e661446d64add866a00ecdc40c14219b68777ab614c5cdaac0adb481f1
AGENTGATEWAY_VERSION=v1.7.0
OUT=${OUT:-$(mktemp -d "${TMPDIR:-/tmp}/g2-kind.XXXXXX")}
mkdir -p "$OUT"
export KUBECONFIG="$OUT/kubeconfig"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

cleanup() {
	if [[ $KEEP != 1 ]]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --config "$root/test/e2e/kind/kind-config.yaml" --wait 120s

manifest="$OUT/gateway-api.yaml"
"$(dirname "$0")/fetch-pinned.sh" "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_CRDS_VERSION}/standard-install.yaml" "${GATEWAY_CRDS_SHA256}" "$manifest"
kubectl apply --server-side -f "$manifest" >/dev/null

helm upgrade -i agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds \
	--version "$AGENTGATEWAY_VERSION" -n agentgateway-system --create-namespace --wait >/dev/null
helm upgrade -i agentgateway oci://cr.agentgateway.dev/charts/agentgateway \
	--version "$AGENTGATEWAY_VERSION" -n agentgateway-system --wait --timeout 4m >/dev/null
kubectl get gatewayclass agentgateway

# The address of the test process as pods see it: Docker Desktop and OrbStack resolve
# host.docker.internal on the node; on Linux the host is the gateway of the kind network.
node="${CLUSTER}-control-plane"
host=$(docker exec "$node" getent hosts host.docker.internal 2>/dev/null | awk '{print $1; exit}' || true)
if [[ -z $host ]]; then
	host=$(docker network inspect kind -f '{{(index .IPAM.Config 0).Gateway}}')
fi
echo "pods reach the test host at $host"

export KOINON_KIND_KUBECONFIG="$KUBECONFIG" KOINON_KIND_HOST="$host"
args=()
[[ -z ${FOCUS:-} ]] || args+=("-ginkgo.focus=$FOCUS")
go test ./internal/gateway/kubernetes -count=1 -v -timeout 30m ${args[@]+"${args[@]}"} 2>&1 | tee "$OUT/go-test.log"
