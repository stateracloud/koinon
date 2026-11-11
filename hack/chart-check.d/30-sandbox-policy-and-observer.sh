# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the CONNECT policy and the sandbox observer.
# Sourced in order by hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

# The CONNECT policy (pods/exec, attach, portforward, pods/proxy, services/proxy) and its binding
# render in every mode that has the sandbox policies, with no exemption (no variables, no identity list), ADR 0048 decision 3.
for f in "${value_files[@]}"; do
	r="$out/$(basename "$f" .yaml).yaml"
	# A value set that turns the policies off (the cluster-scoped objects need admin rights) has none.
	grep -Eq -- '^kind: ValidatingAdmissionPolicy$' "$r" || { grep -A1 -E '^sandboxPolicy:' "$f" | grep -q 'enabled: false' && continue; }
	grep -Eq -- '^  name: koinon-koinon-sandbox-connect$' "$r" || fail "no CONNECT policy with $(basename "$f")"
	[ "$(grep -c -- '^  name: koinon-koinon-sandbox-connect$' "$r")" = 2 ] || fail "CONNECT policy and binding must both render with $(basename "$f")"
	grep -Eq -- 'operations: \[CONNECT\]' "$r" || fail "no CONNECT rule with $(basename "$f")"
	grep -Eq -- 'resources: \[pods/exec, pods/attach, pods/portforward, pods/proxy, services/proxy\]' "$r" || fail "CONNECT rule lacks a subresource with $(basename "$f")"
done
lacks "no CONNECT policy with sandboxPolicy.enabled=false" 'sandbox-connect' --set sandboxPolicy.enabled=false --set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false

