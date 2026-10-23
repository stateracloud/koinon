#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Spike step 5: deploy agentgateway standalone (no CRDs, no controller).
set -euo pipefail
cd "$(dirname "$0")/agentgateway"
K=(kubectl --context homelab)
"${K[@]}" apply -f manifests.yaml
"${K[@]}" -n agentgateway-system create configmap agentgateway-config --from-file=config.yaml \
  --dry-run=client -o yaml | "${K[@]}" apply -f -
"${K[@]}" -n agentgateway-system rollout status deploy/agentgateway --timeout=180s
