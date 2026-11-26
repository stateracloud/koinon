# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: koinon-gateway, the model path (ADR 0097 step 2, issue #279).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

model=(--set gateway.model.driver=koinon)
model_only=(--show-only templates/gateway-koinon-model.yaml)

# Default: agentgateway serves the model path, and nothing of the model gateway renders.
lacks "no model gateway pods by default" 'component: model-gateway'

has "model gateway runs two replicas" '^  replicas: 2$' "${model[@]}" "${model_only[@]}"
has "model gateway rollout keeps every pod until the new one is ready" 'maxUnavailable: 0$' "${model[@]}" "${model_only[@]}"
has "model gateway pods spread over nodes" 'topologyKey: kubernetes.io/hostname$' "${model[@]}" "${model_only[@]}"
has "model gateway serves the model path only" '--mcp-path=false$' "${model[@]}" "${model_only[@]}"
has "model gateway turns the model path on" '--model-path$' "${model[@]}" "${model_only[@]}"
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" "${model[@]}" --set gateway.model.koinon.replicas=1 >/dev/null 2>&1; then
  fail "model gateway accepts one replica"
fi
for p in 15020 15021; do
  if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" "${model[@]}" --set gateway.model.koinon.port=$p >/dev/null 2>&1; then
    fail "model gateway accepts port $p, the fixed metrics or health port"
  fi
done

has "model gateway grace period is the drain timeout plus 30" 'terminationGracePeriodSeconds: 65$' "${model[@]}" "${model_only[@]}"
has "model gateway is told the drain timeout" '--drain-timeout=30s$' "${model[@]}" "${model_only[@]}"
has "model gateway is told the shutdown delay" '--shutdown-delay=5s$' "${model[@]}" "${model_only[@]}"
has "model gateway readiness is /readyz" 'path: /readyz,' "${model[@]}" "${model_only[@]}"
has "model gateway liveness is /healthz" 'path: /healthz,' "${model[@]}" "${model_only[@]}"
has "model gateway has a PodDisruptionBudget" '^kind: PodDisruptionBudget$' "${model[@]}" "${model_only[@]}"
has "model gateway image is built from image.registry" 'image: ghcr.io/stateracloud/koinon/gateway:' "${model[@]}" "${model_only[@]}"

# Secret custody: no database login, no key-encryption key, no Kubernetes API token.
lacks "model gateway mounts no KEK" 'kek|KEK' "${model[@]}" "${model_only[@]}"
lacks "model gateway has no database login" 'KOINON_DATABASE|secretKeyRef|postgres' "${model[@]}" "${model_only[@]}"
n=$(render "${model[@]}" "${model_only[@]}" | grep -c 'automountServiceAccountToken: false' || true)
[ "$n" = 2 ] || fail "model gateway mounts a service account token ($n of 2 places say false)"
has "model gateway mounts the provider keys read-only" 'mountPath: /secrets/providers$' "${model[@]}" "${model_only[@]}"
has "model gateway reads the State stream token from the OTLP token Secret" '--state-token-file=/secrets/otlp/token$' "${model[@]}" "${model_only[@]}"
has "model gateway runs with a read-only root" 'readOnlyRootFilesystem: true$' "${model[@]}" "${model_only[@]}"

# Network: the State listener admits the pods, and the pods have a NetworkPolicy of their own.
model_api_np=$(netpol_doc koinon-api "${model[@]}")
grep -q 'component: model-gateway$' <<<"$model_api_np" || fail "the State stream port of koinon-api does not admit the model gateway"
if grep -q 'component: model-gateway$' <<<"$api_off"; then fail "the State stream port admits the model gateway when it is not selected"; fi
model_np=$(netpol_doc koinon-model-gateway "${model[@]}")
[ -n "$model_np" ] || fail "model gateway has no NetworkPolicy of its own"
grep -q '10.0.0.0/8' <<<"$model_np" || fail "model gateway egress does not carve out the private ranges"

if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" "${model[@]}" --set gateway.mode=kubernetes --set gateway.mcp.driver=agentgateway >/dev/null 2>&1; then
  fail "gateway.model.driver=koinon renders with gateway.mode=kubernetes"
fi
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" --set gateway.model.driver=other >/dev/null 2>&1; then
  fail "gateway.model.driver accepts an unknown driver"
fi
