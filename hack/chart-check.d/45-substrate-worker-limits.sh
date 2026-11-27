# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the LimitRange and the ResourceQuota of the Substrate worker namespace (issue #1161).
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

wl=(--set execution.driver=substrate --set gateway.model.driver=koinon --set gateway.mcp.driver=koinon --set execution.substrate.controlAddress=api.ate-system.svc:443
	--set execution.substrate.routerAddress=atenet-router.ate-system.svc:443 --set execution.substrate.tokenAudience=ateapi
	--set execution.substrate.allowUnhardened=true --set execution.substrate.defaultTier=dev
	--set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --set sandboxLimitRange.enabled=false)
lim=(--show-only templates/substrate-workerlimits.yaml)

# Nothing renders with the default driver.
lacks "no worker LimitRange with the openshell driver" 'koinon-worker-(limits|quota)'
# Both objects render in the worker namespace, with safe defaults.
has "worker LimitRange" 'kind: LimitRange' "${wl[@]}" "${lim[@]}"
has "worker ResourceQuota" 'kind: ResourceQuota' "${wl[@]}" "${lim[@]}"
lr=$(render "${wl[@]}" "${lim[@]}" | docof 'kind: LimitRange')
grep -Fq 'namespace: "ate-workers"' <<<"$lr" || fail "the LimitRange must be in the worker namespace"
rq=$(render "${wl[@]}" "${lim[@]}" | docof 'kind: ResourceQuota')
grep -Fq 'namespace: "ate-workers"' <<<"$rq" || fail "the ResourceQuota must be in the worker namespace"
has "the LimitRange sets default requests" 'defaultRequest:' "${wl[@]}" "${lim[@]}"
has "the LimitRange sets default limits" '^      default:' "${wl[@]}" "${lim[@]}"
has "the quota caps the pod count" '"pods": "20"' "${wl[@]}" "${lim[@]}"
has "the quota caps CPU requests" '"requests.cpu": "64"' "${wl[@]}" "${lim[@]}"
has "the quota caps memory limits" '"limits.memory": "256Gi"' "${wl[@]}" "${lim[@]}"
lacks "the default render has no per-container max" '^      max:' "${wl[@]}" "${lim[@]}"
# The values change the render, and the namespace follows workerNamespace.
has "quota values are settable" '"pods": "5"' "${wl[@]}" --set-string execution.substrate.workerLimits.quota.hard.pods=5 "${lim[@]}"
has "limit values are settable" 'cpu: "500m"' "${wl[@]}" --set-string execution.substrate.workerLimits.limitRange.defaultRequest.cpu=500m "${lim[@]}"
has "a per-container max renders" '^      max:' "${wl[@]}" --set-string execution.substrate.workerLimits.limitRange.max.memory=8Gi "${lim[@]}"
has "the limits follow the worker namespace" 'namespace: "pools-x"' "${wl[@]}" --set execution.substrate.workerNamespace=pools-x "${lim[@]}"
# Off: no object. An empty map: no object of that kind.
lacks "workerLimits.enabled=false renders no limits" 'name: koinon-worker-(limits|quota)$' "${wl[@]}" --set execution.substrate.workerLimits.enabled=false
lacks "an empty quota renders no ResourceQuota" 'name: koinon-worker-quota$' "${wl[@]}" --set-json execution.substrate.workerLimits.quota.hard=null
lacks "empty defaults render no LimitRange" 'name: koinon-worker-limits$' "${wl[@]}" --set-json execution.substrate.workerLimits.limitRange.defaultRequest=null --set-json execution.substrate.workerLimits.limitRange.default=null --set-json execution.substrate.workerLimits.quota.hard=null
# Only the limits on: the namespace still renders, because the objects need it.
has "the limits alone render the worker namespace" 'kind: Namespace' "${wl[@]}" --set execution.substrate.workerPolicy.enabled=false --set execution.substrate.workerNetworkPolicy.enabled=false --set execution.substrate.workerPool.enabled=false --show-only templates/substrate-workernamespace.yaml
# A quota value that is not a string or number fails the schema.
if render "${wl[@]}" --set-json execution.substrate.workerLimits.quota.hard='{"pods":[1]}' >/dev/null 2>"$out/wl.err"; then fail "a list as a quota value must fail the schema"; fi
grep -q 'workerLimits' "$out/wl.err" || fail "the schema error must name workerLimits"
# A quota key that no LimitRange default covers would refuse every unsized pod: the render fails.
if render "${wl[@]}" --set-json execution.substrate.workerLimits.limitRange.default=null >/dev/null 2>"$out/wl2.err"; then fail "a limits.* quota without a default limit must fail the render"; fi
grep -q 'limitRange.default' "$out/wl2.err" || fail "the guard error must name limitRange.default"
if render "${wl[@]}" --set-string execution.substrate.workerLimits.quota.hard.requests\\.ephemeral-storage=1Gi >/dev/null 2>"$out/wl3.err"; then fail "a requests.* quota without a default request must fail the render"; fi
# The write guard (native ValidatingAdmissionPolicy): only the installer writes the LimitRange and the ResourceQuota.
has "the limits have a write guard" 'name: koinon-koinon-worker-limits$' "${wl[@]}" "${lim[@]}"
wlg=$(render "${wl[@]}" "${lim[@]}" | docof 'kind: ValidatingAdmissionPolicy\n')
grep -Fq 'resources: [limitranges, resourcequotas]' <<<"$wlg" || fail "the guard must match limitranges and resourcequotas"
grep -Fq 'operations: [CREATE, UPDATE, DELETE]' <<<"$wlg" || fail "the guard must match create, update and delete"
grep -Fq 'failurePolicy: Fail' <<<"$wlg" || fail "the guard must fail closed"
if grep -q 'resourcequotas/status' <<<"$wlg"; then fail "the guard must not match the quota status (the quota controller writes it)"; fi
grep -Fq 'kubeadm:cluster-admins' <<<"$wlg" || fail "the guard must admit the installer groups"
if grep -q 'ate-controller' <<<"$wlg"; then fail "the guard must not admit the Substrate controller"; fi
wlb=$(render "${wl[@]}" "${lim[@]}" | docof 'kind: ValidatingAdmissionPolicyBinding')
grep -Fq 'kubernetes.io/metadata.name: "ate-workers"' <<<"$wlb" || fail "the guard binding must select the worker namespace"
grep -Fq 'validationActions: ["Deny"]' <<<"$wlb" || fail "the guard binding must deny"
has "the guard follows the installer values" 'g in \[\\"ops:gitops\\"\]' "${wl[@]}" --set 'execution.substrate.workerPolicy.installerGroups={ops:gitops}' "${lim[@]}"
lacks "workerPolicy.enabled=false renders no limits guard" 'kind: ValidatingAdmissionPolicy' "${wl[@]}" --set execution.substrate.workerPolicy.enabled=false "${lim[@]}"
lacks "workerLimits.enabled=false renders no limits guard" 'name: koinon-koinon-worker-limits$' "${wl[@]}" --set execution.substrate.workerLimits.enabled=false
