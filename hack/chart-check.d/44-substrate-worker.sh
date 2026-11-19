# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the Substrate worker namespace and control-plane namespace (ADR 0120 section 6,
# issue #1089): the admission policies, the NetworkPolicies, the worker Namespace and the default
# WorkerPool. Sourced in order by
# hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

wsub=(--set execution.driver=substrate --set execution.substrate.controlAddress=api.ate-system.svc:443
	--set execution.substrate.routerAddress=atenet-router.ate-system.svc:80 --set execution.substrate.tokenAudience=ateapi
	--set execution.substrate.allowUnhardened=true --set execution.substrate.defaultTier=dev --set 'execution.substrate.workerNetworkPolicy.apiServerCIDRs={192.0.2.10/32}'
	--set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --set sandboxLimitRange.enabled=false)
pol=(--show-only templates/substrate-policy.yaml)
np=(--show-only templates/substrate-networkpolicy.yaml)
pool=(--show-only templates/substrate-workerpool.yaml)
wns=(--show-only templates/substrate-workernamespace.yaml)
# docof <awk regex>: the YAML documents of stdin that match the regex (documents split on ---).
docof() { awk -v pat="$1" '/^---$/{ if (buf ~ pat) printf "%s\n---\n", buf; buf=""; next } { buf = buf $0 "\n" } END { if (buf ~ pat) printf "%s", buf }'; }


# Nothing of it renders with the default driver.
lacks "no worker policy with the openshell driver" 'worker-(pods|connect|objects)|control-pods'
lacks "no worker NetworkPolicy with the openshell driver" 'substrate-(workers|control)'
lacks "no WorkerPool with the openshell driver" 'kind: WorkerPool'
lacks "no worker Namespace with the openshell driver" 'ate-workers'

# Policies: native ValidatingAdmissionPolicies, bound to the two Substrate namespaces and nowhere else.
has "worker pod policy" 'name: koinon-koinon-worker-pods$' "${wsub[@]}" "${pol[@]}"
has "control-plane pod policy" 'name: koinon-koinon-control-pods$' "${wsub[@]}" "${pol[@]}"
has "worker CONNECT policy" 'name: koinon-koinon-worker-connect$' "${wsub[@]}" "${pol[@]}"
has "policies are native ValidatingAdmissionPolicies" 'kind: ValidatingAdmissionPolicyBinding' "${wsub[@]}" "${pol[@]}"
lacks "no Kyverno" '[Kk]yverno' "${wsub[@]}" "${pol[@]}"
# The CEL strings are YAML-quoted JSON: strip the escapes so the greps read the CEL.
pdocs=$(render "${wsub[@]}" "${pol[@]}" | tr -d '\\')
wbind=$(docof 'ValidatingAdmissionPolicyBinding\nmetadata:\n  name: koinon-koinon-worker-pods\n' <<<"$pdocs")
cbind=$(docof 'ValidatingAdmissionPolicyBinding\nmetadata:\n  name: koinon-koinon-control-pods\n' <<<"$pdocs")
grep -Fq 'kubernetes.io/metadata.name: "ate-workers"' <<<"$wbind" || fail "the worker pod policy must bind to the worker namespace"
if grep -Fq 'ate-system' <<<"$wbind"; then fail "the worker pod policy must not bind to the control-plane namespace"; fi
grep -Fq 'kubernetes.io/metadata.name: "ate-system"' <<<"$cbind" || fail "the control-plane pod policy must bind to the control-plane namespace"
if grep -Fq 'ate-workers' <<<"$cbind"; then fail "the control-plane pod policy must not bind to the worker namespace"; fi
for b in worker-connect worker-objects; do
	grep -Fq 'values: ["ate-system","ate-workers"]' <<<"$(docof "ValidatingAdmissionPolicyBinding\nmetadata:\n  name: koinon-koinon-$b\n" <<<"$pdocs")" || fail "$b must bind to both namespaces"
