#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Spike step 1: install the OpenShell gateway (requires Agent Sandbox first).
set -euo pipefail

VERSION=0.1.5
DIGEST=sha256:e88977ce447fd31c6278697af22f8437b59eae42c9fae9c0e480498bf59d3129
cd "$(dirname "$0")"

helm --kube-context homelab upgrade --install openshell \
  "oci://ghcr.io/nvidia/openshell/helm-chart@${DIGEST}" --version "$VERSION" \
  --namespace openshell --create-namespace \
  -f openshell-values.yaml --wait --timeout 5m
kubectl --context homelab label namespace openshell app.kubernetes.io/part-of=koinon-spike --overwrite