# The sandbox observer (ADR 0078): on by default; renders a get-only ClusterRole, the binder ClusterRole
# and its binding, and the native policy that fences the reconciler's RoleBindings; off renders none.
obs=templates/sandbox-observer.yaml
role_rules() { # <ClusterRole name> <helm args...>: "resources verbs resourceNames" per rule, sorted, ';'-joined
	local name=$1
	shift
	render --show-only "$obs" "$@" | awk -v n="$name" '
		BEGIN { RS = "\n---\n" }
		$0 ~ "kind: ClusterRole\n" && $0 ~ "\n  name: " n "\n" { print }' |
		awk '/^ *- apiGroups:/ { g = $0; sub(/^ *- apiGroups: */, "", g) }
		     /^ *resources:/ { r = $0; sub(/^ *resources: */, "", r) }
		     /^ *resourceNames:/ { rn = $0; sub(/^ *resourceNames: */, "", rn) }
		     /^ *verbs:/ { v = $0; sub(/^ *verbs: */, "", v); print g " " r " " v (rn == "" ? "" : " names=" rn); rn = "" }' |
		LC_ALL=C sort | tr '\n' ';'
}
want='[""] [pods] [get];[agents.x-k8s.io] [sandboxes] [get];'
got=$(role_rules koinon-koinon-sandbox-observer)
[[ $got == "$(LC_ALL=C sort < <(tr ';' '\n' <<<"$want" | grep .) | tr '\n' ';')" ]] || fail "the observer ClusterRole rules are '$got', want exactly get on pods and agents.x-k8s.io sandboxes"
want='[rbac.authorization.k8s.io] [clusterroles] [bind] names=[koinon-koinon-sandbox-observer];[rbac.authorization.k8s.io] [rolebindings] [create];'
got=$(role_rules koinon-koinon-sandbox-observer-binder)
[[ $got == "$want" ]] || fail "the binder ClusterRole rules are '$got', want create on rolebindings and bind on the observer ClusterRole only"
# The observer ClusterRole itself is never bound cluster-wide: the one ClusterRoleBinding is the binder's.
[[ $(grep -c -- '^kind: ClusterRoleBinding$' <<<"$(render --show-only "$obs")") == 1 ]] || fail "exactly one ClusterRoleBinding (the binder to the reconciler) with the observer on"
bound=$(awk 'BEGIN{RS="\n---\n"} /kind: ClusterRoleBinding/' <<<"$(render --show-only "$obs")")
grep -Fq -- 'name: koinon-koinon-sandbox-observer-binder' <<<"$bound" && ! grep -Eq -- '^  name: koinon-koinon-sandbox-observer$' <<<"$bound" || fail "the ClusterRoleBinding must reference the binder only"
grep -Fq -- 'name: koinon-reconciler' <<<"$bound" || fail "the binder is bound to the reconciler ServiceAccount"
has "the observer policy fails closed" 'failurePolicy: Fail' --show-only "$obs"
has "the observer policy covers RoleBinding create and update" 'operations: \[CREATE, UPDATE\]' --show-only "$obs"
has "the observer policy binding denies" 'validationActions: \["Deny"\]' --show-only "$obs"
has "the observer policy keys on the reconciler identity" 'system:serviceaccount:koinon:koinon-reconciler' --show-only "$obs"
has "the observer policy limits bindings to the configured gateway ID" "openshell.ai/gateway-id'\] == \"prod\"" --set openshell.gatewayId=prod --show-only "$obs"
# Old issue 460: the policy bindings cover the release's own gateway's namespaces only.
bindings=$(render | awk 'BEGIN{RS="\n---\n"} /kind: ValidatingAdmissionPolicyBinding/')
[[ $(grep -c -- '^  name: .*-sandbox-\(pods\|objects\|connect\)\(-unlabelled\)\{0,1\}$' <<<"$bindings") == 6 ]] || fail "6 sandbox policy bindings: 3 for the gateway's namespaces, 3 for namespaces without a gateway-id label"
[[ $(grep -c -- 'openshell.ai/gateway-id: openshell$' <<<"$bindings") == 3 ]] || fail "all 3 sandbox policy bindings select openshell.ai/gateway-id=openshell by default"
[[ $(render --set openshell.gatewayId=prod | awk 'BEGIN{RS="\n---\n"} /kind: ValidatingAdmissionPolicyBinding/' | grep -c -- 'openshell.ai/gateway-id: prod$') == 3 ]] || fail "the bindings follow openshell.gatewayId"
[[ $(render --set sandboxPolicy.scopeToGateway=false | awk 'BEGIN{RS="\n---\n"} /kind: ValidatingAdmissionPolicyBinding/' | grep -c -- 'gateway-id') == 0 ]] || fail "sandboxPolicy.scopeToGateway=false keeps the old scope (no gateway-id label)"
[[ $(grep -c -- '^  name: .*-sandbox-\(pods\|objects\|connect\)-unlabelled$' <<<"$bindings") == 3 ]] || fail "each sandbox policy has an -unlabelled binding while scoped to the gateway"
[[ $(grep -c -- 'operator: DoesNotExist$' <<<"$bindings") == 3 ]] || fail "the unlabelled bindings select namespaces without openshell.ai/gateway-id"
[[ $(render --set sandboxPolicy.scopeToGateway=false | grep -c -- '-sandbox-.*-unlabelled$') == 0 ]] || fail "sandboxPolicy.scopeToGateway=false renders no unlabelled bindings"
[[ $(grep -c -- 'openshell.ai/managed-by: openshell$' <<<"$bindings") == 6 ]] || fail "the bindings keep the managed-by label"
has "the observer policy takes the sandbox namespaces from sandboxPolicy.namespaceSelector" 'variables.labels\[\\"openshell.ai/managed-by\\"\] == \\"openshell\\"' --show-only "$obs"
has "a label added to sandboxPolicy.namespaceSelector reaches the observer policy" 'variables.labels\[\\"team\\"\] == \\"sandboxes\\"' --set sandboxPolicy.namespaceSelector.matchLabels.team=sandboxes --show-only "$obs"
has "a matchExpression of sandboxPolicy.namespaceSelector reaches the observer policy" 'variables.labels\[\\"tier\\"\] in \[\\"a\\",\\"b\\"\]' \
	--set 'sandboxPolicy.namespaceSelector.matchExpressions[0].key=tier' --set 'sandboxPolicy.namespaceSelector.matchExpressions[0].operator=In' \
	--set 'sandboxPolicy.namespaceSelector.matchExpressions[0].values={a,b}' --show-only "$obs"
