#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# E1 prerequisites (ADR 0049: not on the quickstart clock): a single-node kind cluster with
# kube-network-policies, Agent Sandbox v1.0.10, RuntimeClass `runc`, OpenShell 0.1.7 and the
# CloudNativePG operator. Taken from spike S1 (docs/spikes/kind.md), plus CNPG.
# Every kubectl/helm call names --context kind-$CLUSTER; nothing else is touched.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

CLUSTER=${CLUSTER:-koinon-e1}
CTX="kind-${CLUSTER}"
# kind v0.33.0 default node image (Kubernetes 1.37.0).
NODE_IMAGE=kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5

AS_VERSION=v1.0.10
AS_SHA256=7cc1eca2fb45844b0ec0c35fc975340dc01af2994301ffa8bf05db1ad1e397ee
AS_URL="https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AS_VERSION}/sandbox-with-extensions.yaml"

OS_VERSION=0.1.7
OS_DIGEST=sha256:3e5998c749390a6be3a4f553e52e488b6f692e19be7b8bf681a26285d1e18901

# kind's kindnet does not enforce NetworkPolicy (spike S1); the chart's policies need an engine.
KNP_VERSION=v1.1.2
KNP_SHA256=a8345bd40f74e812c7f2e13c40a841d4d6c7b1fc22e245750116072807a0f4ba
KNP_URL="https://raw.githubusercontent.com/kubernetes-sigs/kube-network-policies/${KNP_VERSION}/install.yaml"

# CloudNativePG operator: release manifest pinned by SHA-256, operator image pinned by digest.
CNPG_VERSION=1.30.1
CNPG_SHA256=37237f145d8138256ea25ae830f87759255665ff08f8d552fdd8224a5ec032fb
CNPG_IMAGE_DIGEST=sha256:923c267ec29636db3bee20f993d0ec4973fa22998e1adad37da79e4d32b5bc07
CNPG_URL="https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v${CNPG_VERSION}/cnpg-${CNPG_VERSION}.yaml"

here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=test/e2e/kind/parts.sh
source "$here/parts.sh"
K=(kubectl --context "$CTX")
tmp=$(mktemp -d "${TMPDIR:-/tmp}/up.XXXXXX")
trap 'kill_parts; rm -rf "$tmp"' EXIT

t0=$(date +%s)
step() { echo "== $(date -u +%H:%M:%SZ) $*"; ts=$(date +%s); }
done_step() { echo "-- $1: $(( $(date +%s) - ts ))s"; }

# The add-ons do not depend on each other once the cluster is up (#572): fetch the three manifests together,
# apply them, then wait for the rollouts together. Only OpenShell still waits for Agent Sandbox (and the
# RuntimeClass, applied before). `bg` starts a part in its own process group, `wait_all` fails when any part
# failed and then ends every other part's whole process tree (parts.sh).
install_openshell() {
  "${K[@]}" -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=180s
  helm --kube-context "$CTX" upgrade --install openshell \
    "oci://ghcr.io/nvidia/openshell/helm-chart@${OS_DIGEST}" --version "$OS_VERSION" \
    --namespace openshell --create-namespace \
    -f "$here/openshell-values.yaml" --wait --timeout 5m
}

step "kind create cluster $CLUSTER"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "cluster $CLUSTER exists; reusing it"
else
  kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --config "$here/kind-config.yaml" --wait 120s
fi
done_step kind

step "fetch the add-on manifests (kube-network-policies $KNP_VERSION, CloudNativePG $CNPG_VERSION, Agent Sandbox $AS_VERSION)"
bg "$here/../../../hack/fetch-pinned.sh" "$KNP_URL" "$KNP_SHA256" "$tmp/knp.yaml"
bg "$here/../../../hack/fetch-pinned.sh" "$CNPG_URL" "$CNPG_SHA256" "$tmp/cnpg.yaml"
bg "$here/../../../hack/fetch-pinned.sh" "$AS_URL" "$AS_SHA256" "$tmp/agent-sandbox.yaml"
wait_all
done_step fetch

step "apply the add-ons and RuntimeClass runc"
sed -i.bak "s#image: ghcr.io/cloudnative-pg/cloudnative-pg:${CNPG_VERSION}\$#image: ghcr.io/cloudnative-pg/cloudnative-pg:${CNPG_VERSION}@${CNPG_IMAGE_DIGEST}#" "$tmp/cnpg.yaml"
grep -q "@${CNPG_IMAGE_DIGEST}" "$tmp/cnpg.yaml" || { echo "CNPG image pin not applied" >&2; exit 1; }
"${K[@]}" apply -f "$tmp/knp.yaml"
"${K[@]}" apply --server-side -f "$tmp/cnpg.yaml"
"${K[@]}" apply --server-side -f "$tmp/agent-sandbox.yaml"
"${K[@]}" apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: runc
  labels:
    # OpenShell resource admission refuses RuntimeClasses without this label.
    openshell.ai/sandbox-attachable: "true"
handler: runc
EOF
done_step apply

step "wait for kube-network-policies, CloudNativePG, Agent Sandbox and OpenShell, together"
bg "${K[@]}" -n kube-system rollout status ds/kube-network-policies --timeout=120s
bg "${K[@]}" -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=180s
bg install_openshell
wait_all
done_step rollouts

echo "-- total up: $(( $(date +%s) - t0 ))s"
