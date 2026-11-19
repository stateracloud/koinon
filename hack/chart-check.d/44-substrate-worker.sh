# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the Substrate worker namespace (ADR 0120 section 6, issue #1089): the worker
# ValidatingAdmissionPolicies, the NetworkPolicy and the default WorkerPool. Sourced in order by
# hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

wsub=(--set execution.driver=substrate --set execution.substrate.controlAddress=api.ate-system.svc:443
	--set execution.substrate.routerAddress=atenet-router.ate-system.svc:80 --set execution.substrate.tokenAudience=ateapi
	--set execution.substrate.allowUnhardened=true
	--set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --set sandboxLimitRange.enabled=false)
pol=(--show-only templates/substrate-policy.yaml)
np=(--show-only templates/substrate-networkpolicy.yaml)
pool=(--show-only templates/substrate-workerpool.yaml)

# Nothing of it renders with the default driver.
lacks "no worker policy with the openshell driver" 'worker-pods|worker-connect'
lacks "no worker NetworkPolicy with the openshell driver" 'substrate-workers'
lacks "no WorkerPool with the openshell driver" 'kind: WorkerPool'

# Policies: three kinds of rule, bound to the Substrate namespace and nowhere else.
has "worker pod policy" 'name: koinon-koinon-worker-pods$' "${wsub[@]}" "${pol[@]}"
has "worker CONNECT policy" 'name: koinon-koinon-worker-connect$' "${wsub[@]}" "${pol[@]}"
has "policies are native ValidatingAdmissionPolicies" 'kind: ValidatingAdmissionPolicyBinding' "${wsub[@]}" "${pol[@]}"
lacks "no Kyverno" '[Kk]yverno' "${wsub[@]}" "${pol[@]}"
has "bound to the Substrate namespace" 'kubernetes.io/metadata.name: "ate-system"' "${wsub[@]}" "${pol[@]}"
has "the Substrate namespace is configurable" 'kubernetes.io/metadata.name: "substrate-x"' "${wsub[@]}" --set execution.substrate.namespace=substrate-x "${pol[@]}"
has "the controller identity defaults to ate-controller in the namespace" 'system:serviceaccount:ate-system:ate-controller' "${wsub[@]}" "${pol[@]}"
has "the controller identity follows the namespace" 'system:serviceaccount:substrate-x:ate-controller' "${wsub[@]}" --set execution.substrate.namespace=substrate-x "${pol[@]}"
has "the controller identity is configurable" 'system:serviceaccount:other:ctl' "${wsub[@]}" --set 'execution.substrate.workerPolicy.controllerIdentities={system:serviceaccount:other:ctl}' "${pol[@]}"
has "Kubernetes workload controllers may create the control plane's pods" 'system:serviceaccount:kube-system:replicaset-controller' "${wsub[@]}" "${pol[@]}"
has "images are pinned by digest" "@sha256:\[a-f0-9\]\{64\}" "${wsub[@]}" "${pol[@]}"
has "hostNetwork is denied" 'hostNetwork is not allowed' "${wsub[@]}" "${pol[@]}"
has "hostPath is limited to the documented set" 'v.hostPath.path in \[\\"/dev/kvm\\",\\"/dev/net/tun\\",\\"/dev/vhost-vsock\\",\\"/dev/vhost-net\\"\]' "${wsub[@]}" "${pol[@]}"
has "ephemeral containers are denied" "request.subResource != 'ephemeralcontainers'" "${wsub[@]}" "${pol[@]}"
has "exec, attach, port-forward and proxy are denied" 'resources: \[pods/exec, pods/attach, pods/portforward, pods/proxy\]' "${wsub[@]}" "${pol[@]}"
has "the validation actions deny" 'validationActions: \["Deny"\]' "${wsub[@]}" "${pol[@]}"
has "the digest exemption is the platform controllers by default" 'name: tagAllowed' "${wsub[@]}" "${pol[@]}"
lacks "no object policy without installer identities" 'worker-objects' "${wsub[@]}"
has "worker objects are guarded when installer identities are set" 'name: koinon-koinon-worker-objects$' "${wsub[@]}" --set 'execution.substrate.workerPolicy.installerIdentities={system:admin}' "${pol[@]}"
has "the object guard covers the three kinds" 'resources: \[workerpools, sandboxconfigs, csidriverconfigs\]' "${wsub[@]}" --set 'execution.substrate.workerPolicy.installerIdentities={system:admin}' "${pol[@]}"
lacks "the policies are off when disabled" 'worker-pods|worker-connect' "${wsub[@]}" --set execution.substrate.workerPolicy.enabled=false
if render "${wsub[@]}" --set 'execution.substrate.workerPolicy.validationActions={Nope}' >/dev/null 2>&1; then fail "an unknown validation action must not render"; fi

# NetworkPolicy: default-deny shape in the Substrate namespace, standard API only.
has "worker NetworkPolicy in the Substrate namespace" 'namespace: "ate-system"' "${wsub[@]}" "${np[@]}"
has "worker NetworkPolicy covers ingress and egress" 'policyTypes: \[Ingress, Egress\]' "${wsub[@]}" "${np[@]}"
has "worker NetworkPolicy allows DNS" 'port: 53' "${wsub[@]}" "${np[@]}"
has "worker NetworkPolicy admits Koinon's namespace" 'kubernetes.io/metadata.name: "koinon"' "${wsub[@]}" "${np[@]}"
lacks "worker NetworkPolicy has no public egress" '0\.0\.0\.0/0|::/0' "${wsub[@]}" "${np[@]}"
lacks "no API server rule without CIDRs" 'ipBlock' "${wsub[@]}" "${np[@]}"
has "the API server rule follows apiServerCIDRs" 'cidr: "10.0.0.1/32"' "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.apiServerCIDRs={10.0.0.1/32}' "${np[@]}"
lacks "no CNI-specific resource" 'cilium|calico|CiliumNetworkPolicy' "${wsub[@]}" "${np[@]}"
lacks "the NetworkPolicy is off when disabled" 'substrate-workers' "${wsub[@]}" --set execution.substrate.workerNetworkPolicy.enabled=false

# WorkerPool: off by default, one shared pool when on, pulls with the release's pull secrets.
lacks "no WorkerPool by default" 'kind: WorkerPool' "${wsub[@]}"
has "the default WorkerPool" 'kind: WorkerPool' "${wsub[@]}" --set execution.substrate.workerPool.enabled=true "${pool[@]}"
has "the WorkerPool is in the Substrate namespace" 'namespace: "ate-system"' "${wsub[@]}" --set execution.substrate.workerPool.enabled=true "${pool[@]}"
has "the WorkerPool sandbox class" 'name: microvm' "${wsub[@]}" --set execution.substrate.workerPool.enabled=true "${pool[@]}"
has "the WorkerPool pulls with the global pull secrets (ADR 0061)" 'name: regcred' "${wsub[@]}" --set execution.substrate.workerPool.enabled=true --set 'global.imagePullSecrets={regcred}' "${pool[@]}"
has "the WorkerPool spec override merges" 'foo: bar' "${wsub[@]}" --set execution.substrate.workerPool.enabled=true --set execution.substrate.workerPool.spec.foo=bar "${pool[@]}"
