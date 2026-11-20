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
lacks "no worker policy with the openshell driver" 'worker-(pods|connect|objects|netpol)|control-pods'
lacks "no worker NetworkPolicy with the openshell driver" 'name: koinon-substrate-(workers|control)'
lacks "no WorkerPool with the openshell driver" 'kind: WorkerPool'
lacks "no worker Namespace with the openshell driver" 'ate-workers'

# Policies: native ValidatingAdmissionPolicies, bound to the two Substrate namespaces and nowhere else.
has "worker pod policy" 'name: koinon-koinon-worker-pods$' "${wsub[@]}" "${pol[@]}"
has "control-plane pod policy" 'name: koinon-koinon-control-pods$' "${wsub[@]}" "${pol[@]}"
has "worker CONNECT policy" 'name: koinon-koinon-worker-connect$' "${wsub[@]}" "${pol[@]}"
has "policies are native ValidatingAdmissionPolicies" 'kind: ValidatingAdmissionPolicyBinding' "${wsub[@]}" "${pol[@]}"
lacks "no Kyverno" '[Kk]yverno' "${wsub[@]}" "${pol[@]}"
# The CEL strings are YAML-quoted JSON: strip the escapes so the greps read the CEL.
pdocs=$(render "${wsub[@]}" "${pol[@]}" | tr -d '\134')
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
if grep -Eq 'x in \["SYS_ADMIN","NET_ADMIN"\]' <<<"$wpods"; then fail "the namespace must have no capability ceiling: only the pinned worker shape adds capabilities"; fi
grep -Fq "(c.name == 'worker' && c.image in [])" <<<"$wpods" || fail "with no worker image no container is a worker container (fail closed)"
grep -Fq 'size(c.securityContext.capabilities.add) == 0' <<<"$wpods" || fail "a container that is not the worker container must add no capability"
grep -Fq "c.securityContext.capabilities.drop == ['ALL']" <<<"$wpods" || fail "the worker container must drop ALL"
grep -Fq 'c.securityContext.runAsUser == 0' <<<"$wpods" || fail "the worker container must run as user 0 (the shape of Substrate)"
grep -Fq 'c.securityContext.runAsGroup == 0' <<<"$wpods" || fail "the worker container must run as group 0 (the shape of Substrate)"
grep -Fq '!c.securityContext.allowPrivilegeEscalation' <<<"$wpods" || fail "the worker container must not escalate privilege"
wpodsu=$(tr -d '\134' <<<"$wpods")
grep -Fq '["NET_ADMIN","SYS_ADMIN","SETUID","SETGID","DAC_OVERRIDE","FOWNER","CHOWN","NET_RAW","SETFCAP"]' <<<"$wpodsu" || fail "the gVisor class capabilities must be the set of Substrate"
grep -Fq '["NET_ADMIN","SYS_ADMIN","SETUID","SETGID","DAC_OVERRIDE","FOWNER","CHOWN","MKNOD","SETFCAP","FSETID","SETPCAP"]' <<<"$wpodsu" || fail "the micro-VM class capabilities must be the set of Substrate"
if grep -Eq '"(ALL|SYS_PTRACE|SYS_MODULE|SYS_CHROOT)"' <<<"$wpodsu"; then fail "no capability beyond the two Substrate sets"; fi
grep -Fq '@sha256:[a-f0-9]{64}' <<<"$wpods" || fail "worker images must be pinned by digest"
grep -Fq 'has(src.clusterTrustBundle)' <<<"$wpods" || fail "projected sources in worker pods must be an allowlist without Secret"
grep -Fq 'secretKeyRef' <<<"$wpods" || fail "worker pods must not take a Secret in env"
grep -Fq 'hostPort' <<<"$wpods" || fail "worker pods must not use hostPort"
grep -Fq "!= 'Unconfined'" <<<"$wpods" || fail "worker pods must not run Unconfined seccomp or AppArmor"
creatorline=$(grep -A1 -E 'name: creator' <<<"$cpods" | tail -1)
if grep -Fq 'ate-controller' <<<"$creatorline"; then fail "the Substrate controller must not create pods in the control-plane namespace"; fi
if render "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.atenetEgressSelector={}' --show-only templates/substrate-networkpolicy.yaml >/dev/null 2>"$out/sel.err"; then fail "an empty atenetEgressSelector must fail the render"; fi
# Control-plane pods: hostPath only for the node agent, created by the DaemonSet controller.
grep -Fq "o.kind == 'DaemonSet'" <<<"$cpods" || fail "the node agent pod must belong to a DaemonSet"
nodeline=$(grep -A1 -E 'name: nodeAgent' <<<"$cpods" | tail -1)
grep -Fq 'daemon-set-controller' <<<"$nodeline" || fail "the DaemonSet controller must create the node agent pod"
grep -Fq 'object.spec.serviceAccountName in ["atelet"]' <<<"$nodeline" || fail "the node agent pod must run as the atelet ServiceAccount"
grep -Fq 'v.hostPath.path in ["/var/lib/ate","/var/lib/kubelet/plugins","/var/lib/kubelet/device-plugins","/dev","/home/kubernetes/bin","/etc/srv/kubernetes/cri_auth_config.yaml"]' <<<"$cpods" || fail "the node agent hostPath list must be atelet's own list"
grep -Fq 'variables.nodeAgent && v.hostPath.path in' <<<"$cpods" || fail "hostPath in the control-plane namespace must need the node agent rule"
if grep -Fq 'ate-workers' <<<"$cpods"; then fail "the control-plane pod policy must not mention the worker namespace"; fi
grep -Fq "hostNetwork is not allowed" <<<"$cpods" || fail "hostNetwork must be denied in the control-plane namespace"
has "the node agent ServiceAccount is configurable" 'object.spec.serviceAccountName in \[\\"agent-x\\"\]' "${wsub[@]}" --set execution.substrate.workerPolicy.nodeAgent.serviceAccount=agent-x "${pol[@]}"
has "the node agent hostPath list is configurable" 'v.hostPath.path in \[\\"/dev/kvm\\"\]' "${wsub[@]}" --set 'execution.substrate.workerPolicy.nodeAgent.allowedHostPaths={/dev/kvm}' "${pol[@]}"
wimg=(--set 'execution.substrate.workerPolicy.workerImages={registry.example/w@sha256:aaaa}')
has "the worker images are configurable" '\\"registry.example/w@sha256:aaaa\\"' "${wsub[@]}" "${wimg[@]}" "${pol[@]}"
wpool=(--set execution.substrate.workerPool.enabled=true --set execution.substrate.workerPool.workerImage=registry.example/ateworker-gvisor:v1.0.0)
has "the WorkerPool image is a worker image" '\\"registry.example/ateworker-gvisor:v1.0.0\\"' "${wsub[@]}" "${wpool[@]}" "${pol[@]}"
wmir=(--set 'global.registryMirrors.registry\.example=mirror.example/cache')
has "the mirrored worker image is the policy worker image" '\\"mirror.example/cache/ateworker-gvisor:v1.0.0\\"' "${wsub[@]}" "${wmir[@]}" "${wpool[@]}" "${pol[@]}"
has "the mirrored worker image is the pool worker image" 'workerImage: mirror.example/cache/ateworker-gvisor:v1.0.0' "${wsub[@]}" "${wmir[@]}" "${wpool[@]}" "${pool[@]}"
has "worker hostPath list is configurable" 'v.hostPath.path in \[\\"/dev/kvm\\"\]' "${wsub[@]}" --set 'execution.substrate.workerPolicy.allowedHostPaths={/dev/kvm}' "${pol[@]}"
# Digest exemption: the default is the platform controllers; the list is used as written, so [] means none (#1140).
tagline=$(grep -A1 -E 'name: tagAllowed' <<<"$wpods" | tail -1)
grep -Fq 'replicaset-controller' <<<"$tagline" || fail "the digest exemption must default to the platform controllers"
if grep -Fq 'ate-controller' <<<"$tagline"; then fail "the Substrate controller must not be exempt from the digest rule by default"; fi
custom=$(render "${wsub[@]}" --set 'execution.substrate.workerPolicy.digestExemptIdentities={system:serviceaccount:x:y}' "${pol[@]}")
tagline=$(grep -A1 -E 'name: tagAllowed' <<<"$custom" | tail -1)
grep -Fq 'system:serviceaccount:x:y' <<<"$tagline" || fail "digestExemptIdentities must set the exemption"
if grep -Fq 'replicaset-controller' <<<"$tagline"; then fail "digestExemptIdentities must replace the default list"; fi
mkdir -p "$out/values"
printf 'execution:\n  substrate:\n    workerPolicy:\n      digestExemptIdentities: []\n' >"$out/values/no-exempt.yaml"
none=$(render "${wsub[@]}" -f "$out/values/no-exempt.yaml" "${pol[@]}")
tagline=$(grep -A1 -E 'name: tagAllowed' <<<"$none" | tail -1)
grep -Fq 'request.userInfo.username in []' <<<"$tagline" || fail "an empty digestExemptIdentities must exempt nobody"
if grep -Fq 'replicaset-controller' <<<"$tagline"; then fail "an empty digestExemptIdentities must not fall back to the platform controllers"; fi