if render --set 'sandboxPolicy.namespaceSelector.matchExpressions[0].key=x' --set 'sandboxPolicy.namespaceSelector.matchExpressions[0].operator=Gt' >/dev/null 2>&1; then fail "an unsupported namespaceSelector operator must not render with the observer"; fi
has "the observer policy requires the workspace namespace name" 'namespaceObject.metadata.name.startsWith\("openshell-openshell-"\)' --show-only "$obs"
has "the observer policy requires the sandbox-workspace label" "'openshell.ai/sandbox-workspace' in variables.labels" --show-only "$obs"
has "the observer policy refuses Koinon's own namespace" 'namespaceObject.metadata.name != "koinon"' --show-only "$obs"
has "the observer policy matches the observer role of any kind" 'has\(object.roleRef\) && object.roleRef.name == ' --show-only "$obs"
has "the observer policy fixes the roleRef" 'object.roleRef.name == "koinon-koinon-sandbox-observer"' --show-only "$obs"
has "the observer policy fixes the subject" 'size\(object.subjects\) == 1' --show-only "$obs"
has "the observer policy fences other identities" "only the Koinon reconciler may bind the sandbox observer role" --show-only "$obs"
has "the reconciler is told the observer is on" 'enabled: true' --show-only templates/config.yaml
has "the reconciler is told the observer ClusterRole" 'clusterRole: koinon-koinon-sandbox-observer' --show-only templates/config.yaml
has "the reconciler is told its ServiceAccount" 'serviceAccount: koinon-reconciler' --show-only templates/config.yaml
has "the reconciler is told the OpenShell gateway ID" 'gatewayId: "openshell"' --show-only templates/config.yaml
has "openshell.gatewayId reaches the reconciler config" 'gatewayId: "prod"' --set openshell.gatewayId=prod --show-only templates/config.yaml
off=(--set sandboxObserver.enabled=false)
lacks "sandboxObserver.enabled=false renders no observer role" 'name: koinon-koinon-sandbox-observer' "${off[@]}"
lacks "sandboxObserver.enabled=false renders no binder" 'rolebindings' "${off[@]}"
has "sandboxObserver.enabled=false tells the reconciler" 'enabled: false' "${off[@]}" --show-only templates/config.yaml
lacks "sandboxObserver.enabled=false names no ClusterRole to the reconciler" 'clusterRole:' "${off[@]}" --show-only templates/config.yaml
has "sandboxObserver.enabled=false keeps the sandbox policies" 'name: koinon-koinon-sandbox-pods' "${off[@]}"
has "the preflight compares the gateway ID" '--openshell-gateway-id=prod' --set openshell.gatewayId=prod --show-only templates/preflight.yaml
has "the preflight defaults the gateway ID to OpenShell's" '--openshell-gateway-id=openshell' --show-only templates/preflight.yaml
has "the preflight fails a gateway ID mismatch while the policies are scoped to the gateway" '--sandbox-policy-scoped-to-gateway' --show-only templates/preflight.yaml
lacks "the preflight does not fail a gateway ID mismatch when scopeToGateway is off" '--sandbox-policy-scoped-to-gateway' --set sandboxPolicy.scopeToGateway=false --show-only templates/preflight.yaml
lacks "the preflight does not warn about the observer when it is on" 'sandbox-observer-disabled' --show-only templates/preflight.yaml
has "the preflight warns when the observer is off" '--sandbox-observer-disabled' "${off[@]}" --show-only templates/preflight.yaml
if render --set sandboxPolicy.enabled=false >/dev/null 2>"$out/obs.err"; then fail "the observer without sandboxPolicy must not render"; fi
grep -q 'sandboxObserver.enabled needs sandboxPolicy.enabled' "$out/obs.err" || fail "the observer-needs-policy error must name both settings"
render --set sandboxPolicy.enabled=false "${off[@]}" --set sandboxNetworkPolicy.enabled=false >/dev/null 2>&1 || fail "the sandbox policy off with the observer and NetworkPolicy writer off must render"
if render --set 'sandboxPolicy.validationActions={Warn}' >/dev/null 2>&1; then fail "the observer without a Deny validation action must not render"; fi
# sandboxNetworkPolicy (on by default) also requires Deny; turn both fences off to prove Warn alone can render.
render --set 'sandboxPolicy.validationActions={Warn}' "${off[@]}" --set sandboxNetworkPolicy.enabled=false >/dev/null 2>&1 || fail "validation actions without Deny and the observer and NetworkPolicy writer off must render"
if render --set openshell.gatewayId=Not_Valid >/dev/null 2>&1; then fail "an invalid openshell.gatewayId must not render"; fi


