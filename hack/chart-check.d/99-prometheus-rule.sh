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