# CONNECT and objects.
has "exec, attach, port-forward, pod proxy and service proxy are denied" 'resources: \[pods/exec, pods/attach, pods/portforward, pods/proxy, services/proxy\]' "${wsub[@]}" "${pol[@]}"
connpol=$(docof 'ValidatingAdmissionPolicy\nmetadata:\n  name: koinon-koinon-worker-connect\n' <<<"$pdocs")
connbind=$(docof 'ValidatingAdmissionPolicyBinding\nmetadata:\n  name: koinon-koinon-worker-connect\n' <<<"$pdocs")
grep -Fq "request.subResource in ['exec', 'attach', 'portforward']" <<<"$connpol" || fail "only exec, attach and port-forward may pass the CONNECT policy"
grep -Fq 'variables.inControlPlane && variables.installer' <<<"$connpol" || fail "the CONNECT policy admits the installer in the control-plane namespace only"
grep -Fq 'request.namespace == "ate-system"' <<<"$(tr -d '\134' <<<"$connpol")" || fail "inControlPlane must be the control-plane namespace"
grep -Fq 'system:masters' <<<"$connpol" || fail "the CONNECT installer groups follow workerPolicy.installerGroups"
grep -Fq 'auditAnnotations:' <<<"$connpol" || fail "a CONNECT request leaves an audit annotation"
grep -Fq 'validationActions: ["Deny","Audit"]' <<<"$connbind" || fail "the CONNECT binding denies and audits"
has "the CONNECT installer identities are configurable" '\\"ops-x\\"' "${wsub[@]}" --set 'execution.substrate.workerPolicy.installerIdentities={ops-x}' "${pol[@]}"
has "the validation actions deny" 'validationActions: \["Deny"\]' "${wsub[@]}" "${pol[@]}"
has "the object guard covers the three Substrate kinds" 'resources: \[workerpools, sandboxconfigs, csidriverconfigs\]' "${wsub[@]}" "${pol[@]}"
has "the object guard covers the workload kinds" 'resources: \[deployments, replicasets, daemonsets, statefulsets\]' "${wsub[@]}" "${pol[@]}"
has "the object guard covers jobs and cronjobs" 'resources: \[jobs, cronjobs\]' "${wsub[@]}" "${pol[@]}"
has "replication controllers are guarded" 'replicationcontrollers' "${wsub[@]}" "${pol[@]}"
has "the installer group defaults to system:masters" 'g in \[\\"system:masters\\",\\"kubeadm:cluster-admins\\"\]' "${wsub[@]}" "${pol[@]}"
has "the kubeadm admin group is an installer by default" 'kubeadm:cluster-admins' "${wsub[@]}" "${pol[@]}"
has "installer identities are configurable" 'system:admin' "${wsub[@]}" --set 'execution.substrate.workerPolicy.installerIdentities={system:admin}' "${pol[@]}"
objs=$(docof 'ValidatingAdmissionPolicy\nmetadata:\n  name: koinon-koinon-worker-objects\n' <<<"$pdocs")
grep -Fq "variables.reqNamespace == \"ate-workers\" && request.resource.group == 'apps' && request.resource.resource == 'deployments'" <<<"$objs" || fail "the controller may apply Deployments in the worker namespace only"
grep -Fq '!object.spec.template.spec.automountServiceAccountToken' <<<"$objs" || fail "the Deployment template must not automount a token"
grep -Fq 'object.spec.template.spec.volumes.all(v, !has(v.hostPath) || v.hostPath.path in' <<<"$objs" || fail "the Deployment template hostPath must follow allowedHostPaths"
grep -Fq "variables.reqNamespace in [\"ate-system\",\"ate-workers\",\"\"] && request.operation == 'UPDATE' && request.resource.group == 'ate.dev'" <<<"$objs" || fail "the controller may change metadata of Substrate objects in both namespaces and of cluster-scoped ones"
grep -Fq "has(request.namespace) ? request.namespace : ''" <<<"$objs" || fail "a cluster-scoped object has no request.namespace key: the objects policy must guard the read"
rest=$(tr -d '\134' <<<"$objs" | grep -Fv '#' | sed "s/has(request.namespace) ? request.namespace : ''//")
if grep -Fq 'request.namespace' <<<"$rest"; then fail "the objects policy reads request.namespace outside the guard"; fi
grep -Fq 'c.securityContext.privileged' <<<"$wpods" || fail "the worker pod rules must deny privileged containers"
grep -Fq 'object.spec.template.spec.containers.all(c, !has(c.securityContext) || !has(c.securityContext.privileged)' <<<"$objs" || fail "the Deployment template must not be privileged"
lacks "the policies are off when disabled" 'worker-(pods|connect|objects|netpol)|control-pods' "${wsub[@]}" --set execution.substrate.workerPolicy.enabled=false
# NetworkPolicy writes in the worker namespace (issue #1158): NetworkPolicies add up, so the installer and the controller write
# them, the controller's policies reach the control-plane namespace and DNS only, and the chart's own policy is the installer's.
nppol=$(docof 'ValidatingAdmissionPolicy\nmetadata:\n  name: koinon-koinon-worker-netpol\n' <<<"$pdocs")
npbind=$(docof 'ValidatingAdmissionPolicyBinding\nmetadata:\n  name: koinon-koinon-worker-netpol\n' <<<"$pdocs")
grep -Fq 'resources: [networkpolicies]' <<<"$nppol" || fail "the NetworkPolicy guard must cover networkpolicies"
grep -Fq 'operations: [CREATE, UPDATE, DELETE]' <<<"$nppol" || fail "the NetworkPolicy guard must cover create, update and delete"
grep -Fq 'kubernetes.io/metadata.name: "ate-workers"' <<<"$npbind" || fail "the NetworkPolicy guard must bind to the worker namespace"
if grep -Fq 'ate-system' <<<"$npbind"; then fail "the NetworkPolicy guard must not bind to the control-plane namespace"; fi
grep -Fq 'validationActions: ["Deny"]' <<<"$npbind" || fail "the NetworkPolicy guard must deny"
grep -Fq 'request.userInfo.username in ["system:serviceaccount:ate-system:ate-controller"]' <<<"$nppol" || fail "the NetworkPolicy guard must name the Substrate controller"
grep -Fq 'request.name == "koinon-substrate-workers"' <<<"$nppol" || fail "the controller must not write the chart's worker NetworkPolicy"
grep -Fq 'has(r.to) && size(r.to) > 0' <<<"$nppol" || fail "an egress rule of the controller needs a to list (no to means every destination)"
grep -Fq '!has(p.ipBlock)' <<<"$nppol" || fail "the controller's NetworkPolicy must have no ipBlock peer"
grep -Fq "== \"ate-system\"" <<<"$nppol" || fail "the controller's egress must reach the control-plane namespace"
grep -Fq "q.port == 53" <<<"$nppol" || fail "the DNS peer must be limited to port 53"
if grep -Fq 'request.namespace' <<<"$(grep -Fv '#' <<<"$nppol")"; then fail "the NetworkPolicy guard must not read request.namespace (the binding selects the namespace)"; fi
has "the NetworkPolicy guard follows the worker namespace" 'kubernetes.io/metadata.name: "pools-x"' "${wsub[@]}" --set execution.substrate.workerNamespace=pools-x "${pol[@]}"
has "the NetworkPolicy guard follows the control-plane namespace" 'metadata.name.\] == "substrate-x"' "${wsub[@]}" --set execution.substrate.namespace=substrate-x --set execution.substrate.gatewayClientNamespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=substrate-x "${pol[@]}"
has "the NetworkPolicy guard follows the installer identities" '\\"ops-x\\"' "${wsub[@]}" --set 'execution.substrate.workerPolicy.installerIdentities={ops-x}' "${pol[@]}"
if render "${wsub[@]}" --set 'execution.substrate.workerPolicy.validationActions={Nope}' >/dev/null 2>&1; then fail "an unknown validation action must not render"; fi