done
has "the worker namespace is configurable" 'kubernetes.io/metadata.name: "pools-x"' "${wsub[@]}" --set execution.substrate.workerNamespace=pools-x "${pol[@]}"
if render "${wsub[@]}" --set execution.substrate.workerNamespace=ate-system >/dev/null 2>"$out/sub.err"; then fail "the worker namespace must differ from the control-plane namespace"; fi
grep -q 'workerNamespace' "$out/sub.err" || fail "the same-namespace error must name workerNamespace"
has "the controller identity defaults to ate-controller in the control-plane namespace" 'system:serviceaccount:ate-system:ate-controller' "${wsub[@]}" "${pol[@]}"
has "the controller identity follows the namespace" 'system:serviceaccount:substrate-x:ate-controller' "${wsub[@]}" --set execution.substrate.namespace=substrate-x --set execution.substrate.gatewayClientNamespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=substrate-x "${pol[@]}"
has "the controller identity is configurable" 'system:serviceaccount:other:ctl' "${wsub[@]}" --set 'execution.substrate.workerPolicy.controllerIdentities={system:serviceaccount:other:ctl}' "${pol[@]}"

# Worker pods: ReplicaSet controller only, from a ReplicaSet; the rest of the rules of section 6.
wpods=$(docof 'ValidatingAdmissionPolicy\nmetadata:\n  name: koinon-koinon-worker-pods\n' <<<"$pdocs")
cpods=$(docof 'ValidatingAdmissionPolicy\nmetadata:\n  name: koinon-koinon-control-pods\n' <<<"$pdocs")
grep -Eq 'name: creator' <<<"$wpods" || fail "the worker pod policy must have a creator rule"
creatorline=$(grep -A1 -E 'name: creator' <<<"$wpods" | tail -1)
grep -Fq 'replicaset-controller' <<<"$creatorline" || fail "the ReplicaSet controller must create worker pods"
if grep -Eq 'daemon-set-controller|statefulset-controller|job-controller|ate-controller' <<<"$creatorline"; then fail "only the ReplicaSet controller may create worker pods"; fi
grep -Fq "o.kind == 'ReplicaSet'" <<<"$wpods" || fail "a worker pod must belong to a ReplicaSet"
grep -Fq "hostNetwork is not allowed" <<<"$wpods" || fail "hostNetwork must be denied for worker pods"
grep -Fq "hostPID and hostIPC are not allowed" <<<"$wpods" || fail "hostPID and hostIPC must be denied for worker pods"
grep -Fq "io.katacontainers" <<<"$wpods" || fail "kata annotations must be denied for worker pods"
grep -Fq "request.subResource != 'ephemeralcontainers'" <<<"$wpods" || fail "ephemeral containers must be denied for worker pods"
grep -Fq 'may change the labels of a pod' <<<"$wpods" || fail "untrusted identities must not relabel a worker pod"
grep -Eq 'v.hostPath.path in \["/dev/kvm","/dev/net/tun","/dev/vhost-vsock","/dev/vhost-net","/var/lib/ate"\]' <<<"$wpods" || fail "worker hostPath must be the documented set"
grep -Fq 'has(v.emptyDir) || has(v.configMap) || has(v.downwardAPI) || has(v.projected)' <<<"$wpods" || fail "worker volumes must be limited to the safe kinds (no PVC, CSI, ephemeral, NFS)"
if grep -Eq 'persistentVolumeClaim|has\(v.csi\)|has\(v.nfs\)|has\(v.secret\)' <<<"$wpods"; then fail "the worker volume allowlist must not list network volumes or secrets"; fi
grep -Fq '!has(src.serviceAccountToken)' <<<"$wpods" || fail "a projected serviceAccountToken must be denied for worker pods"
grep -Fq '!has(object.spec.automountServiceAccountToken) || !object.spec.automountServiceAccountToken' <<<"$wpods" || fail "automountServiceAccountToken must be not true for worker pods (unset passes)"
grep -Eq 'x in \["SYS_ADMIN","NET_ADMIN"\]' <<<"$wpods" || fail "added capabilities must be limited to the ceiling"
grep -Fq '@sha256:[a-f0-9]{64}' <<<"$wpods" || fail "worker images must be pinned by digest"
# Control-plane pods: hostPath only for the node agent, created by the DaemonSet controller.
grep -Fq "o.kind == 'DaemonSet'" <<<"$cpods" || fail "the node agent pod must belong to a DaemonSet"
nodeline=$(grep -A1 -E 'name: nodeAgent' <<<"$cpods" | tail -1)
grep -Fq 'daemon-set-controller' <<<"$nodeline" || fail "the DaemonSet controller must create the node agent pod"
grep -Fq 'object.spec.serviceAccountName in ["atelet"]' <<<"$nodeline" || fail "the node agent pod must run as the atelet ServiceAccount"
grep -Fq 'variables.nodeAgent && v.hostPath.path in' <<<"$cpods" || fail "hostPath in the control-plane namespace must need the node agent rule"
if grep -Fq 'ate-workers' <<<"$cpods"; then fail "the control-plane pod policy must not mention the worker namespace"; fi
grep -Fq "hostNetwork is not allowed" <<<"$cpods" || fail "hostNetwork must be denied in the control-plane namespace"
has "the node agent ServiceAccount is configurable" 'object.spec.serviceAccountName in \[\\"agent-x\\"\]' "${wsub[@]}" --set execution.substrate.workerPolicy.nodeAgent.serviceAccount=agent-x "${pol[@]}"
has "the node agent hostPath list is configurable" 'v.hostPath.path in \[\\"/dev/kvm\\"\]' "${wsub[@]}" --set 'execution.substrate.workerPolicy.nodeAgent.allowedHostPaths={/dev/kvm}' "${pol[@]}"
has "the capability ceiling is configurable" 'x in \[\\"SYS_ADMIN\\"\]' "${wsub[@]}" --set 'execution.substrate.workerPolicy.allowedCapabilities={SYS_ADMIN}' "${pol[@]}"
has "worker hostPath list is configurable" 'v.hostPath.path in \[\\"/dev/kvm\\"\]' "${wsub[@]}" --set 'execution.substrate.workerPolicy.allowedHostPaths={/dev/kvm}' "${pol[@]}"
# Digest exemption: the default is the platform controllers; the list is used as written, so [] means none (#1140).
tagline=$(grep -A1 -E 'name: tagAllowed' <<<"$wpods" | tail -1)
grep -Fq 'replicaset-controller' <<<"$tagline" || fail "the digest exemption must default to the platform controllers"
if grep -Fq 'ate-controller' <<<"$tagline"; then fail "the Substrate controller must not be exempt from the digest rule by default"; fi
tagline=$(render "${wsub[@]}" --set 'execution.substrate.workerPolicy.digestExemptIdentities={system:serviceaccount:x:y}' "${pol[@]}" | grep -A1 -E 'name: tagAllowed' | head -2 | tail -1)
grep -Fq 'system:serviceaccount:x:y' <<<"$tagline" || fail "digestExemptIdentities must set the exemption"
if grep -Fq 'replicaset-controller' <<<"$tagline"; then fail "digestExemptIdentities must replace the default list"; fi
printf 'execution:\n  substrate:\n    workerPolicy:\n      digestExemptIdentities: []\n' >"$out/no-exempt.yaml"
tagline=$(render "${wsub[@]}" -f "$out/no-exempt.yaml" "${pol[@]}" | grep -A1 -E 'name: tagAllowed' | head -2 | tail -1)
grep -Fq 'request.userInfo.username in []' <<<"$tagline" || fail "an empty digestExemptIdentities must exempt nobody"
if grep -Fq 'replicaset-controller' <<<"$tagline"; then fail "an empty digestExemptIdentities must not fall back to the platform controllers"; fi

