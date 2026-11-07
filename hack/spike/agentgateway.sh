#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Spike step 5: deploy agentgateway standalone (no CRDs, no controller).

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo "Koinon's scripts need bash 5 or newer (this is $BASH_VERSION); on macOS: brew install bash" >&2; exit 2; }

set -euo pipefail
cd "$(dirname "$0")/agentgateway"
K=(kubectl --context "${SPIKE_CONTEXT:?set SPIKE_CONTEXT}")
"${K[@]}" apply -f manifests.yaml
"${K[@]}" -n agentgateway-system create configmap agentgateway-config --from-file=config.yaml \
  --dry-run=client -o yaml | "${K[@]}" apply -f -
"${K[@]}" -n agentgateway-system rollout status deploy/agentgateway --timeout=180s
