# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: koinon-gateway, the MCP path (ADR 0097 step 2, issue #278).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

mcp=(--set gateway.mcp.driver=koinon --set gateway.mcp.koinon.acceptOrgTargetsDropped=true)
mcp_only=(--show-only templates/gateway-koinon-mcp.yaml)

# Default: agentgateway serves MCP, and nothing of koinon-gateway renders (the fallback is the default).
lacks "no koinon-gateway pods by default" 'component: mcp-gateway'
env_is KOINON_GATEWAY_MCP_URL '"http://koinon-gateway.koinon.svc.cluster.local:8080/mcp"'

# Selected: koinon-api sends approved MCP calls to koinon-gateway's Service, and the LLM path stays.
env_is KOINON_GATEWAY_MCP_URL '"http://koinon-mcp-gateway.koinon.svc.cluster.local:8080/mcp"' "${mcp[@]}"
env_is KOINON_GATEWAY_URL '"http://koinon-gateway.koinon.svc.cluster.local:8080"' "${mcp[@]}"
has "agentgateway still renders as the LLM path" 'ghcr.io/agentgateway/agentgateway' "${mcp[@]}"

# Two replicas, rolling without a gap, spread over nodes.
has "koinon-gateway runs two replicas" '^  replicas: 2$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway rollout keeps every pod until the new one is ready" 'maxUnavailable: 0$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway pods spread over nodes" 'topologyKey: kubernetes.io/hostname$' "${mcp[@]}" "${mcp_only[@]}"
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" "${mcp[@]}" --set gateway.mcp.koinon.replicas=1 >/dev/null 2>&1; then
  fail "koinon-gateway accepts one replica"
fi

# Drain: the pod grace period is the shutdown delay, the drain timeout and 30 seconds, and the process gets the timeout.
has "koinon-gateway grace period is the drain timeout plus 30" 'terminationGracePeriodSeconds: 65$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway is told the drain timeout" '--drain-timeout=30s$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway is told the shutdown delay" '--shutdown-delay=5s$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway grace period follows the drain timeout" 'terminationGracePeriodSeconds: 95$' "${mcp[@]}" --set gateway.mcp.koinon.drainTimeoutSeconds=60 "${mcp_only[@]}"
has "koinon-gateway readiness is /readyz" 'path: /readyz,' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway liveness is /healthz" 'path: /healthz,' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway has a PodDisruptionBudget" '^kind: PodDisruptionBudget$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway PodDisruptionBudget takes one pod at a time" '^  maxUnavailable: 1$' "${mcp[@]}" "${mcp_only[@]}"

# The image and pull secrets follow the chart's rules, like every Koinon binary.
has "koinon-gateway image is built from image.registry" 'image: ghcr.io/stateracloud/koinon/gateway:' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway image follows a registry mirror" 'image: mirror.example.test/stateracloud/koinon/gateway:' "${mcp[@]}" --set 'global.registryMirrors.ghcr\.io=mirror.example.test' "${mcp_only[@]}"
has "koinon-gateway uses the image pull secrets" 'name: pull-secret$' "${mcp[@]}" --set 'global.imagePullSecrets[0]=pull-secret' "${mcp_only[@]}"

# Secret custody (ADR 0097 decision 5, ADR 0120): no database login, no key-encryption key, no
# Kubernetes API token. The pod mounts the credential files of the MCP targets and the OTLP token only.
lacks "koinon-gateway mounts no KEK" 'kek|KEK' "${mcp[@]}" "${mcp_only[@]}"
lacks "koinon-gateway has no database login" 'KOINON_DATABASE|secretKeyRef|postgres' "${mcp[@]}" "${mcp_only[@]}"
n=$(render "${mcp[@]}" "${mcp_only[@]}" | grep -c 'automountServiceAccountToken: false' || true)
[ "$n" = 2 ] || fail "koinon-gateway mounts a service account token ($n of 2 places say false)"
has "koinon-gateway mounts the provider keys read-only" 'mountPath: /secrets/providers$' "${mcp[@]}" "${mcp_only[@]}"
has "koinon-gateway reads the State stream token from the OTLP token Secret" '--state-token-file=/secrets/otlp/token$' "${mcp[@]}" "${mcp_only[@]}"
lacks "koinon-gateway has no RBAC" 'name: koinon-mcp-gateway$' "${mcp[@]}" --show-only templates/rbac.yaml
has "koinon-gateway runs non-root with a read-only root" 'readOnlyRootFilesystem: true$' "${mcp[@]}" "${mcp_only[@]}"

# Network: koinon-api's State stream port admits the pods, and the pods have a NetworkPolicy of their own.
# netpol_doc NAME [helm args]: the one NetworkPolicy document called NAME, so an assertion cannot
# pass on text that another policy renders.
netpol_doc() {
  local n=$1; shift
  render "$@" --show-only templates/networkpolicy.yaml | awk -v n="$n" 'BEGIN { RS = "\n---\n" } $0 ~ ("\n  name: " n "\n") { print }'
}
api_np=$(netpol_doc koinon-api "${mcp[@]}")
grep -q 'component: mcp-gateway$' <<<"$api_np" || fail "the State stream port of koinon-api does not admit koinon-gateway"
api_off=$(netpol_doc koinon-api)
if grep -q 'component: mcp-gateway$' <<<"$api_off"; then fail "the State stream port admits koinon-gateway when it is not selected"; fi
mcp_np=$(netpol_doc koinon-mcp-gateway "${mcp[@]}")
[ -n "$mcp_np" ] || fail "koinon-gateway has no NetworkPolicy of its own"
grep -q '10.0.0.0/8' <<<"$mcp_np" || fail "koinon-gateway egress does not carve out the private ranges"
if grep -q 'openshell.ai/managed-by' <<<"$mcp_np"; then fail "koinon-gateway admits sandbox namespaces before agents use it"; fi

# The switch is refused until the operator accepts that org targets are dropped (until the guard slice).
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" --set gateway.mcp.driver=koinon >/dev/null 2>&1; then
  fail "gateway.mcp.driver=koinon renders without acceptOrgTargetsDropped"
fi

# The switch needs standalone mode: kubernetes mode is frozen.
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" "${mcp[@]}" --set gateway.mode=kubernetes >/dev/null 2>&1; then
  fail "gateway.mcp.driver=koinon renders with gateway.mode=kubernetes"
fi
if "$HELM" template koinon "$CHART" --namespace koinon --set openshell.insecure=true --set "$ADDRESS" --set gateway.mcp.driver=other >/dev/null 2>&1; then
  fail "gateway.mcp.driver accepts an unknown driver"
fi