# CONNECT and objects.
has "exec, attach, port-forward, pod proxy and service proxy are denied" 'resources: \[pods/exec, pods/attach, pods/portforward, pods/proxy, services/proxy\]' "${wsub[@]}" "${pol[@]}"
has "the validation actions deny" 'validationActions: \["Deny"\]' "${wsub[@]}" "${pol[@]}"
has "the object guard covers the three Substrate kinds" 'resources: \[workerpools, sandboxconfigs, csidriverconfigs\]' "${wsub[@]}" "${pol[@]}"
has "the object guard covers the workload kinds" 'resources: \[deployments, replicasets, daemonsets, statefulsets\]' "${wsub[@]}" "${pol[@]}"
has "the object guard covers jobs and cronjobs" 'resources: \[jobs, cronjobs\]' "${wsub[@]}" "${pol[@]}"
has "replication controllers are guarded" 'replicationcontrollers' "${wsub[@]}" "${pol[@]}"
has "the installer group defaults to system:masters" 'g in \[\\"system:masters\\",\\"kubeadm:cluster-admins\\"\]' "${wsub[@]}" "${pol[@]}"
has "the kubeadm admin group is an installer by default" 'kubeadm:cluster-admins' "${wsub[@]}" "${pol[@]}"
has "installer identities are configurable" 'system:admin' "${wsub[@]}" --set 'execution.substrate.workerPolicy.installerIdentities={system:admin}' "${pol[@]}"
objs=$(docof 'ValidatingAdmissionPolicy\nmetadata:\n  name: koinon-koinon-worker-objects\n' <<<"$pdocs")
grep -Fq "request.namespace == \"ate-workers\" && request.resource.group == 'apps' && request.resource.resource == 'deployments'" <<<"$objs" || fail "the controller may apply Deployments in the worker namespace only"
grep -Fq '!object.spec.template.spec.automountServiceAccountToken' <<<"$objs" || fail "the Deployment template must not automount a token"
grep -Fq 'object.spec.template.spec.volumes.all(v, !has(v.hostPath) || v.hostPath.path in' <<<"$objs" || fail "the Deployment template hostPath must follow allowedHostPaths"
grep -Fq "request.namespace == \"ate-workers\" && request.operation == 'UPDATE' && request.resource.group == 'ate.dev'" <<<"$objs" || fail "the controller may change metadata of Substrate objects in the worker namespace only"
lacks "the policies are off when disabled" 'worker-(pods|connect|objects)|control-pods' "${wsub[@]}" --set execution.substrate.workerPolicy.enabled=false
if render "${wsub[@]}" --set 'execution.substrate.workerPolicy.validationActions={Nope}' >/dev/null 2>&1; then fail "an unknown validation action must not render"; fi

