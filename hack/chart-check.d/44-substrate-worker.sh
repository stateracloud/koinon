# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the Substrate worker namespace (ADR 0120 section 6, issue #1089): the worker
# ValidatingAdmissionPolicies, the NetworkPolicy and the default WorkerPool. Sourced in order by
# hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

wsub=(--set execution.driver=substrate --set execution.substrate.controlAddress=api.ate-system.svc:443
	--set execution.substrate.routerAddress=atenet-router.ate-system.svc:80 --set execution.substrate.tokenAudience=ateapi
	--set execution.substrate.allowUnhardened=true --set execution.substrate.defaultTier=dev --set 'execution.substrate.workerNetworkPolicy.apiServerCIDRs={192.0.2.10/32}'
	--set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --set sandboxLimitRange.enabled=false)
pol=(--show-only templates/substrate-policy.yaml)
np=(--show-only templates/substrate-networkpolicy.yaml)
pool=(--show-only templates/substrate-workerpool.yaml)

# Nothing of it renders with the default driver.
lacks "no worker policy with the openshell driver" 'worker-(pods|connect|objects)'
lacks "no worker NetworkPolicy with the openshell driver" 'substrate-workers'
lacks "no WorkerPool with the openshell driver" 'kind: WorkerPool'

# Policies: three kinds of rule, bound to the Substrate namespace and nowhere else.
has "worker pod policy" 'name: koinon-koinon-worker-pods$' "${wsub[@]}" "${pol[@]}"
has "worker CONNECT policy" 'name: koinon-koinon-worker-connect$' "${wsub[@]}" "${pol[@]}"
has "policies are native ValidatingAdmissionPolicies" 'kind: ValidatingAdmissionPolicyBinding' "${wsub[@]}" "${pol[@]}"
lacks "no Kyverno" '[Kk]yverno' "${wsub[@]}" "${pol[@]}"
has "bound to the Substrate namespace" 'kubernetes.io/metadata.name: "ate-system"' "${wsub[@]}" "${pol[@]}"
has "the Substrate namespace is configurable" 'kubernetes.io/metadata.name: "substrate-x"' "${wsub[@]}" --set execution.substrate.namespace=substrate-x --set execution.substrate.gatewayClientNamespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=substrate-x "${pol[@]}"
has "the controller identity defaults to ate-controller in the namespace" 'system:serviceaccount:ate-system:ate-controller' "${wsub[@]}" "${pol[@]}"
has "the controller identity follows the namespace" 'system:serviceaccount:substrate-x:ate-controller' "${wsub[@]}" --set execution.substrate.namespace=substrate-x --set execution.substrate.gatewayClientNamespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=substrate-x "${pol[@]}"
has "the controller identity is configurable" 'system:serviceaccount:other:ctl' "${wsub[@]}" --set 'execution.substrate.workerPolicy.controllerIdentities={system:serviceaccount:other:ctl}' "${pol[@]}"
has "Kubernetes workload controllers may create the control plane's pods" 'system:serviceaccount:kube-system:replicaset-controller' "${wsub[@]}" "${pol[@]}"
has "images are pinned by digest" "@sha256:\[a-f0-9\]\{64\}" "${wsub[@]}" "${pol[@]}"
has "hostNetwork is denied" 'hostNetwork is not allowed' "${wsub[@]}" "${pol[@]}"
has "hostPath is limited to the documented set" 'v.hostPath.path in \[\\"/dev/kvm\\",\\"/dev/net/tun\\",\\"/dev/vhost-vsock\\",\\"/dev/vhost-net\\",\\"/var/lib/ate\\"\]' "${wsub[@]}" "${pol[@]}"
has "ephemeral containers are denied" "request.subResource != 'ephemeralcontainers'" "${wsub[@]}" "${pol[@]}"
has "exec, attach, port-forward, pod proxy and service proxy are denied" 'resources: \[pods/exec, pods/attach, pods/portforward, pods/proxy, services/proxy\]' "${wsub[@]}" "${pol[@]}"
has "the validation actions deny" 'validationActions: \["Deny"\]' "${wsub[@]}" "${pol[@]}"
tag=$(render "${wsub[@]}" "${pol[@]}")
grep -Eq -- 'name: tagAllowed' <<<"$tag" || fail "the digest exemption variable is missing"
tagline=$(grep -A1 -E 'name: tagAllowed' <<<"$tag" | tail -1)
grep -Fq 'replicaset-controller' <<<"$tagline" || fail "the digest exemption must default to the platform controllers"
if grep -Fq 'ate-controller' <<<"$tagline"; then fail "the Substrate controller must not be exempt from the digest rule by default"; fi
tagline=$(render "${wsub[@]}" --set 'execution.substrate.workerPolicy.digestExemptIdentities={system:serviceaccount:x:y}' "${pol[@]}" | grep -A1 -E 'name: tagAllowed' | tail -1)
grep -Fq 'system:serviceaccount:x:y' <<<"$tagline" || fail "digestExemptIdentities must set the exemption"
if grep -Fq 'replicaset-controller' <<<"$tagline"; then fail "digestExemptIdentities must replace the default list"; fi
has "hostPID and hostIPC are denied" 'hostPID and hostIPC are not allowed' "${wsub[@]}" "${pol[@]}"
has "kata annotations are denied" 'io.katacontainers' "${wsub[@]}" "${pol[@]}"
has "untrusted identities cannot relabel a worker pod" 'may change the labels of a pod' "${wsub[@]}" "${pol[@]}"
has "the object guard is on by default" 'name: koinon-koinon-worker-objects$' "${wsub[@]}" "${pol[@]}"
has "the object guard covers the three Substrate kinds" 'resources: \[workerpools, sandboxconfigs, csidriverconfigs\]' "${wsub[@]}" "${pol[@]}"
has "the object guard covers the workload kinds" 'resources: \[deployments, replicasets, daemonsets, statefulsets\]' "${wsub[@]}" "${pol[@]}"
has "the object guard covers jobs and cronjobs" 'resources: \[jobs, cronjobs\]' "${wsub[@]}" "${pol[@]}"
has "the installer group defaults to system:masters" 'g in \[\\"system:masters\\",\\"kubeadm:cluster-admins\\"\]' "${wsub[@]}" "${pol[@]}"
has "installer identities are configurable" 'system:admin' "${wsub[@]}" --set 'execution.substrate.workerPolicy.installerIdentities={system:admin}' "${pol[@]}"
has "the controller may change metadata of Substrate objects only" "request.operation == 'UPDATE' && request.resource.group == 'ate.dev' && has\(object.spec\) && has\(oldObject.spec\) && object.spec == oldObject.spec" "${wsub[@]}" "${pol[@]}"
lacks "the policies are off when disabled" 'worker-(pods|connect|objects)' "${wsub[@]}" --set execution.substrate.workerPolicy.enabled=false
if render "${wsub[@]}" --set 'execution.substrate.workerPolicy.validationActions={Nope}' >/dev/null 2>&1; then fail "an unknown validation action must not render"; fi

