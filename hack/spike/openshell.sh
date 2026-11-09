#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Spike step 1: install the OpenShell gateway (requires Agent Sandbox first).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

VERSION=0.1.7
DIGEST=sha256:3e5998c749390a6be3a4f553e52e488b6f692e19be7b8bf681a26285d1e18901
cd "$(dirname "$0")"

helm --kube-context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}" upgrade --install openshell \
  "oci://ghcr.io/nvidia/openshell/helm-chart@${DIGEST}" --version "$VERSION" \
  --namespace openshell --create-namespace \
  -f openshell-values.yaml --wait --timeout 5m
kubectl --context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}" label namespace openshell app.kubernetes.io/part-of=koinon-spike --overwrite