# The SPIRE agent socket never reaches a sandbox (ADR 0097 decision 11.2, old issue 282): the pods policy
# refuses the SPIFFE CSI driver and a hostPath naming the socket, with its own messages.
has "the pods policy refuses SPIFFE CSI volumes" "SPIFFE CSI volumes \(csi.spiffe.io\) are not allowed in sandbox pods"
has "the pods policy refuses the SPIRE agent socket hostPath" "the SPIRE agent socket must not be mounted in sandbox pods"
has "the pods policy matches the csi.spiffe.io driver" "v.csi.driver != 'csi.spiffe.io'"
lacks "no SPIFFE rule without the sandbox policy" 'csi.spiffe.io' --set sandboxPolicy.enabled=false --set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false

# Old issue 475: a sandbox pod mounts only the PersistentVolumeClaim its own Sandbox made (the
# controller names it <volumeClaimTemplate name>-<sandbox name>), and no generic ephemeral volume.
has "the pods policy derives the owning Sandbox from ownerReferences" "o.kind == 'Sandbox'.*startsWith\('agents.x-k8s.io/'\)"
has "the pods policy checks claimName against volume name and owner" "claimName == v.name .* variables.owningSandboxes\[0\]"
has "the pods policy refuses foreign claims" "sandbox pods may mount only the PersistentVolumeClaim their Sandbox made"
has "the pods policy refuses generic ephemeral volumes" "generic ephemeral volumes are not allowed in sandbox pods"
has "the pods policy refuses a hyphen in a claim volume name" "!v.name.contains\\('-'\\)"
has "the pods policy checks claims on CREATE only" "request.operation != 'CREATE' \\|\\| !has\\(object.spec.volumes\\)"
has "the objects policy refuses hyphens in volumeClaimTemplate names" "must not contain '-'"
has "the objects policy refuses a volumeClaimTemplate volumeName" "must not set spec.volumeName"
lacks "no claim rule without the sandbox policy" 'owningSandboxes' --set sandboxPolicy.enabled=false --set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false