# NetworkPolicy: default-deny shape in the Substrate namespace, standard API only.
has "worker NetworkPolicy in the Substrate namespace" 'namespace: "ate-system"' "${wsub[@]}" "${np[@]}"
has "worker NetworkPolicy covers ingress and egress" 'policyTypes: \[Ingress, Egress\]' "${wsub[@]}" "${np[@]}"
has "worker NetworkPolicy allows DNS" 'port: 53' "${wsub[@]}" "${np[@]}"
has "worker NetworkPolicy admits Koinon's namespace" 'kubernetes.io/metadata.name: "koinon"' "${wsub[@]}" "${np[@]}"
has "worker egress to Koinon names the gateway pods" 'app.kubernetes.io/component: gateway' "${wsub[@]}" "${np[@]}"
has "worker egress to Koinon names the API pods" 'app.kubernetes.io/component: api' "${wsub[@]}" "${np[@]}"
lacks "worker egress does not name the database" 'postgres|cnpg' "${wsub[@]}" "${np[@]}"
lacks "worker NetworkPolicy has no public egress" '0\.0\.0\.0/0|::/0' "${wsub[@]}" "${np[@]}"
has "the default API server rule uses the CIDR" 'cidr: "192.0.2.10/32"' "${wsub[@]}" "${np[@]}"
# Worker pods (label ate.dev/worker-pool) get no API server route; the control-plane pods get it.
has "the worker label is the pool label" 'key: ate.dev/worker-pool' "${wsub[@]}" "${np[@]}"
npdocs=$(render "${wsub[@]}" "${np[@]}")
workers=$(awk '/^---/{d++} d>=1 && /name: koinon-substrate-workers/{f=1} f' <<<"$npdocs" | awk 'NR>1 && /^---/{exit} {print}')
[[ $(grep -c '^kind: NetworkPolicy' <<<"$npdocs") == 2 ]] || fail "expected two NetworkPolicies (workers and control plane)"
grep -Fq 'operator: Exists' <<<"$workers" || fail "the workers policy must select pods that have the worker label"
grep -Fq 'ate.dev/worker-pool' <<<"$workers" || fail "the workers policy must select the worker label"
if grep -Eq '192\.0\.2\.10|6443|ipBlock' <<<"$workers"; then fail "worker pods must have no API server route"; fi
control=$(awk '/name: koinon-substrate-control/{f=1} f' <<<"$npdocs")
grep -Fq 'operator: DoesNotExist' <<<"$control" || fail "the control-plane policy must select pods without the worker label"
grep -Fq 'cidr: "192.0.2.10/32"' <<<"$control" || fail "the control-plane policy must carry the API server route"
if render "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.apiServerCIDRs=null' >/dev/null 2>"$out/sub.err"; then fail "an empty apiServerCIDRs must not render"; fi
grep -q 'apiServerCIDRs' "$out/sub.err" || fail "the empty-CIDR error must name apiServerCIDRs"
if render "${wsub[@]}" --set execution.substrate.namespace=substrate-x >/dev/null 2>"$out/sub.err"; then fail "a namespace that differs from gatewayClientNamespaceSelector must not render"; fi
grep -q 'gatewayClientNamespaceSelector' "$out/sub.err" || fail "the namespace-mismatch error must name gatewayClientNamespaceSelector"
has "a matching namespace and selector render" 'namespace: "substrate-x"' "${wsub[@]}" --set execution.substrate.namespace=substrate-x --set execution.substrate.gatewayClientNamespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=substrate-x "${np[@]}"
has "replication controllers are guarded" 'replicationcontrollers' "${wsub[@]}" "${pol[@]}"
has "the digest rule skips metadata-only updates" "object.spec == oldObject.spec) || variables.tagAllowed" "${wsub[@]}" "${pol[@]}"
has "the API server rule follows apiServerCIDRs" 'cidr: "10.0.0.1/32"' "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.apiServerCIDRs={10.0.0.1/32}' "${np[@]}"
lacks "no CNI-specific resource" 'cilium|calico|CiliumNetworkPolicy' "${wsub[@]}" "${np[@]}"
lacks "the NetworkPolicy is off when disabled" 'substrate-workers' "${wsub[@]}" --set execution.substrate.workerNetworkPolicy.enabled=false

