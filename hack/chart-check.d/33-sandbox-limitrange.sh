# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the sandbox LimitRange writer (#76, ADR 0083 follow-up).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

lr=templates/sandbox-limitrange.yaml

# On by default: a ClusterRole, binding and ValidatingAdmissionPolicy for the reconciler to apply
# a LimitRange in each OpenShell workspace namespace.
has "sandbox LimitRange writer ClusterRole" 'name: koinon-koinon-sandbox-limitrange-writer$' --show-only "$lr"
has "sandbox LimitRange writer ClusterRole LimitRanges" 'resources: \[limitranges\]' --show-only "$lr"
has "sandbox LimitRange writer ClusterRole verbs" 'verbs: \[create, get, update, patch\]' --show-only "$lr"
has "sandbox LimitRange writer ClusterRoleBinding" 'kind: ClusterRoleBinding' --show-only "$lr"
has "sandbox LimitRange writer ClusterRoleBinding subject" 'name: koinon-reconciler$' --show-only "$lr"
has "sandbox LimitRange writer ValidatingAdmissionPolicy" 'name: koinon-koinon-sandbox-limitrange-binding$' --show-only "$lr"
has "sandbox LimitRange writer policy fails closed" 'failurePolicy: Fail' --show-only "$lr"
has "sandbox LimitRange writer covers CREATE and UPDATE" 'operations: \[CREATE, UPDATE\]' --show-only "$lr"
has "sandbox LimitRange writer binding denies" 'validationActions: \["Deny"\]' --show-only "$lr"
has "sandbox LimitRange writer keys on the reconciler identity" 'system:serviceaccount:koinon:koinon-reconciler' --show-only "$lr"
has "sandbox LimitRange writer requires the sized shape" 'Container defaults to 50m/64Mi requests and 250m/256Mi limits' --show-only "$lr"
has "sandbox LimitRange writer limits to the configured gateway ID" "openshell.ai/gateway-id'\] == \"prod\"" --set openshell.gatewayId=prod --show-only "$lr"

# The reconciler is told the writer is on and the fixed LimitRange name.
cfg=$(render --show-only templates/config.yaml)
grep -A2 -F -- 'sandboxLimitRange:' <<<"$cfg" | grep -Fq -- 'enabled: true' || fail "the reconciler is told the sandbox LimitRange is on"
grep -A3 -F -- 'sandboxLimitRange:' <<<"$cfg" | grep -Fq -- 'name: "koinon-koinon-sandbox-limitrange"' || fail "the reconciler is told the sandbox LimitRange name"

off=(--set sandboxLimitRange.enabled=false)
lacks "sandboxLimitRange.enabled=false renders no writer" 'sandbox-limitrange' "${off[@]}"
off_cfg=$(render "${off[@]}" --show-only templates/config.yaml)
grep -A2 -F -- 'sandboxLimitRange:' <<<"$off_cfg" | grep -Fq -- 'enabled: false' || fail "sandboxLimitRange.enabled=false tells the reconciler"
grep -A3 -F -- 'sandboxLimitRange:' <<<"$off_cfg" | grep -q -- 'name:' && fail "sandboxLimitRange.enabled=false names no LimitRange to the reconciler"

# Without Deny the writer ClusterRole would be unbounded (validationActions Warn/Audit only).
if render --set 'sandboxPolicy.validationActions={Warn}' --set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false >/dev/null 2>&1; then
	fail "sandboxLimitRange without a Deny validation action must not render"
fi
render --set 'sandboxPolicy.validationActions={Warn}' --set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false "${off[@]}" >/dev/null 2>&1 ||
	fail "validation actions without Deny and the LimitRange writer off must render"
if render --set sandboxPolicy.enabled=false --set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false >/dev/null 2>&1; then
	fail "sandboxLimitRange.enabled needs sandboxPolicy.enabled"
fi

# A label added to sandboxPolicy.namespaceSelector reaches the writer policy (same as the observer).
has "a label added to sandboxPolicy.namespaceSelector reaches the LimitRange writer" 'variables.labels\[\\"team\\"\] == \\"sandboxes\\"' \
	--set sandboxPolicy.namespaceSelector.matchLabels.team=sandboxes --show-only "$lr"
