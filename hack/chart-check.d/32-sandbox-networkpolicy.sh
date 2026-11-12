# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the sandbox default-deny NetworkPolicy writer (spike 8.15, ADR 0045).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

np=templates/sandbox-networkpolicy.yaml

# On by default: a ClusterRole, binding and ValidatingAdmissionPolicy for the reconciler to apply
# a namespace-wide default-deny NetworkPolicy in each OpenShell workspace namespace.
has "sandbox NetworkPolicy writer ClusterRole" 'name: koinon-koinon-sandbox-default-deny-writer$' --show-only "$np"
has "sandbox NetworkPolicy writer may create NetworkPolicies" 'resources: \[networkpolicies\]' --show-only "$np"
has "sandbox NetworkPolicy writer ValidatingAdmissionPolicy" 'name: koinon-koinon-sandbox-default-deny-binding$' --show-only "$np"
has "sandbox NetworkPolicy writer policy fails closed" 'failurePolicy: Fail' --show-only "$np"
has "sandbox NetworkPolicy writer covers CREATE and UPDATE" 'operations: \[CREATE, UPDATE\]' --show-only "$np"
has "sandbox NetworkPolicy writer binding denies" 'validationActions: \["Deny"\]' --show-only "$np"
has "sandbox NetworkPolicy writer keys on the reconciler identity" 'system:serviceaccount:koinon:koinon-reconciler' --show-only "$np"
has "sandbox NetworkPolicy writer requires the default-deny shape" 'policyTypes Ingress and Egress, and allow no traffic' --show-only "$np"
has "sandbox NetworkPolicy writer limits to the configured gateway ID" "openshell.ai/gateway-id'\] == \"prod\"" --set openshell.gatewayId=prod --show-only "$np"

# The reconciler is told the writer is on and the fixed NetworkPolicy name.
cfg=$(render --show-only templates/config.yaml)
grep -A2 -F -- 'sandboxNetworkPolicy:' <<<"$cfg" | hit -F -- 'enabled: true' || fail "the reconciler is told the sandbox NetworkPolicy is on"
grep -A3 -F -- 'sandboxNetworkPolicy:' <<<"$cfg" | hit -F -- 'name: "koinon-koinon-sandbox-default-deny"' || fail "the reconciler is told the sandbox NetworkPolicy name"

off=(--set sandboxNetworkPolicy.enabled=false)
lacks "sandboxNetworkPolicy.enabled=false renders no writer" 'sandbox-default-deny' "${off[@]}"
off_cfg=$(render "${off[@]}" --show-only templates/config.yaml)
grep -A2 -F -- 'sandboxNetworkPolicy:' <<<"$off_cfg" | hit -F -- 'enabled: false' || fail "sandboxNetworkPolicy.enabled=false tells the reconciler"
grep -A3 -F -- 'sandboxNetworkPolicy:' <<<"$off_cfg" | hit  -- 'name:' && fail "sandboxNetworkPolicy.enabled=false names no policy to the reconciler"

# Without Deny the writer ClusterRole would be unbounded (validationActions Warn/Audit only).
# sandboxLimitRange (on by default) also requires Deny; turn it off for the NetworkPolicy-only checks.
if render --set 'sandboxPolicy.validationActions={Warn}' --set sandboxObserver.enabled=false --set sandboxLimitRange.enabled=false >/dev/null 2>&1; then
	fail "sandboxNetworkPolicy without a Deny validation action must not render"
fi
render --set 'sandboxPolicy.validationActions={Warn}' --set sandboxObserver.enabled=false --set sandboxLimitRange.enabled=false "${off[@]}" >/dev/null 2>&1 ||
	fail "validation actions without Deny and the NetworkPolicy writer off must render"
if render --set sandboxPolicy.enabled=false --set sandboxObserver.enabled=false --set sandboxLimitRange.enabled=false >/dev/null 2>&1; then
	fail "sandboxNetworkPolicy.enabled needs sandboxPolicy.enabled"
fi

# A label added to sandboxPolicy.namespaceSelector reaches the writer policy (same as the observer).
has "a label added to sandboxPolicy.namespaceSelector reaches the NetworkPolicy writer" 'variables.labels\[\\"team\\"\] == \\"sandboxes\\"' \
	--set sandboxPolicy.namespaceSelector.matchLabels.team=sandboxes --show-only "$np"
