#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Spike step 5.6: agentgateway in Kubernetes mode (controller, its own CRDs and GatewayClass),
# released as agw-k8s next to the standalone agentgateway in agentgateway-system. The
# Gateway API CRDs are the platform's (Cilium) and are not touched.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
H=(helm --kube-context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}")
"${H[@]}" upgrade --install agw-k8s-crds oci://cr.agentgateway.dev/charts/agentgateway-crds \
  --version v1.6.0 -n agentgateway-system --wait
"${H[@]}" upgrade --install agw-k8s oci://cr.agentgateway.dev/charts/agentgateway \
  --version v1.6.0 -n agentgateway-system --wait --timeout 5m
