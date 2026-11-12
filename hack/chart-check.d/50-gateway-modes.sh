# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: Kubernetes gateway mode and the reconciler's RBAC.
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# Kubernetes gateway mode (plan item G2): the chart renders no gateway of its own, only the Secret the
# reconciler writes provider keys into; a standard NetworkPolicy selects the controller's proxy pods
# by their gateway-name label; the reconciler may manage the agentgateway objects in the release
# namespace and nothing cluster-wide.
k8s=(--set gateway.mode=kubernetes)
lacks "no gateway Deployment in kubernetes mode" 'ghcr.io/agentgateway/agentgateway' "${k8s[@]}"
has "provider keys Secret in kubernetes mode" 'name: koinon-gateway-providers$' "${k8s[@]}" --show-only templates/gateway-kubernetes.yaml
n=$(render "${k8s[@]}" | grep -c 'secretName: koinon-gateway-providers$' || true)
[ "$n" = 0 ] || fail "the provider keys Secret is mounted by $n pods in kubernetes mode, want none (the controller reads it)"
has "proxy NetworkPolicy selects the controller's pod label" 'gateway.networking.k8s.io/gateway-name: koinon-gateway$' "${k8s[@]}" --show-only templates/networkpolicy.yaml
has "proxy egress to the controller's xDS port" 'port: 9978$' "${k8s[@]}" --show-only templates/networkpolicy.yaml
has "proxy egress carves out the private ranges" '10.0.0.0/8' "${k8s[@]}" --show-only templates/networkpolicy.yaml
has "reconciler manages the agentgateway objects" 'agentgatewaymodels' "${k8s[@]}" --show-only templates/rbac.yaml
lacks "no agentgateway rules in standalone mode" 'agentgatewaymodels' --show-only templates/rbac.yaml
# The reconciler never talks to the agents.x-k8s.io API (sandboxes go through the OpenShell gateway),
# so no rule in any mode grants it Sandbox permissions, and the chart has no ClusterRole for it.
lacks "no Sandbox permissions in the reconciler's RBAC" 'x-k8s\.io|sandboxes' --show-only templates/rbac.yaml
lacks "no Sandbox permissions in the reconciler's RBAC (gateway.mode=kubernetes)" 'x-k8s\.io|sandboxes' "${k8s[@]}" --show-only templates/rbac.yaml
lacks "no ClusterRole in rbac.yaml" '^kind: ClusterRole' --show-only templates/rbac.yaml
has "reconciler config names the Gateway" 'name: koinon-gateway$' "${k8s[@]}" --show-only templates/config.yaml
has "reconciler config names the GatewayClass" 'gatewayClassName: agentgateway$' "${k8s[@]}" --show-only templates/config.yaml
has "sandboxes reach the Gateway's Service" 'http://koinon-gateway.koinon.svc.cluster.local:4000' "${k8s[@]}" --show-only templates/deployments.yaml
env_is KOINON_GATEWAY_MCP_URL '"http://koinon-gateway.koinon.svc.cluster.local:4000/mcp"' "${k8s[@]}"
has "preflight checks the agentgateway controller" '--agentgateway-controller=agentgateway$' "${k8s[@]}" --show-only templates/preflight.yaml
has "preflight may read the agentgateway CRDs" 'agentgatewaymodels.agentgateway.dev$' "${k8s[@]}" --show-only templates/preflight.yaml
env_is KOINON_GATEWAY_MCP_URL '"http://koinon-gateway.koinon.svc.cluster.local:8080/mcp"'
# koinon-api reaches the gateway's LLM listener for the provider echo check, on the same host and port.
env_is KOINON_GATEWAY_URL '"http://koinon-gateway.koinon.svc.cluster.local:8080"'
env_is KOINON_GATEWAY_URL '"http://koinon-gateway.koinon.svc.cluster.local:4000"' "${k8s[@]}"
has "gateway.kubernetes.port is used" 'url: http://koinon-gateway.koinon.svc.cluster.local:4001$' "${k8s[@]}" --set gateway.kubernetes.port=4001 --show-only templates/config.yaml

# The tool servers trust the agent header the gateway sets (gateway.AgentHeader, X-Koinon-Agent; #281,
# ADR 0097 decision 5), so the tools port of koinon-api admits the gateway's pods and nothing else, in
# both gateway modes. A request that skips the gateway never reaches the listener, so a client cannot
# forge the header. The header's overwrite is tested in internal/gateway (drivers) and the refusal of a
# call without it in internal/githubtools.
tools_from() { # prints the 'from' selectors of the ingress rule on the tools port
  render "$@" --show-only templates/networkpolicy.yaml |
    awk '/^  ingress:/{i=1} i&&/^    - ports:/{blk=""; grab=1} grab{blk=blk $0 "\n"} /^    # /{if(grab&&blk ~ /- port: tools/){printf "%s", blk; exit} grab=0}'
}
tf=$(tools_from)
printf '%s\n' "$tf" | grep -q 'component: gateway' || fail "the tools port does not admit the gateway (standalone)"
for c in agentd sandbox api reconciler web; do
  ! printf '%s\n' "$tf" | grep -q "component: $c" || fail "the tools port admits $c (standalone)"
done
tf=$(tools_from "${k8s[@]}")
printf '%s\n' "$tf" | grep -q 'gateway.networking.k8s.io/gateway-name: koinon-gateway' || fail "the tools port does not admit the controller's proxy (kubernetes)"
! printf '%s\n' "$tf" | grep -q 'component: agentd' || fail "the tools port admits koinon-agentd (kubernetes)"
