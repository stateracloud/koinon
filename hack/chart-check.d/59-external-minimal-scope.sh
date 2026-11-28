# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154
# Chart assertions: charts/koinon/ci/external-minimal-values.yaml is the namespace-scoped install. Its header names
# the only cluster-scoped objects it may render; any other (a guard switched back on, a new cluster-scoped
# template) fails here. Sourced in order by hack/chart-check.sh.

# scope_cluster_objects prints kind/name of every cluster-scoped object in a render on stdin.
scope_cluster_objects() {
	awk '
		/^---/ { kind = ""; inmeta = 0; next }
		/^kind: / { kind = $2; next }
		/^metadata:/ { inmeta = 1; next }
		/^[^ ]/ { inmeta = 0 }
		inmeta && /^  name: / && kind != "" { name = $2; gsub(/"/, "", name); print kind "/" name; kind = "" }
	' | grep -E '^(Namespace|ClusterRole|ClusterRoleBinding|ValidatingAdmissionPolicy|ValidatingAdmissionPolicyBinding|MutatingAdmissionPolicy|MutatingAdmissionPolicyBinding|ValidatingWebhookConfiguration|MutatingWebhookConfiguration|CustomResourceDefinition|APIService|StorageClass|PriorityClass|RuntimeClass|IngressClass|PersistentVolume|CSIDriver|ClusterTrustBundle)/' | sort -u
}

# The objects it needs (keep in step with the file's header).
scope_allowed="ClusterRole/koinon-koinon-platform-recovery
ClusterRole/koinon-koinon-preflight
ClusterRole/koinon-koinon-reconciler-openshell-config
ClusterRole/koinon-provisioner-koinon
ClusterRoleBinding/koinon-koinon-platform-recovery
ClusterRoleBinding/koinon-koinon-preflight
ClusterRoleBinding/koinon-koinon-reconciler-openshell-config
ClusterRoleBinding/koinon-provisioner-koinon
ValidatingAdmissionPolicy/koinon-koinon-platform-recovery
ValidatingAdmissionPolicyBinding/koinon-koinon-platform-recovery"

# scope_extra <extra helm args>: the cluster-scoped objects of the fixture that are not allowed, one per line.
scope_extra() {
	local r got
	r=$(render_default -f "$CHART/ci/external-minimal-values.yaml" "$@" 2>/dev/null) || { echo "RENDER/failed"; return; }
	got=$(scope_cluster_objects <<<"$r" || true)
	[[ -n $got ]] || { echo "PARSE/none-found"; return; }
	comm -23 <(echo "$got") <(echo "$scope_allowed" | sort)
}
extra=$(scope_extra)
[[ -z $extra ]] || fail "external-minimal-values.yaml renders cluster-scoped objects it may not: $(tr '\n' ' ' <<<"$extra")"
# The check itself: each guard that the fixture turns off, switched back on, must make it fail.
for on in gateway.mcp.koinon.recipientKey.guard.enabled execution.substrate.workerPolicy.enabled execution.substrate.workerPool.enabled \
	secrets.kek.guard.enabled secrets.reconcilerFence.enabled; do
	e=$(scope_extra --set "$on=true")
	[[ -n $e && $e != RENDER/failed && $e != PARSE/none-found ]] || fail "59-external-minimal-scope.sh does not see the cluster-scoped objects of $on=true"
done
