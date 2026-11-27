# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the read-only observer access to the Substrate worker namespace (issue #1162,
# ADR 0120 section 6). Sourced in order by hack/chart-check.sh; it uses that script's helpers.

wobs=(--set execution.driver=substrate --set gateway.model.driver=koinon --set gateway.mcp.driver=koinon --set execution.substrate.controlAddress=api.ate-system.svc:443
	--set execution.substrate.routerAddress=atenet-router.ate-system.svc:443 --set execution.substrate.tokenAudience=ateapi
	--set execution.substrate.allowUnhardened=true --set execution.substrate.defaultTier=dev
	--set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --set sandboxLimitRange.enabled=false)
wobsT=(--show-only templates/substrate-observer.yaml)

lacks "no worker observer with the openshell driver" 'worker-observer'
has "the worker observer Role" 'kind: Role$' "${wobs[@]}" "${wobsT[@]}"
has "the worker observer RoleBinding" 'kind: RoleBinding$' "${wobs[@]}" "${wobsT[@]}"
has "the observer Role is in the worker namespace" 'namespace: "ate-workers"' "${wobs[@]}" "${wobsT[@]}"
has "the observer is the reconciler" 'name: koinon-reconciler' "${wobs[@]}" "${wobsT[@]}"
has "the worker namespace is configurable for the observer" 'namespace: "pools-x"' "${wobs[@]}" --set execution.substrate.workerNamespace=pools-x "${wobsT[@]}"
obs=$(render "${wobs[@]}" "${wobsT[@]}")
# Read-only: no verb but get, list and watch, and no wildcard.
nverbs=0
while IFS= read -r line; do
	nverbs=$((nverbs + 1))
	[[ $line == *'verbs: [get, list, watch]' ]] || fail "the worker observer must have get, list and watch only"
done < <(grep -E 'verbs:' <<<"$obs")
((nverbs > 0)) || fail "the worker observer must have rules"
if grep -Eq '\*|create|update|patch|delete|escalate|bind|impersonate' <<<"$(grep -E 'verbs:|resources:|apiGroups:' <<<"$obs")"; then fail "the worker observer must not write, bind or use a wildcard"; fi
if grep -Eq 'secrets|pods/exec|pods/log|pods/attach|pods/portforward|pods/proxy|pods/ephemeralcontainers' <<<"$obs"; then fail "the worker observer must not read Secrets or reach pod subresources"; fi
has "the observer reads pods and events" 'resources: \[pods, events\]' "${wobs[@]}" "${wobsT[@]}"
has "the observer reads the WorkerPool objects" 'resources: \[workerpools\]' "${wobs[@]}" "${wobsT[@]}"
lacks "the worker observer is off when disabled" 'worker-observer' "${wobs[@]}" --set execution.substrate.workerObserver.enabled=false
