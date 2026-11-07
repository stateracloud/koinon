#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Spike step 1: install the OpenShell gateway (requires Agent Sandbox first).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail

VERSION=0.1.5
DIGEST=sha256:e88977ce447fd31c6278697af22f8437b59eae42c9fae9c0e480498bf59d3129
cd "$(dirname "$0")"

helm --kube-context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}" upgrade --install openshell \
  "oci://ghcr.io/nvidia/openshell/helm-chart@${DIGEST}" --version "$VERSION" \
  --namespace openshell --create-namespace \
  -f openshell-values.yaml --wait --timeout 5m
kubectl --context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}" label namespace openshell app.kubernetes.io/part-of=koinon-spike --overwrite
