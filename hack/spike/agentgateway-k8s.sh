#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Spike step 5.6: agentgateway in Kubernetes mode (controller, its own CRDs and GatewayClass),
# released as agw-k8s next to the standalone agentgateway in agentgateway-system. The
# Gateway API CRDs are the platform's (Cilium) and are not touched.
set -euo pipefail
H=(helm --kube-context homelab)
"${H[@]}" upgrade --install agw-k8s-crds oci://cr.agentgateway.dev/charts/agentgateway-crds \
  --version v1.6.0 -n agentgateway-system --wait
"${H[@]}" upgrade --install agw-k8s oci://cr.agentgateway.dev/charts/agentgateway \
  --version v1.6.0 -n agentgateway-system --wait --timeout 5m