# WorkerPool (Substrate v1.0.0 WorkerPoolSpec): off by default; workerImage is required; one sandbox
# class; placement and the pull ServiceAccount live under spec.template; the spec has no imagePullSecrets.
wp=(--set execution.substrate.workerPool.enabled=true --set execution.substrate.workerPool.workerImage=registry.example/ateworker-gvisor:v1.0.0)
lacks "no WorkerPool by default" 'kind: WorkerPool' "${wsub[@]}"
if render "${wsub[@]}" --set execution.substrate.workerPool.enabled=true >/dev/null 2>"$out/sub.err"; then fail "a WorkerPool without workerImage must not render"; fi
grep -q 'workerImage' "$out/sub.err" || fail "the missing-image error must name workerImage"
has "the default WorkerPool" 'kind: WorkerPool' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the WorkerPool is in the Substrate namespace" 'namespace: "ate-system"' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the WorkerPool carries workerImage" 'workerImage: registry.example/ateworker-gvisor:v1.0.0' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the default pool is named after its class" 'name: "koinon-gvisor"' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the pool name follows the class" 'name: "koinon-microvm"' "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.sandboxClass=microvm "${pool[@]}"
lacks "no ServiceAccount when spec sets one" 'kind: ServiceAccount' "${wsub[@]}" "${wp[@]}" --set 'global.imagePullSecrets={regcred}' --set execution.substrate.workerPool.spec.template.serviceAccountName=mine "${pool[@]}"
has "the controller may apply only Deployments, with a checked template" "request.resource.resource == 'deployments' && \\(request.operation == 'DELETE' \\|\\| \\(!has\\(object.spec.template.spec.hostNetwork\\)" "${wsub[@]}" "${pol[@]}"
has "the Deployment template hostPath follows allowedHostPaths" 'object.spec.template.spec.volumes.all\(v, !has\(v.hostPath\) \|\| v.hostPath.path in' "${wsub[@]}" "${pol[@]}"
has "the WorkerPool default class is gvisor" 'name: gvisor' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the WorkerPool class is configurable" 'name: microvm' "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.sandboxClass=microvm "${pool[@]}"
if render "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.sandboxClass=kata >/dev/null 2>&1; then fail "an unknown sandbox class must not render"; fi
lacks "the WorkerPool has exactly one class" 'name: microvm' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
lacks "the WorkerPool spec has no imagePullSecrets or top-level nodeSelector" '^  (imagePullSecrets|nodeSelector|tolerations):' "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.nodeSelector.pool=vm --set 'global.imagePullSecrets={regcred}' "${pool[@]}"
has "nodeSelector sits under spec.template" '^    nodeSelector:' "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.nodeSelector.pool=vm "${pool[@]}"
has "the pull secrets go to a ServiceAccount" 'kind: ServiceAccount' "${wsub[@]}" "${wp[@]}" --set 'global.imagePullSecrets={regcred}' "${pool[@]}"
has "the ServiceAccount carries the pull secret (ADR 0061)" 'name: regcred' "${wsub[@]}" "${wp[@]}" --set 'global.imagePullSecrets={regcred}' "${pool[@]}"
has "the pool runs as that ServiceAccount" 'serviceAccountName: "?koinon-gvisor-worker"?' "${wsub[@]}" "${wp[@]}" --set 'global.imagePullSecrets={regcred}' "${pool[@]}"
has "the chart ServiceAccount exists without pull secrets" 'kind: ServiceAccount' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
lacks "no pull secret on it when none is set" 'imagePullSecrets' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the worker ServiceAccount mounts no API token" 'automountServiceAccountToken: false' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the worker ServiceAccount mounts no API token with pull secrets" 'automountServiceAccountToken: false' "${wsub[@]}" "${wp[@]}" --set 'global.imagePullSecrets={regcred}' "${pool[@]}"
has "a named ServiceAccount is used as is" 'serviceAccountName: "?my-sa"?' "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.serviceAccountName=my-sa "${pool[@]}"
has "the WorkerPool spec override merges" 'foo: bar' "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.spec.foo=bar "${pool[@]}"

# Admission against a real Substrate install (kind, v1.0.0): the worker's base directory, the controller's
# Deployment writes and the kubeadm/kind installer group.
has "the worker base directory is allowed" '/var/lib/ate' "${wsub[@]}" "${pol[@]}"
has "the controller may write Deployments" "request.resource.group == 'apps' && request.resource.resource == 'deployments'" "${wsub[@]}" "${pol[@]}"
has "the kubeadm admin group is an installer by default" 'kubeadm:cluster-admins' "${wsub[@]}" "${pol[@]}"
has "system:masters stays an installer" 'system:masters' "${wsub[@]}" "${pol[@]}"