# The worker Namespace: created by the chart, with the Pod Security level these pods need.
has "the chart creates the worker namespace" 'kind: Namespace' "${wsub[@]}" "${wns[@]}"
has "the worker namespace name" 'name: "ate-workers"' "${wsub[@]}" "${wns[@]}"
has "the worker namespace carries the name label" 'kubernetes.io/metadata.name: "ate-workers"' "${wsub[@]}" "${wns[@]}"
has "the worker namespace is privileged for Pod Security" 'pod-security.kubernetes.io/enforce: privileged' "${wsub[@]}" "${wns[@]}"

# NetworkPolicies: standard API only.
npdocs=$(render "${wsub[@]}" "${np[@]}")
[[ $(grep -c '^kind: NetworkPolicy' <<<"$npdocs") == 2 ]] || fail "expected two NetworkPolicies (workers and control plane)"
workers=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-workers\n' <<<"$npdocs")
control=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-control\n' <<<"$npdocs")
grep -Fq 'namespace: "ate-workers"' <<<"$workers" || fail "the workers policy must be in the worker namespace"
grep -Fq 'namespace: "ate-system"' <<<"$control" || fail "the control policy must be in the control-plane namespace"
grep -Fq 'podSelector: {}' <<<"$workers" || fail "the workers policy must cover every pod of the worker namespace"
grep -Fq 'policyTypes: [Ingress, Egress]' <<<"$workers" || fail "the workers policy must cover ingress and egress"
grep -Fq 'app: atenet-egress' <<<"$workers" || fail "worker egress must name atenet-egress"
grep -Fq 'app.kubernetes.io/component: gateway' <<<"$workers" || fail "worker egress must name the gateway pods"
if grep -Fq 'app.kubernetes.io/component: api' <<<"$workers"; then fail "worker egress must not name the Koinon API pods"; fi
grep -Fq 'port: 53' <<<"$workers" || fail "worker egress must allow DNS"
if grep -Eq '192\.0\.2\.10|6443|ipBlock|extraEgress' <<<"$workers"; then fail "worker pods must have no API server route"; fi
if grep -Eq '0\.0\.0\.0/0|::/0|postgres|cnpg' <<<"$npdocs"; then fail "no public or database egress"; fi
grep -Fq 'key: app' <<<"$control" || fail "the control policy must select on the app label"
grep -Fq 'operator: In' <<<"$control" || fail "the control policy must select an explicit list of app labels"
if grep -Eq 'DoesNotExist|ate.dev/worker-pool' <<<"$control"; then fail "the control policy must not key on the absence of the worker label (fails open)"; fi
for a in atelet ate-controller ate-api-server atenet-router atenet-egress; do
	grep -Eq -- "- $a\$" <<<"$control" || fail "the control policy must list app $a"
