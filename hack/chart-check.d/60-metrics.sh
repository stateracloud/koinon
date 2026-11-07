# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: metrics Services, ServiceMonitors and NetworkPolicies.
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# Metrics (old issue 106, ADR 0023): every component has a "metrics" container port and a Service of its
# own; the ServiceMonitors are off by default and need the CRD; the NetworkPolicies admit the port only
# from metrics.allowFrom.
n=$(render --show-only templates/deployments.yaml | grep -c -- '- name: metrics$' || true)
[ "$n" = 3 ] || fail "the metrics container port is on $n Deployments, want 3"
has "metrics port follows metrics.port" 'containerPort: 9191$' --set metrics.port=9191 --show-only templates/deployments.yaml
env_is KOINON_METRICS_ADDR '":9191"' --set metrics.port=9191
n=$(render --show-only templates/metrics.yaml | grep -c '^kind: Service$' || true)
[ "$n" = 3 ] || fail "$n metrics Services, want 3"
lacks "no ServiceMonitor by default" 'kind: ServiceMonitor'
sm=(--set metrics.serviceMonitor.enabled=true --api-versions monitoring.coreos.com/v1/ServiceMonitor)
n=$(render "${sm[@]}" | grep -c '^kind: ServiceMonitor$' || true)
[ "$n" = 4 ] || fail "$n ServiceMonitors with metrics.serviceMonitor.enabled, want 4 (api, reconciler, agentd, gateway)"
has "gateway ServiceMonitor scrapes the stats port" 'port: stats$' "${sm[@]}" --show-only templates/metrics.yaml
has "gateway ServiceMonitor is named for the gateway" 'name: koinon-gateway$' "${sm[@]}" --show-only templates/metrics.yaml
# The gateway ServiceMonitor selects by component: exactly one Service carries component=gateway,
# and it is the gateway Service with its "stats" port.
gwsvc=$(render "${sm[@]}" | awk 'BEGIN{RS="\n---\n"} /\nkind: Service\n/ && /app\.kubernetes\.io\/component: gateway\n/ {n++; if (/name: koinon-gateway\n/ && /- name: stats\n/) s++} END{print n+0, s+0}')
[ "$gwsvc" = "1 1" ] || fail "Services matching the gateway ServiceMonitor (count, of which the gateway Service with a stats port): $gwsvc, want 1 1"
gwsel=$(render "${sm[@]}" --show-only templates/metrics.yaml | awk 'BEGIN{RS="\n---\n"} /kind: ServiceMonitor\n/ && /\n  name: koinon-gateway\n/ && /matchLabels:\n( +app\.kubernetes\.io\/(name|instance): koinon\n)+ +app\.kubernetes\.io\/component: gateway\n/ {n++} END{print n+0}')
[ "$gwsel" = 1 ] || fail "gateway ServiceMonitor must select app.kubernetes.io/component=gateway"
smk=("${sm[@]}" --set gateway.mode=kubernetes)
lacks "no PodMonitor in standalone mode" 'kind: PodMonitor' "${sm[@]}"
pm=(--api-versions monitoring.coreos.com/v1/PodMonitor)
n=$(render "${smk[@]}" "${pm[@]}" | grep -c '^kind: ServiceMonitor$' || true)
[ "$n" = 3 ] || fail "$n ServiceMonitors in kubernetes mode, want 3 (the chart renders no gateway Service)"
n=$(render "${smk[@]}" "${pm[@]}" | grep -c '^kind: PodMonitor$' || true)
[ "$n" = 1 ] || fail "$n PodMonitors in kubernetes mode, want 1"
has "PodMonitor selects the proxy pods" 'gateway.networking.k8s.io/gateway-name: koinon-gateway$' "${smk[@]}" "${pm[@]}" --show-only templates/metrics.yaml
has "PodMonitor scrapes the stats port" 'portNumber: 15020$' "${smk[@]}" "${pm[@]}" --set metrics.serviceMonitor.labels.release=prom --show-only templates/metrics.yaml
if render "${smk[@]}" >/dev/null 2>&1; then fail "kubernetes mode with the ServiceMonitor CRD but no PodMonitor CRD must fail"; fi
n=$(render --set metrics.serviceMonitor.enabled=true --set metrics.serviceMonitor.skipCapabilityCheck=true --set gateway.mode=kubernetes | grep -c '^kind: PodMonitor$' || true)
[ "$n" = 1 ] || fail "$n PodMonitors with skipCapabilityCheck, want 1"
has "ServiceMonitor interval and labels" 'interval: "30s"' "${sm[@]}" --set metrics.serviceMonitor.interval=30s --set metrics.serviceMonitor.labels.release=prom --show-only templates/metrics.yaml
has "ServiceMonitor extra labels" 'release: prom$' "${sm[@]}" --set metrics.serviceMonitor.labels.release=prom --show-only templates/metrics.yaml
if render --set metrics.serviceMonitor.enabled=true >/dev/null 2>&1; then
	fail "metrics.serviceMonitor.enabled=true must fail without the ServiceMonitor CRD"
fi
n=$(render --set metrics.serviceMonitor.enabled=true --set metrics.serviceMonitor.skipCapabilityCheck=true | grep -c '^kind: ServiceMonitor$' || true)
[ "$n" = 4 ] || fail "$n ServiceMonitors with skipCapabilityCheck and no --api-versions, want 4"
lacks "metrics port closed by default" 'port: metrics$' --show-only templates/networkpolicy.yaml
scrape=(--set 'metrics.allowFrom[0].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name=monitoring')
n=$(render "${scrape[@]}" --show-only templates/networkpolicy.yaml | grep -c 'port: metrics$' || true)
[ "$n" = 3 ] || fail "metrics.allowFrom opens the metrics port in $n policies, want api, reconciler and agentd (3)"
has "metrics.allowFrom peer is rendered" 'kubernetes.io/metadata.name: monitoring$' "${scrape[@]}" --show-only templates/networkpolicy.yaml
