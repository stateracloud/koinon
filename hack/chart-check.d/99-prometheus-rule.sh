# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the alerting rules, metrics.prometheusRule (ADR 0085 decision 11, old issue 414).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# The stranded-workflows alert (ADR 0085, decision 11, old issue 414): a PrometheusRule, off by default, that
# needs its CRD; the durable group exists only with api.durable.enabled.
lacks "no PrometheusRule by default" 'kind: PrometheusRule'
pr=(--set metrics.prometheusRule.enabled=true --api-versions monitoring.coreos.com/v1/PrometheusRule)
if render --set metrics.prometheusRule.enabled=true >/dev/null 2>&1; then fail "metrics.prometheusRule.enabled=true must fail without the PrometheusRule CRD"; fi
has "PrometheusRule renders with the CRD" '^kind: PrometheusRule$' "${pr[@]}"
has "PrometheusRule renders with skipCapabilityCheck" '^kind: PrometheusRule$' --set metrics.prometheusRule.enabled=true --set metrics.prometheusRule.skipCapabilityCheck=true
has "PrometheusRule extra labels" 'release: prom$' "${pr[@]}" --set metrics.prometheusRule.labels.release=prom --show-only templates/prometheusrule.yaml
has "stranded alert with api.durable.enabled" 'alert: KoinonDurableWorkflowsStranded$' "${pr[@]}" --set api.durable.enabled=true --show-only templates/prometheusrule.yaml
has "stranded alert aggregates replicas with max, not sum" 'max by \(version\) \(koinon_durable_workflows_pending\) > 0$' "${pr[@]}" --set api.durable.enabled=true --show-only templates/prometheusrule.yaml
has "stranded alert waits durableStrandedFor" 'for: "20m"$' "${pr[@]}" --set api.durable.enabled=true --set metrics.prometheusRule.durableStrandedFor=20m --show-only templates/prometheusrule.yaml
lacks "no durable alert without api.durable.enabled" 'koinon_durable' "${pr[@]}" --set api.durable.enabled=false --show-only templates/prometheusrule.yaml

# The sandbox default-deny NetworkPolicy error alert (#993): always rendered with the rules.
has "network policy error alert" 'alert: KoinonSandboxNetworkPolicyErrors$' "${pr[@]}" --show-only templates/prometheusrule.yaml
has "network policy error alert reads the writer's counter" 'koinon_reconciler_sandbox_network_policy_errors_total\[15m\]' "${pr[@]}" --show-only templates/prometheusrule.yaml

# The provider key echo alert (ADR 0097 section 15, #1332): only when koinon-gateway serves the model path.
has "key echo alert with the koinon model driver" 'alert: KoinonModelKeyEcho$' "${pr[@]}" --set gateway.model.driver=koinon --show-only templates/prometheusrule.yaml
has "key echo alert reads the gateway's counter" 'koinon_gateway_model_key_echo_total\[1h\]' "${pr[@]}" --set gateway.model.driver=koinon --show-only templates/prometheusrule.yaml
lacks "no key echo alert with the agentgateway model driver" 'KoinonModelKeyEcho' "${pr[@]}" --set gateway.model.driver=agentgateway --show-only templates/prometheusrule.yaml
# The alert needs the series: with the koinon model driver, the monitor scrapes the model-gateway stats port.
sm=(--set metrics.serviceMonitor.enabled=true --api-versions monitoring.coreos.com/v1/ServiceMonitor --set gateway.model.driver=koinon --show-only templates/metrics.yaml)
has "ServiceMonitor for the model gateway" 'name: koinon-model-gateway$' "${sm[@]}"
has "model gateway monitor selects its component" 'app.kubernetes.io/component: model-gateway$' "${sm[@]}"
lacks "no model gateway monitor with the agentgateway model driver" 'model-gateway' --set metrics.serviceMonitor.enabled=true --api-versions monitoring.coreos.com/v1/ServiceMonitor --set gateway.model.driver=agentgateway --show-only templates/metrics.yaml

# The node-disk alert for /var/lib/ate (#1494): only with the Substrate driver. The leak guard's byte alert (#1058)
# counts MCP bytes, not disk, so it cannot fire on a full home.
has "home filesystem alert with the Substrate driver" 'alert: KoinonSandboxHomeFilesystemFilling$' "${pr[@]}" "${wsub[@]}" --show-only templates/prometheusrule.yaml
has "home filesystem alert reads the /var/lib/ate mount point" 'node_filesystem_avail_bytes\{mountpoint="/var/lib/ate"\}' "${pr[@]}" "${wsub[@]}" --show-only templates/prometheusrule.yaml
has "home filesystem alert default threshold" '< 15$' "${pr[@]}" "${wsub[@]}" --show-only templates/prometheusrule.yaml
has "home filesystem alert threshold is a value" '< 20$' "${pr[@]}" "${wsub[@]}" --set metrics.prometheusRule.ateFilesystem.freeBelowPercent=20 --show-only templates/prometheusrule.yaml
has "home filesystem alert wait is a value" 'for: "5m"$' "${pr[@]}" "${wsub[@]}" --set metrics.prometheusRule.ateFilesystem.for=5m --show-only templates/prometheusrule.yaml
lacks "no home filesystem alert when switched off" 'KoinonSandboxHomeFilesystemFilling' "${pr[@]}" "${wsub[@]}" --set metrics.prometheusRule.ateFilesystem.enabled=false --show-only templates/prometheusrule.yaml
lacks "no home filesystem alert with the OpenShell driver" 'KoinonSandboxHomeFilesystemFilling' "${pr[@]}" --show-only templates/prometheusrule.yaml

# The fleet breaker alert of the front check (#1481): only with the Substrate driver.
has "fleet breaker alert with the Substrate driver" 'alert: KoinonSandboxFrontsSilentAcrossFleet$' "${pr[@]}" "${wsub[@]}" --show-only templates/prometheusrule.yaml
has "fleet breaker alert reads the leader's gauge with max" 'max\(koinon_reconciler_front_breaker_open\) > 0$' "${pr[@]}" "${wsub[@]}" --show-only templates/prometheusrule.yaml
lacks "no fleet breaker alert with the OpenShell driver" 'KoinonSandboxFrontsSilentAcrossFleet' "${pr[@]}" --show-only templates/prometheusrule.yaml