done
grep -Fq 'cidr: "192.0.2.10/32"' <<<"$control" || fail "the control-plane policy must carry the API server route"
grep -Fq 'kubernetes.io/metadata.name: "ate-workers"' <<<"$control" || fail "the control-plane policy must reach the worker namespace"
has "the app list is configurable" '- app-x$' "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.controlPlaneApps={app-x}' "${np[@]}"
if render "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.controlPlaneApps=null' >/dev/null 2>"$out/sub.err"; then fail "an empty controlPlaneApps must not render"; fi
grep -q 'controlPlaneApps' "$out/sub.err" || fail "the empty-list error must name controlPlaneApps"
has "worker NetworkPolicy admits Koinon's namespace" 'kubernetes.io/metadata.name: "koinon"' "${wsub[@]}" "${np[@]}"
if render "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.apiServerCIDRs=null' >/dev/null 2>"$out/sub.err"; then fail "an empty apiServerCIDRs must not render"; fi
grep -q 'apiServerCIDRs' "$out/sub.err" || fail "the empty-CIDR error must name apiServerCIDRs"
if render "${wsub[@]}" --set execution.substrate.namespace=substrate-x >/dev/null 2>"$out/sub.err"; then fail "a namespace that differs from gatewayClientNamespaceSelector must not render"; fi
grep -q 'gatewayClientNamespaceSelector' "$out/sub.err" || fail "the namespace-mismatch error must name gatewayClientNamespaceSelector"
has "a matching namespace and selector render" 'namespace: "substrate-x"' "${wsub[@]}" --set execution.substrate.namespace=substrate-x --set execution.substrate.gatewayClientNamespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=substrate-x "${np[@]}"
has "the API server rule follows apiServerCIDRs" 'cidr: "10.0.0.1/32"' "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.apiServerCIDRs={10.0.0.1/32}' "${np[@]}"
lacks "no CNI-specific resource" 'cilium|calico|CiliumNetworkPolicy' "${wsub[@]}" "${np[@]}"
lacks "the NetworkPolicies are off when disabled" 'substrate-(workers|control)' "${wsub[@]}" --set execution.substrate.workerNetworkPolicy.enabled=false

