# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: the DBOS executor ID of koinon-api is its pod name (ADR 0085, #413).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# The ID comes from the downward API: unique per replica, never reused. With api.durable off there
# is no DBOS runtime and no executor ID.
exec_env() { render --show-only templates/deployments.yaml "$@" | grep -A4 -E -- '- name: KOINON_EXECUTOR_ID$'; }
exec_env --set api.durable.enabled=true | grep -Fq 'fieldPath: metadata.name' || fail "KOINON_EXECUTOR_ID is the pod name with api.durable.enabled"
if exec_env --set api.durable.enabled=false | grep -q .; then fail "no KOINON_EXECUTOR_ID with api.durable.enabled=false"; fi

# The stranded-workflows alert (ADR 0085, decision 11, #414): a PrometheusRule, off by default, that
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