# The worker Namespace: created by the chart, with the Pod Security level these pods need.
has "the chart creates the worker namespace" 'kind: Namespace' "${wsub[@]}" "${wns[@]}"
has "the worker namespace name" 'name: "ate-workers"' "${wsub[@]}" "${wns[@]}"
has "the worker namespace carries the name label" 'kubernetes.io/metadata.name: "ate-workers"' "${wsub[@]}" "${wns[@]}"
has "the worker namespace is privileged for Pod Security" 'pod-security.kubernetes.io/enforce: privileged' "${wsub[@]}" "${wns[@]}"

# NetworkPolicies: standard API only.
npdocs=$(render "${wsub[@]}" "${np[@]}")
[[ $(grep -c '^kind: NetworkPolicy' <<<"$npdocs") == 5 ]] || fail "expected five NetworkPolicies (workers, control plane, default-deny egress, API-only pods, bucket-init Job)"
closed=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-default-deny-egress\n' <<<"$npdocs")
apionly=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-api-only\n' <<<"$npdocs")
grep -Eq '^  podSelector: \{\}$' <<<"$closed" || fail "the default-deny egress policy must cover every pod of the control-plane namespace"
grep -Fq 'namespace: "ate-system"' <<<"$closed" || fail "the default-deny egress policy must be in the control-plane namespace"
grep -Fq 'egress: []' <<<"$closed" || fail "the default-deny egress policy must deny all egress"
grep -Fq 'policyTypes: [Egress]' <<<"$closed" || fail "the default-deny egress policy must leave ingress to Substrate's own policies"
grep -Fq -- '- k8s-credential-provider' <<<"$apionly" || fail "the API-only policy must select k8s-credential-provider"
grep -Fq 'cidr: "192.0.2.10/32"' <<<"$apionly" || fail "the API-only policy must reach the API server CIDRs"
grep -Fq 'policyTypes: [Egress]' <<<"$apionly" || fail "the API-only policy must leave ingress to Substrate's own policies"
bucket=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-bucket-init\n' <<<"$npdocs")
grep -Fq 'key: batch.kubernetes.io/job-name' <<<"$bucket" || fail "the bucket-init policy must select the Job pod by Job name"
grep -Fq -- '- rustfs-bucket-init' <<<"$bucket" || fail "the bucket-init policy must select rustfs-bucket-init"
grep -Fq 'app: "rustfs"' <<<"$bucket" || fail "the bucket-init policy must reach the object store only"
grep -Fq '{port: 9000, protocol: TCP}' <<<"$bucket" || fail "the bucket-init policy must reach the S3 port"
grep -Fq 'policyTypes: [Egress]' <<<"$bucket" || fail "the bucket-init policy must leave ingress to Substrate's own policies"
lacks "no bucket-init policy with an empty list" 'substrate-bucket-init' "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.bucketInitJobs=null' "${np[@]}"
lacks "no image-source policy until an image source is set" 'substrate-image-source' "${wsub[@]}" "${np[@]}"
imgpod=(--set 'execution.substrate.workerNetworkPolicy.imageSource.podSelector.matchLabels.app=zot' --set 'execution.substrate.workerNetworkPolicy.imageSource.namespaceSelector.matchLabels.kubernetes\.io/metadata\.name=registry' --set execution.substrate.workerNetworkPolicy.imageSource.port=5000)
imgdoc=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-image-source\n' <<<"$(render "${wsub[@]}" "${imgpod[@]}" "${np[@]}")")
grep -Fq 'app: zot' <<<"$imgdoc" || fail "the image-source policy must select the in-cluster cache pods"
grep -Fq 'kubernetes.io/metadata.name: registry' <<<"$imgdoc" || fail "the image-source policy must name the cache namespace"
grep -Fq '{port: 5000, protocol: TCP}' <<<"$imgdoc" || fail "the image-source policy must use the configured port"
grep -Fq -- '- atelet' <<<"$imgdoc" || fail "the image-source policy must select the node agent"
grep -Fq 'policyTypes: [Egress]' <<<"$imgdoc" || fail "the image-source policy must leave ingress to Substrate's own policies"
if grep -Fq 'ipBlock' <<<"$imgdoc"; then fail "an in-cluster image source must not open a CIDR"; fi
imgcidr=(--set 'execution.substrate.workerNetworkPolicy.imageSource.cidrs={198.51.100.0/24}')
has "an operator CIDR can be the image source" 'cidr: "198.51.100.0/24"' "${wsub[@]}" "${imgcidr[@]}" "${np[@]}"
if render "${wsub[@]}" "${imgpod[@]}" "${imgcidr[@]}" "${np[@]}" >/dev/null 2>"$out/img.err"; then fail "two image sources must not render"; fi
if render "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.imageSource.cidrs={0.0.0.0/0}' "${np[@]}" >/dev/null 2>&1; then fail "the whole internet must not be an image source"; fi
has "a matchExpressions pod selector can be the image source" 'matchExpressions' "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.imageSource.podSelector.matchExpressions[0].key=app' --set 'execution.substrate.workerNetworkPolicy.imageSource.podSelector.matchExpressions[0].operator=Exists' --set execution.substrate.workerNetworkPolicy.imageSource.port=5000 "${np[@]}"
grep -Fq 'one image source' "$out/img.err" || fail "the two-sources error must say that there is one image source"
closedcode=$(grep -v '^[[:space:]]*#' <<<"$closed")
if grep -Eq 'ate-idp-server|postgres|rustfs' <<<"$closedcode"; then fail "the default-deny egress policy names no app: it covers them all"; fi
workers=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-workers\n' <<<"$npdocs")
control=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-control\n' <<<"$npdocs")
grep -Fq 'namespace: "ate-workers"' <<<"$workers" || fail "the workers policy must be in the worker namespace"
grep -Fq 'namespace: "ate-system"' <<<"$control" || fail "the control policy must be in the control-plane namespace"
grep -Eq '^  podSelector: \{\}$' <<<"$workers" || fail "the workers policy must cover every pod of the worker namespace"
grep -Fq 'policyTypes: [Ingress, Egress]' <<<"$workers" || fail "the workers policy must cover ingress and egress"
grep -Fq 'app: atenet-egress' <<<"$workers" || fail "worker egress must name atenet-egress"
if grep -Fq 'app.kubernetes.io/component: gateway' <<<"$workers"; then fail "worker egress must not name the gateway pods (atenet carries the traffic)"; fi
if grep -Eq '^        - podSelector: \{\}$' <<<"$workers"; then fail "worker ingress must not allow other worker pods"; fi
if grep -Fq 'app.kubernetes.io/component: api' <<<"$workers"; then fail "worker egress must not name the Koinon API pods"; fi
grep -Fq 'port: 53' <<<"$workers" || fail "worker egress must allow DNS"
if grep -Eq '192\.0\.2\.10|6443|ipBlock|extraEgress' <<<"$workers"; then fail "worker pods must have no API server route"; fi
# The closed policy names Substrate's own postgres pod (it gets no egress), so only the policies with egress rules are checked.
if grep -Eq '0\.0\.0\.0/0|::/0|postgres|cnpg' <<<"$workers$control$apionly"; then fail "no public or database egress"; fi
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
lacks "the NetworkPolicies are off when disabled" 'name: koinon-substrate-(workers|control)' "${wsub[@]}" --set execution.substrate.workerNetworkPolicy.enabled=false

# WorkerPool (Substrate v1.0.0 WorkerPoolSpec): off by default; workerImage is required; one sandbox
# class; placement and the pull ServiceAccount live under spec.template; the spec has no imagePullSecrets.
# The pool and its ServiceAccount are in the worker namespace, and the template names the chart
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