# WorkerPool (Substrate v1.0.0 WorkerPoolSpec): off by default; workerImage is required; one sandbox
# class; placement and the pull ServiceAccount live under spec.template; the spec has no imagePullSecrets.
# The pool and its ServiceAccount are in the worker namespace, and the template always names the chart
# ServiceAccount (automountServiceAccountToken false), because the controller's own ateworker ServiceAccount
# leaves the field unset.
wp=(--set execution.substrate.workerPool.enabled=true --set execution.substrate.workerPool.workerImage=registry.example/ateworker-gvisor:v1.0.0)
lacks "no WorkerPool by default" 'kind: WorkerPool' "${wsub[@]}"
if render "${wsub[@]}" --set execution.substrate.workerPool.enabled=true >/dev/null 2>"$out/sub.err"; then fail "a WorkerPool without workerImage must not render"; fi
grep -q 'workerImage' "$out/sub.err" || fail "the missing-image error must name workerImage"
has "the default WorkerPool" 'kind: WorkerPool' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
pooldocs=$(render "${wsub[@]}" "${wp[@]}" "${pool[@]}")
grep -Eq 'namespace: "ate-workers"' <<<"$pooldocs" || fail "the WorkerPool must be in the worker namespace"
if grep -Fq 'ate-system' <<<"$pooldocs"; then fail "nothing of the pool belongs in the control-plane namespace"; fi
[[ $(grep -c 'namespace: "ate-workers"' <<<"$pooldocs") == 2 ]] || fail "the WorkerPool and its ServiceAccount must be in the worker namespace"
has "the WorkerPool carries workerImage" 'workerImage: registry.example/ateworker-gvisor:v1.0.0' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the default pool is named after its class" 'name: "koinon-gvisor"' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the pool name follows the class" 'name: "koinon-microvm"' "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.sandboxClass=microvm "${pool[@]}"
lacks "no ServiceAccount when spec sets one" 'kind: ServiceAccount' "${wsub[@]}" "${wp[@]}" --set 'global.imagePullSecrets={regcred}' --set execution.substrate.workerPool.spec.template.serviceAccountName=mine "${pool[@]}"
has "the WorkerPool default class is gvisor" 'name: gvisor' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the WorkerPool class is configurable" 'name: microvm' "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.sandboxClass=microvm "${pool[@]}"
if render "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.sandboxClass=kata >/dev/null 2>&1; then fail "an unknown sandbox class must not render"; fi
lacks "the WorkerPool has exactly one class" 'name: microvm' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
lacks "the WorkerPool spec has no imagePullSecrets or top-level nodeSelector" '^  (imagePullSecrets|nodeSelector|tolerations):' "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.nodeSelector.pool=vm --set 'global.imagePullSecrets={regcred}' "${pool[@]}"
has "nodeSelector sits under spec.template" '^    nodeSelector:' "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.nodeSelector.pool=vm "${pool[@]}"
has "the pull secrets go to a ServiceAccount" 'kind: ServiceAccount' "${wsub[@]}" "${wp[@]}" --set 'global.imagePullSecrets={regcred}' "${pool[@]}"
has "the ServiceAccount carries the pull secret (ADR 0061)" 'name: regcred' "${wsub[@]}" "${wp[@]}" --set 'global.imagePullSecrets={regcred}' "${pool[@]}"
has "the pool always runs as the chart ServiceAccount" 'serviceAccountName: "?koinon-gvisor-worker"?' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the pool runs as that ServiceAccount with pull secrets" 'serviceAccountName: "?koinon-gvisor-worker"?' "${wsub[@]}" "${wp[@]}" --set 'global.imagePullSecrets={regcred}' "${pool[@]}"
has "the chart ServiceAccount exists without pull secrets" 'kind: ServiceAccount' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
lacks "no pull secret on it when none is set" 'imagePullSecrets' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the worker ServiceAccount mounts no API token" 'automountServiceAccountToken: false' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
has "the worker ServiceAccount mounts no API token with pull secrets" 'automountServiceAccountToken: false' "${wsub[@]}" "${wp[@]}" --set 'global.imagePullSecrets={regcred}' "${pool[@]}"
has "a named ServiceAccount is used as is" 'serviceAccountName: "?my-sa"?' "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.serviceAccountName=my-sa "${pool[@]}"
has "the WorkerPool spec override merges" 'foo: bar' "${wsub[@]}" "${wp[@]}" --set execution.substrate.workerPool.spec.foo=bar "${pool[@]}"
lacks "the render has no lookup or random value" 'randAlphaNum|lookup' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