# Old issue 475: the namespace label policy. The sandbox policies select namespaces by label, so
# relabelling a namespace would take it out of all of them; only sandboxPolicy.identities.namespaces
# may change the labels the selectors use. It binds to every namespace UPDATE (a namespaceSelector
# reads the NEW object's labels) and judges the OLD object's labels itself.
nsp=templates/sandbox-policy.yaml
has "the namespace label policy renders" '^  name: koinon-koinon-sandbox-namespaces$' --show-only "$nsp"
has "the namespace label policy fails closed" 'failurePolicy: Fail' --show-only "$nsp"
has "the namespace label policy covers namespace updates only" 'operations: \[UPDATE\]' --show-only "$nsp"
has "the namespace label policy names the namespace lifecycle owner" 'request.userInfo.username in \[\\"system:serviceaccount:openshell:openshell\\"\]' --show-only "$nsp"
has "the namespace label policy guards gateway-id and managed-by" '\[\\"openshell.ai/gateway-id\\",\\"openshell.ai/managed-by\\"\]' --show-only "$nsp"
has "the namespace label policy judges the old object" 'variables.oldLabels\[\\"openshell.ai/managed-by\\"\] == \\"openshell\\"' --show-only "$nsp"
has "the namespace label policy follows openshell.gatewayId" 'variables.oldLabels\[\\"openshell.ai/gateway-id\\"\] == \\"prod\\"' --set openshell.gatewayId=prod --show-only "$nsp"
has "the namespace label policy follows sandboxPolicy.identities.namespaces" 'request.userInfo.username in \[\\"system:serviceaccount:ns:own\\"\]' --set-json 'sandboxPolicy.identities.namespaces=["system:serviceaccount:ns:own"]' --show-only "$nsp"
has "a key of sandboxPolicy.namespaceSelector is guarded" '\[\\"openshell.ai/gateway-id\\",\\"openshell.ai/managed-by\\",\\"team\\"\]' --set sandboxPolicy.namespaceSelector.matchLabels.team=sandboxes --show-only "$nsp"
# Issue 507: a pre-filter on the OLD object, so namespaces without a guarded label never reach the CEL.
has "the namespace label policy pre-filters on the old object's guarded labels" 'name: old-object-has-a-guarded-label' --show-only "$nsp"
has "the pre-filter reads the old labels map with has()" 'has\(oldObject.metadata.labels\) && \[\\"openshell.ai/gateway-id\\",\\"openshell.ai/managed-by\\"\]\.exists\(k, k in oldObject.metadata.labels\)' --show-only "$nsp"
has "the pre-filter follows sandboxPolicy.namespaceSelector" 'has\(oldObject.metadata.labels\) && \[\\"openshell.ai/gateway-id\\",\\"openshell.ai/managed-by\\",\\"team\\"\]\.exists' --set sandboxPolicy.namespaceSelector.matchLabels.team=sandboxes --show-only "$nsp"
# A selector with only NotIn / DoesNotExist covers a namespace without the keys, so no pre-filter (review of 507).
neg='sandboxPolicy.namespaceSelector={"matchLabels":null,"matchExpressions":[{"key":"koinon.dev/exempt","operator":"DoesNotExist"}]}'
lacks "a negative-only selector renders no pre-filter" 'old-object-has-a-guarded-label|matchConditions' --set sandboxPolicy.scopeToGateway=false --set-json "$neg" --show-only "$nsp"
has "a negative-only selector still renders the namespace label policy" '^  name: koinon-koinon-sandbox-namespaces$' --set sandboxPolicy.scopeToGateway=false --set-json "$neg" --show-only "$nsp"
lacks "a negative-only selector scoped to the gateway renders no pre-filter" 'old-object-has-a-guarded-label' --set-json "$neg" --show-only "$nsp"
has "an Exists selector keeps the pre-filter" 'name: old-object-has-a-guarded-label' --set-json 'sandboxPolicy.namespaceSelector={"matchLabels":null,"matchExpressions":[{"key":"koinon.dev/sb","operator":"Exists"}]}' --set sandboxPolicy.scopeToGateway=false --show-only "$nsp"
nsb=$(render --show-only "$nsp" | awk 'BEGIN{RS="\n---\n"} /kind: ValidatingAdmissionPolicyBinding/ && /sandbox-namespaces/')
[[ -n $nsb ]] || fail "the namespace label policy has a binding"
! grep -Eq -- 'namespaceSelector|objectSelector' <<<"$nsb" || fail "the namespace label policy binds without a selector (it reads the old object itself)"
grep -Fq -- 'validationActions: ["Deny"]' <<<"$nsb" || fail "the namespace label binding denies"
[[ $(render --show-only "$nsp" | grep -c -- '^  name: koinon-koinon-sandbox-namespaces$') == 2 ]] || fail "the namespace label policy and binding render once each"
lacks "no namespace label policy with sandboxPolicy.enabled=false" 'sandbox-namespaces' --set sandboxPolicy.enabled=false --set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false
