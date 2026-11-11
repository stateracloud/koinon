# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: restoreHold puts all three Deployments in restore-hold mode (issue #462).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# Off by default: no Deployment carries the variable.
lacks "no KOINON_RESTORE_HOLD by default" 'KOINON_RESTORE_HOLD' --show-only templates/deployments.yaml
# On: koinon-api, koinon-reconciler and koinon-agentd each get it, so none of them acts outward.
hold_count() { render --show-only templates/deployments.yaml --set restoreHold=true | grep -c -E -- '- name: KOINON_RESTORE_HOLD$' || true; }
[[ $(hold_count) == 3 ]] || fail "restoreHold=true sets KOINON_RESTORE_HOLD on the three Deployments, got $(hold_count)"
env_is KOINON_RESTORE_HOLD '"true"' --set restoreHold=true
# The value is a boolean: a typo fails the render instead of silently releasing the hold.
if render --set restoreHold=yes-please >/dev/null 2>&1; then fail "restoreHold must be a boolean"; fi

# The held-install alert (issue #462): a PrometheusRule group that renders whether or not durable is on.
rh=(--set metrics.prometheusRule.enabled=true --api-versions monitoring.coreos.com/v1/PrometheusRule --show-only templates/prometheusrule.yaml)
has "restore-hold alert renders" 'alert: KoinonRestoreHoldOn$' "${rh[@]}"
has "restore-hold alert renders without durable" 'alert: KoinonRestoreHoldOn$' "${rh[@]}" --set api.durable.enabled=false
has "restore-hold alert reads the max of the gauge" 'expr: max\(koinon_restore_hold\) == 1$' "${rh[@]}"
has "restore-hold alert waits 30m" 'for: 30m$' "${rh[@]}"

# A held standalone gateway is ready but deny-all (issue #530, ADR 0107 amendment): the chart renders a
# fixed config ConfigMap and the gateway Deployment mounts it, so helm --wait and GitOps health checks
# finish while nothing can go out. The reconciler's ConfigMap (koinon-gateway-config) stays unrendered.
gw=(--set gateway.mode=standalone --show-only templates/gateway-standalone.yaml)
lacks "unheld standalone renders no held gateway config" 'koinon-gateway-config-held' "${gw[@]}"
has "unheld gateway mounts the reconciler's config" 'name: koinon-gateway-config$' "${gw[@]}"
has "held install renders the held gateway config" 'name: koinon-gateway-config-held$' "${gw[@]}" --set restoreHold=true
has "held gateway config admits no request" '- "false"$' "${gw[@]}" --set restoreHold=true
has "held gateway config has no models" 'models: \[\]$' "${gw[@]}" --set restoreHold=true
has "held gateway config keeps the readiness listener" "readinessAddr: '\[::\]:15021'" "${gw[@]}" --set restoreHold=true
held_mounts=$(render "${gw[@]}" --set restoreHold=true | grep -c -E -- '^            name: koinon-gateway-config-held$' || true)
[[ $held_mounts == 1 ]] || fail "held gateway Deployment mounts the held config once, got $held_mounts"
lacks "held gateway Deployment does not mount the reconciler's config" '^            name: koinon-gateway-config$' "${gw[@]}" --set restoreHold=true
lacks "kubernetes gateway mode renders no held gateway config" 'koinon-gateway-config-held' --set gateway.mode=kubernetes --set restoreHold=true
