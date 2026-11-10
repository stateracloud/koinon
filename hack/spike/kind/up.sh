#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Spike S1: single-node kind cluster with Agent Sandbox v1.0.10 and OpenShell 0.1.7
# (Kubernetes driver, default RuntimeClass `runc`). Prints wall-clock per step.
# Touches only the context kind-koinon-s1.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

CLUSTER=${CLUSTER:-koinon-s1}
CTX="kind-${CLUSTER}"
# kind v0.33.0 default node image (Kubernetes 1.37.0).
NODE_IMAGE=kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5

AS_VERSION=v1.0.10
AS_SHA256=7cc1eca2fb45844b0ec0c35fc975340dc01af2994301ffa8bf05db1ad1e397ee
AS_URL="https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AS_VERSION}/sandbox-with-extensions.yaml"

OS_VERSION=0.1.7
OS_DIGEST=sha256:3e5998c749390a6be3a4f553e52e488b6f692e19be7b8bf681a26285d1e18901

# kind's kindnet does not enforce NetworkPolicy; OpenShell's workload fence needs it.
KNP_VERSION=v1.1.2
KNP_SHA256=a8345bd40f74e812c7f2e13c40a841d4d6c7b1fc22e245750116072807a0f4ba
KNP_URL="https://raw.githubusercontent.com/kubernetes-sigs/kube-network-policies/${KNP_VERSION}/install.yaml"

# Optional: metrics-server for `kubectl top` (METRICS=0 to skip).
METRICS=${METRICS:-1}
MS_VERSION=v0.9.0

here="$(cd "$(dirname "$0")" && pwd)"
K=(kubectl --context "$CTX")
tmp=$(mktemp -d "${TMPDIR:-/tmp}/up.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

t0=$(date +%s)
step() { echo "== $(date -u +%H:%M:%SZ) $*"; ts=$(date +%s); }
done_step() { echo "-- $1: $(( $(date +%s) - ts ))s"; }

step "kind create cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s
done_step kind

step "kube-network-policies $KNP_VERSION"
curl -fsSL -o "$tmp/knp.yaml" "$KNP_URL"
echo "${KNP_SHA256}  $tmp/knp.yaml" | shasum -a 256 -c -
"${K[@]}" apply -f "$tmp/knp.yaml"
"${K[@]}" -n kube-system rollout status ds/kube-network-policies --timeout=120s
done_step network-policies

step "Agent Sandbox $AS_VERSION"
curl -fsSL -o "$tmp/agent-sandbox.yaml" "$AS_URL"
echo "${AS_SHA256}  $tmp/agent-sandbox.yaml" | shasum -a 256 -c -
"${K[@]}" apply --server-side -f "$tmp/agent-sandbox.yaml"
"${K[@]}" -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=180s
done_step agent-sandbox

step "RuntimeClass runc"
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
done_step runtimeclass

step "OpenShell $OS_VERSION"
helm --kube-context "$CTX" upgrade --install openshell \
  "oci://ghcr.io/nvidia/openshell/helm-chart@${OS_DIGEST}" --version "$OS_VERSION" \
  --namespace openshell --create-namespace \
  -f "$here/openshell-values.yaml" --wait --timeout 5m
done_step openshell

if [[ "$METRICS" == 1 ]]; then
  step "metrics-server $MS_VERSION"
  curl -fsSL -o "$tmp/metrics-server.yaml" \
    "https://github.com/kubernetes-sigs/metrics-server/releases/download/${MS_VERSION}/components.yaml"
  "${K[@]}" apply -f "$tmp/metrics-server.yaml"
  # kind's kubelet serves a self-signed certificate.
  "${K[@]}" -n kube-system patch deploy metrics-server --type=json \
    -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
  "${K[@]}" -n kube-system rollout status deploy/metrics-server --timeout=180s
  done_step metrics-server
fi

echo "-- total up: $(( $(date +%s) - t0 ))s"
