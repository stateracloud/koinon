# SPDX-License-Identifier: Apache-2.0
#
# shellcheck shell=bash disable=SC2154,SC2046,SC2206,SC2015
# Chart assertions: the Substrate worker namespace and control-plane namespace (ADR 0120 section 6,
# issue #1089): the admission policies, the NetworkPolicies, the worker Namespace and the default
# WorkerPool. Sourced in order by
# hack/chart-check.sh; it uses that script's helpers (render, has, lacks, ...).

wsub=(--set execution.driver=substrate --set gateway.model.driver=koinon --set gateway.mcp.driver=koinon --set execution.substrate.controlAddress=api.ate-system.svc:443
	--set execution.substrate.routerAddress=atenet-router.ate-system.svc:443 --set execution.substrate.tokenAudience=ateapi
	--set execution.substrate.allowUnhardened=true --set execution.substrate.defaultTier=dev
	--set sandboxObserver.enabled=false --set sandboxNetworkPolicy.enabled=false --set sandboxLimitRange.enabled=false
	# The operator image source paths below; the default in-cluster cache has its own file (45-substrate-imagecache.sh).
	--set execution.substrate.imageCache.enabled=false)
pol=(--show-only templates/substrate-policy.yaml)
np=(--show-only templates/substrate-networkpolicy.yaml)
pool=(--show-only templates/substrate-workerpool.yaml)
wns=(--show-only templates/substrate-workernamespace.yaml)
# nocomment: stdin without YAML comment lines. A line is a comment only when '#' is its first non-blank character: a CEL
# string that holds '#' stays (issue #1176 m1; a filter on any '#' would hide such a line from the checks).
nocomment() { grep -v '^[[:space:]]*#' || true; }
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
# The default pool's pinned image is a worker image (issue #1089); with the pool off and no workerShapes image, no container is.
grep -Fq "(c.name == 'worker' && !('worker' in variables.initNames) && c.image in [\"ghcr.io/agent-substrate/substrate/ateworker-gvisor:v1.0.1\"])" <<<"$wpods" || fail "the default pool image is the one worker image"
nopool=(--set execution.substrate.workerPool.enabled=false)
wpods0=$(docof 'ValidatingAdmissionPolicy\nmetadata:\n  name: koinon-koinon-worker-pods\n' <<<"$(render "${wsub[@]}" "${nopool[@]}" "${pol[@]}" | tr -d '\134')")
grep -Fq "(c.name == 'worker' && !('worker' in variables.initNames) && c.image in [])" <<<"$wpods0" || fail "with no worker image no container is a worker container (fail closed)"
grep -Fq 'object.spec.initContainers.map(c, c.name)' <<<"$wpods" || fail "an init container named worker must never be the worker container"
[[ $(grep -c "c.name == 'worker' && !('worker' in variables.initNames)" <<<"$wpods") -eq 3 ]] || fail "all three worker-container rules must exclude an init container named worker"
# m1: the comment filter keeps a code line that holds '#' and drops only comment lines.
[[ $(printf '%s\n' '    # request.namespace' '    - expression: "a # b"' | nocomment) == '    - expression: "a # b"' ]] || fail "nocomment must drop only comment lines"
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
# The capability set follows the class of the worker image (issue #1176): a gVisor image gets the gVisor set only.
wimg=(--set 'execution.substrate.workerPolicy.workerShapes.gvisor.images={registry.example/g@sha256:aaaa}' --set 'execution.substrate.workerPolicy.workerShapes.microvm.images={registry.example/m@sha256:bbbb}')
cls=$(docof 'ValidatingAdmissionPolicy\nmetadata:\n  name: koinon-koinon-worker-pods\n' <<<"$(render "${wsub[@]}" "${nopool[@]}" "${wimg[@]}" "${pol[@]}" | tr -d '\134')")
grep -Fq 'c.image in ["registry.example/g@sha256:aaaa"] && c.securityContext.capabilities.add.all(x, x in ["NET_ADMIN","SYS_ADMIN","SETUID","SETGID","DAC_OVERRIDE","FOWNER","CHOWN","NET_RAW","SETFCAP"])' <<<"$cls" || fail "a gVisor image must get the gVisor capability set only"
grep -Fq 'c.image in ["registry.example/m@sha256:bbbb"] && c.securityContext.capabilities.add.all(x, x in ["NET_ADMIN","SYS_ADMIN","SETUID","SETGID","DAC_OVERRIDE","FOWNER","CHOWN","MKNOD","SETFCAP","FSETID","SETPCAP"])' <<<"$cls" || fail "a micro-VM image must get the micro-VM capability set only"
grep -Fq 'c.image in ["registry.example/g@sha256:aaaa","registry.example/m@sha256:bbbb"])' <<<"$cls" || fail "the worker images of both classes are worker images"
if render "${wsub[@]}" --set 'execution.substrate.workerPolicy.workerShapes.gvisor.images={registry.example/x@sha256:cccc}' --set 'execution.substrate.workerPolicy.workerShapes.microvm.images={registry.example/x@sha256:cccc}' "${pol[@]}" >/dev/null 2>"$out/cls.err"; then fail "one image in both class lists must not render"; fi
grep -Fq 'a worker image has one sandbox class' "$out/cls.err" || fail "the two-class error must say that a worker image has one class"
if render "${wsub[@]}" --set 'execution.substrate.workerPolicy.workerImages={registry.example/w@sha256:aaaa}' "${pol[@]}" >/dev/null 2>&1; then fail "the class-free workerImages list is gone (an image without a class)"; fi
wpool=(--set execution.substrate.workerPool.enabled=true --set execution.substrate.workerPool.workerImage=registry.example/ateworker-gvisor:v1.0.1)
has "the WorkerPool image is a worker image" '\\"registry.example/ateworker-gvisor:v1.0.1\\"' "${wsub[@]}" "${wpool[@]}" "${pol[@]}"
gpool=$(render "${wsub[@]}" "${wpool[@]}" "${pol[@]}" | tr -d '\134')
grep -Fq 'c.image in ["registry.example/ateworker-gvisor:v1.0.1"] && c.securityContext.capabilities.add.all(x, x in ["NET_ADMIN","SYS_ADMIN","SETUID","SETGID","DAC_OVERRIDE","FOWNER","CHOWN","NET_RAW","SETFCAP"])' <<<"$gpool" || fail "a gvisor pool image must get the gVisor set"
grep -Fq 'c.image in [] && c.securityContext.capabilities.add.all(x, x in ["NET_ADMIN","SYS_ADMIN","SETUID","SETGID","DAC_OVERRIDE","FOWNER","CHOWN","MKNOD"' <<<"$gpool" || fail "a gvisor pool image must not get the micro-VM set"
mpool=$(render "${wsub[@]}" --set execution.substrate.workerPool.enabled=true --set execution.substrate.workerPool.sandboxClass=microvm --set execution.substrate.workerPool.workerImage=registry.example/ateworker-microvm:v1.0.1 "${pol[@]}" | tr -d '\134')
grep -Fq 'c.image in ["registry.example/ateworker-microvm:v1.0.1"] && c.securityContext.capabilities.add.all(x, x in ["NET_ADMIN","SYS_ADMIN","SETUID","SETGID","DAC_OVERRIDE","FOWNER","CHOWN","MKNOD"' <<<"$mpool" || fail "a microvm pool image must get the micro-VM set"
grep -Fq 'c.image in [] && c.securityContext.capabilities.add.all(x, x in ["NET_ADMIN","SYS_ADMIN","SETUID","SETGID","DAC_OVERRIDE","FOWNER","CHOWN","NET_RAW"' <<<"$mpool" || fail "a microvm pool image must not get the gVisor set"
wmir=(--set 'global.registryMirrors.registry\.example=mirror.example/cache')
has "the mirrored worker image is the policy worker image" '\\"mirror.example/cache/ateworker-gvisor:v1.0.1\\"' "${wsub[@]}" "${wmir[@]}" "${wpool[@]}" "${pol[@]}"
has "the mirrored worker image is the pool worker image" 'workerImage: mirror.example/cache/ateworker-gvisor:v1.0.1' "${wsub[@]}" "${wmir[@]}" "${wpool[@]}" "${pool[@]}"
has "worker hostPath list is configurable" 'v.hostPath.path in \[\\"/dev/kvm\\"\]' "${wsub[@]}" --set 'execution.substrate.workerPolicy.allowedHostPaths={/dev/kvm}' "${pol[@]}"
# Digest exemption: the default is the platform controllers; the list is used as written, so [] means none (#1140).
tagline=$(grep -A1 -E 'name: tagAllowed' <<<"$wpods" | tail -1)
grep -Fq 'replicaset-controller' <<<"$tagline" || fail "the digest exemption must default to the platform controllers"
if grep -Fq 'ate-controller' <<<"$tagline"; then fail "the Substrate controller must not be exempt from the digest rule by default"; fi
custom=$(render "${wsub[@]}" "${nopool[@]}" --set 'execution.substrate.workerPolicy.digestExemptIdentities={system:serviceaccount:x:y}' "${pol[@]}")
tagline=$(grep -A1 -E 'name: tagAllowed' <<<"$custom" | tail -1)
grep -Fq 'system:serviceaccount:x:y' <<<"$tagline" || fail "digestExemptIdentities must set the exemption"
if grep -Fq 'replicaset-controller' <<<"$tagline"; then fail "digestExemptIdentities must replace the default list"; fi
mkdir -p "$out/values"
printf 'execution:\n  substrate:\n    workerPolicy:\n      digestExemptIdentities: []\n' >"$out/values/no-exempt.yaml"
none=$(render "${wsub[@]}" "${nopool[@]}" -f "$out/values/no-exempt.yaml" "${pol[@]}")
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
rest=$(tr -d '\134' <<<"$objs" | nocomment | sed "s/has(request.namespace) ? request.namespace : ''//")
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
grep -Fq 'failurePolicy: Fail' <<<"$nppol" || fail "the NetworkPolicy guard must fail closed"
grep -Fq 'variables.kube && (request.operation == '"'DELETE'"' || variables.specUnchanged)' <<<"$nppol" || fail "Kubernetes controllers may delete and change metadata only"
grep -Fq '!has(q.endPort)' <<<"$nppol" || fail "the DNS peer must have no port range"
grep -Fq 'size(p.namespaceSelector.matchLabels) == 1' <<<"$nppol" || fail "a peer must name exactly one namespace label"
grep -Fq 'size(p.namespaceSelector.matchExpressions) == 0' <<<"$nppol" || fail "a peer must have no match expressions"
grep -Fq 'in ["ate-system","koinon"]' <<<"$nppol" || fail "the controller's ingress must come from the control-plane and Koinon namespaces only"
grep -Fq "q.port == 53" <<<"$nppol" || fail "the DNS peer must be limited to port 53"
grep -Fq 'has(r.ports) && size(r.ports) > 0' <<<"$nppol" || fail "the DNS peer needs explicit ports (no ports means every port)"
grep -Fq 'has(r.from) && size(r.from) > 0' <<<"$nppol" || fail "an ingress rule of the controller needs a from list (no from means every source)"
grep -Fq "request.operation == 'DELETE' ||" <<<"$nppol" || fail "a delete has no object: the rules that read the object must skip it"
if grep -Fq 'request.namespace' <<<"$(nocomment <<<"$nppol")"; then fail "the NetworkPolicy guard must not read request.namespace (the binding selects the namespace)"; fi
grep -Fq 'kubernetes.io/metadata.name: "pools-x"' <<<"$(docof 'ValidatingAdmissionPolicyBinding\nmetadata:\n  name: koinon-koinon-worker-netpol\n' <<<"$(render "${wsub[@]}" --set execution.substrate.workerNamespace=pools-x "${pol[@]}")")" || fail "the NetworkPolicy guard follows the worker namespace"
has "the NetworkPolicy guard follows the control-plane namespace" 'metadata.name.\] == "substrate-x"' "${wsub[@]}" --set execution.substrate.namespace=substrate-x --set execution.substrate.gatewayClientNamespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=substrate-x "${pol[@]}"
grep -Fq 'ops-x' <<<"$(docof 'ValidatingAdmissionPolicy\nmetadata:\n  name: koinon-koinon-worker-netpol\n' <<<"$(render "${wsub[@]}" --set 'execution.substrate.workerPolicy.installerIdentities={ops-x}' "${pol[@]}")")" || fail "the NetworkPolicy guard follows the installer identities"
if render "${wsub[@]}" --set 'execution.substrate.workerPolicy.validationActions={Nope}' >/dev/null 2>&1; then fail "an unknown validation action must not render"; fi

# The worker Namespace: created by the chart, with the Pod Security level these pods need.
has "the chart creates the worker namespace" 'kind: Namespace' "${wsub[@]}" "${wns[@]}"
has "the worker namespace name" 'name: "ate-workers"' "${wsub[@]}" "${wns[@]}"
has "the worker namespace carries the name label" 'kubernetes.io/metadata.name: "ate-workers"' "${wsub[@]}" "${wns[@]}"
has "the worker namespace is privileged for Pod Security" 'pod-security.kubernetes.io/enforce: privileged' "${wsub[@]}" "${wns[@]}"

# NetworkPolicies: standard API only.
npdocs=$(render "${wsub[@]}" "${np[@]}")
[[ $(grep -c '^kind: NetworkPolicy' <<<"$npdocs") == 6 ]] || fail "expected six NetworkPolicies (workers, control plane, default-deny egress, API server pods, API-only pods, bucket-init Job)"
closed=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-default-deny-egress\n' <<<"$npdocs")
apionly=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-api-only\n' <<<"$npdocs")
grep -Eq '^  podSelector: \{\}$' <<<"$closed" || fail "the default-deny egress policy must cover every pod of the control-plane namespace"
grep -Fq 'namespace: "ate-system"' <<<"$closed" || fail "the default-deny egress policy must be in the control-plane namespace"
grep -Fq 'egress: []' <<<"$closed" || fail "the default-deny egress policy must deny all egress"
grep -Fq 'policyTypes: [Egress]' <<<"$closed" || fail "the default-deny egress policy must leave ingress to Substrate's own policies"
grep -Fq -- '- k8s-credential-provider' <<<"$apionly" || fail "the API-only policy must select k8s-credential-provider"
apiport() { # apiport <name> <policy doc>: the API server rule is a ports-only rule (no address) on 443 and 6443
	grep -Fq -- '- ports:' <<<"$2" || fail "$1 must carry the ports-only API server rule"
	grep -Fq 'port: 443, protocol: TCP' <<<"$2" || fail "$1 must open the API server port 443"
	grep -Fq 'port: 6443, protocol: TCP' <<<"$2" || fail "$1 must open the API server port 6443"
	if grep -Fq 'ipBlock' <<<"$2"; then fail "$1 must not name an API server address (no CNI-neutral peer; ADR 0120 amendment 10)"; fi
}
apiport "the API-only policy" "$apionly"
grep -Fq 'policyTypes: [Egress]' <<<"$apionly" || fail "the API-only policy must leave ingress to Substrate's own policies"
bucket=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-bucket-init\n' <<<"$npdocs")
grep -Fq 'key: batch.kubernetes.io/job-name' <<<"$bucket" || fail "the bucket-init policy must select the Job pod by Job name"
grep -Fq -- '- rustfs-bucket-init' <<<"$bucket" || fail "the bucket-init policy must select rustfs-bucket-init"
grep -Fq 'app: "rustfs"' <<<"$bucket" || fail "the bucket-init policy must reach the object store only"
grep -Fq '{port: 9000, protocol: TCP}' <<<"$bucket" || fail "the bucket-init policy must reach the S3 port"
grep -Fq 'policyTypes: [Egress]' <<<"$bucket" || fail "the bucket-init policy must leave ingress to Substrate's own policies"
lacks "no bucket-init policy with an empty list" 'substrate-bucket-init' "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.bucketInitJobs=null' "${np[@]}"
# m3 (issue #1176): with bucket-init Jobs the object store values are required (an empty app selects nothing useful and
# an empty port is invalid YAML).
for v in objectStoreApp objectStorePort; do
	if render "${wsub[@]}" --set "execution.substrate.workerNetworkPolicy.$v=null" "${np[@]}" >/dev/null 2>"$out/os.err"; then fail "$v=null must not render with bucket-init Jobs"; fi
	grep -Fq "workerNetworkPolicy.$v is required" "$out/os.err" || fail "the error must name $v"
done
lacks "the object store values are not needed without bucket-init Jobs" 'substrate-bucket-init' "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.bucketInitJobs=null' --set execution.substrate.workerNetworkPolicy.objectStoreApp=null "${np[@]}"
has "the object store app is configurable" 'app: "minio"' "${wsub[@]}" --set execution.substrate.workerNetworkPolicy.objectStoreApp=minio "${np[@]}"
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
# m2 (issue #1176): the prefix length decides, so every spelling of a wide range is refused, not only 0.0.0.0/0 and ::/0.
for c in 0.0.0.0/0 0.0.0.0/1 1.0.0.0/8 100.64.0.0/10 172.32.0.0/12 10.0.0.0/7 ::/0 0::/0 0:0:0:0:0:0:0:0/0 2000::/3 2001:db8::/32 ::ffff:0:0/96 64:ff9b::/96 64:ff9b:1::/48 ::/48 ::/64 ::1/128 0064:ff9b::/96 bogus 300.0.0.0/24; do
	if render "${wsub[@]}" --set "execution.substrate.workerNetworkPolicy.imageSource.cidrs={$c}" "${np[@]}" >/dev/null 2>"$out/cidr.err"; then fail "the image source $c is too wide and must not render"; fi
	grep -Fq "imageSource.cidrs: $c" "$out/cidr.err" || fail "the error for $c must name imageSource.cidrs and the CIDR"
done
for c in 198.51.100.0/24 203.0.113.7/32 8.8.0.0/16 10.0.0.0/8 172.16.0.0/12 fd00::/8 2001:db8:1::/48; do
	has "the image source $c renders" "cidr: \"$c\"" "${wsub[@]}" --set "execution.substrate.workerNetworkPolicy.imageSource.cidrs={$c}" "${np[@]}"
done
# The matchExpressions selector must reach the image-source policy itself, not only some document of the render.
mxdoc=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-image-source\n' <<<"$(render "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.imageSource.podSelector.matchExpressions[0].key=app' --set 'execution.substrate.workerNetworkPolicy.imageSource.podSelector.matchExpressions[0].operator=Exists' --set execution.substrate.workerNetworkPolicy.imageSource.port=5000 "${np[@]}")")
# The policy's own spec.podSelector also has matchExpressions, so read only the peer under "- podSelector:".
mxpeer=$(grep -A4 -E '^ *- podSelector:$' <<<"$mxdoc")
grep -Fq 'matchExpressions:' <<<"$mxpeer" && grep -Fq 'key: app' <<<"$mxpeer" && grep -Fq 'operator: Exists' <<<"$mxpeer" || fail "a matchExpressions pod selector must be the image source of the image-source policy"
grep -Fq '{port: 5000, protocol: TCP}' <<<"$mxdoc" || fail "the matchExpressions image source must use the configured port"
if grep -Fq 'ipBlock' <<<"$mxdoc"; then fail "a matchExpressions image source must not open a CIDR"; fi
grep -Fq 'one image source' "$out/img.err" || fail "the two-sources error must say that there is one image source"
closedcode=$(nocomment <<<"$closed")
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
if grep -Eq -- '- ports:|port: (443|6443), protocol: TCP' <<<"$control"; then fail "the control-plane policy must carry no any-address API server rule (atenet-egress would get it; ADR 0120 amendment 10)"; fi
apisrv=$(docof 'NetworkPolicy\nmetadata:\n  name: koinon-substrate-api-server\n' <<<"$npdocs")
apiport "the API server policy" "$apisrv"
for a in atelet ate-controller ate-api-server atenet-router; do
	grep -Eq -- "- $a\$" <<<"$apisrv" || fail "the API server policy must list app $a"
done
if grep -Eq -- "- atenet-egress\$" <<<"$apisrv"; then fail "the API server policy must not select atenet-egress (no API server client; no TCP route to any address)"; fi
grep -Fq 'kubernetes.io/metadata.name: "ate-workers"' <<<"$control" || fail "the control-plane policy must reach the worker namespace"
# Koinon's namespace reaches ateapi (443) and the router's TLS port (8443) in the control-plane namespace, and no other port (#1446).
koinon_in=$(awk '/^  ingress:/{f=1;next} /^  egress:/{f=0} f' <<<"$control" | nocomment | awk '/kubernetes.io\/metadata.name: "koinon"/{k=1} k')
for p in 443 8443; do
	grep -Eq -- "^ +- port: $p\$" <<<"$koinon_in" || fail "the control-plane ingress for Koinon's namespace must open port $p"
done
[[ $(grep -Ec -- '^ +- port: ' <<<"$koinon_in") == 2 ]] || fail "the control-plane ingress for Koinon's namespace must open exactly ports 443 and 8443"
has "the Koinon ingress ports are configurable" '- port: 9443$' "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.koinonIngressPorts={9443}' "${np[@]}"
if render "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.koinonIngressPorts=null' >/dev/null 2>"$out/sub.err"; then fail "an empty koinonIngressPorts must not render"; fi
grep -q 'koinonIngressPorts' "$out/sub.err" || fail "the empty-ports error must name koinonIngressPorts"
has "the app list is configurable" '- app-x$' "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.controlPlaneApps={app-x}' "${np[@]}"
if render "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.controlPlaneApps=null' >/dev/null 2>"$out/sub.err"; then fail "an empty controlPlaneApps must not render"; fi
grep -q 'controlPlaneApps' "$out/sub.err" || fail "the empty-list error must name controlPlaneApps"
has "worker NetworkPolicy admits Koinon's namespace" 'kubernetes.io/metadata.name: "koinon"' "${wsub[@]}" "${np[@]}"
if render "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.apiServerPorts=null' >/dev/null 2>"$out/sub.err"; then fail "an empty apiServerPorts must not render"; fi
grep -q 'apiServerPorts' "$out/sub.err" || fail "the empty-ports error must name apiServerPorts"
if render "${wsub[@]}" --set execution.substrate.namespace=substrate-x >/dev/null 2>"$out/sub.err"; then fail "a namespace that differs from gatewayClientNamespaceSelector must not render"; fi
grep -q 'gatewayClientNamespaceSelector' "$out/sub.err" || fail "the namespace-mismatch error must name gatewayClientNamespaceSelector"
has "a matching namespace and selector render" 'namespace: "substrate-x"' "${wsub[@]}" --set execution.substrate.namespace=substrate-x --set execution.substrate.gatewayClientNamespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=substrate-x "${np[@]}"
has "the API server rule follows apiServerPorts" 'port: 8443, protocol: TCP' "${wsub[@]}" --set 'execution.substrate.workerNetworkPolicy.apiServerPorts={8443}' "${np[@]}"
lacks "no ipBlock for the API server in any Substrate policy" 'ipBlock' "${wsub[@]}" "${np[@]}"
lacks "no CNI-specific resource" 'cilium|calico|CiliumNetworkPolicy' "${wsub[@]}" "${np[@]}"
lacks "the NetworkPolicies are off when disabled" 'name: koinon-substrate-(workers|control)' "${wsub[@]}" --set execution.substrate.workerNetworkPolicy.enabled=false

# WorkerPool (Substrate v1.0.1 WorkerPoolSpec): on by default with the substrate driver (issue #1089), with the pinned
# worker image of its class and a small size; one sandbox class; placement and the pull ServiceAccount live under spec.template; the spec has no imagePullSecrets.
# The pool and its ServiceAccount are in the worker namespace, and the template names the chart
# ServiceAccount (automountServiceAccountToken false), because the controller's own ateworker ServiceAccount
# leaves the field unset.
wp=(--set execution.substrate.workerPool.enabled=true --set execution.substrate.workerPool.workerImage=registry.example/ateworker-gvisor:v1.0.1)
has "a WorkerPool by default with the substrate driver" 'kind: WorkerPool' "${wsub[@]}" "${pool[@]}"
lacks "no WorkerPool when it is off" 'kind: WorkerPool' "${wsub[@]}" "${nopool[@]}"
lacks "no worker ServiceAccount when it is off" 'koinon-gvisor-worker' "${wsub[@]}" "${nopool[@]}"
lacks "no WorkerPool with the openshell driver even when it is on" 'kind: WorkerPool' --set execution.substrate.workerPool.enabled=true
has "the default pool runs the pinned gvisor worker image" 'workerImage: ghcr.io/agent-substrate/substrate/ateworker-gvisor:v1.0.1$' "${wsub[@]}" "${pool[@]}"
has "the default microvm pool runs the pinned microvm worker image" 'workerImage: ghcr.io/agent-substrate/substrate/ateworker-microvm:v1.0.1$' "${wsub[@]}" --set execution.substrate.workerPool.sandboxClass=microvm "${pool[@]}"
has "the pinned worker image follows the registry mirrors" 'workerImage: mirror.example/gh/agent-substrate/substrate/ateworker-gvisor:v1.0.1$' "${wsub[@]}" --set 'global.registryMirrors.ghcr\.io=mirror.example/gh' "${pool[@]}"
# The size: one replica that requests 500m CPU and 1Gi (limits 4 CPU, 8704Mi: the largest sandbox); every value is settable.
sized=$(render "${wsub[@]}" "${pool[@]}")
grep -Eq '^  replicas: 1$' <<<"$sized" || fail "the default pool has one replica"
sizeline=$(grep -A6 '^    resources:' <<<"$sized" | tr -d '\n ')
grep -Fq 'limits:cpu:"4"memory:8704Mirequests:cpu:500mmemory:1Gi' <<<"$sizeline" || fail "the default pool size is 500m/1Gi requests and 4/8704Mi limits"
has "replicas is the size value" '^  replicas: 3$' "${wsub[@]}" --set execution.substrate.workerPool.replicas=3 "${pool[@]}"
has "the resources are settable" 'memory: 10Gi' "${wsub[@]}" --set execution.substrate.workerPool.resources.limits.memory=10Gi "${pool[@]}"
lacks "resources null leaves the LimitRange defaults" '^    resources:' "${wsub[@]}" --set execution.substrate.workerPool.resources=null "${pool[@]}"
if render "${wsub[@]}" --set execution.substrate.workerPool.resources.requests=null "${pool[@]}" >/dev/null 2>"$out/sub.err"; then fail "resources without requests must not render"; fi
grep -q 'both requests and limits' "$out/sub.err" || fail "the resources error must ask for both requests and limits"
if render "${wsub[@]}" --set execution.substrate.workerPool.replicas=0 "${pool[@]}" >/dev/null 2>"$out/sub.err"; then fail "zero replicas must not render (switch the pool off instead)"; fi
grep -q 'replicas' "$out/sub.err" || fail "the zero-replicas error must name replicas"
if render "${wsub[@]}" --set execution.substrate.workerPool.spec.template.resources.requests.cpu=4 "${pool[@]}" >/dev/null 2>"$out/sub.err"; then fail "spec.template.resources must not render (it would merge with the default resources)"; fi
grep -q 'set execution.substrate.workerPool.resources' "$out/sub.err" || fail "the spec.template.resources error must point to workerPool.resources"
# Capacity: the worker limits must fit the largest sandbox (sandbox.resources.bounds.*.limit.max). The
# memory need is the limit plus the 64Mi reserve, plus 4Mi + 8Mi/GiB for microvm: 8256Mi gvisor, 8324Mi microvm.
capacity_refused() { # capacity_refused <what> <stderr text> <render args...>: the render must fail and name the text
  local what=$1 text=$2; shift 2
  if render "$@" >/dev/null 2>"$out/sub.err"; then fail "$what must not render"; return; fi
  grep -Fq "$text" "$out/sub.err" || fail "$what: the error must say: $text"
}
capacity_refused "a worker cpu limit below the sandbox cpu maximum" 'the worker cpu limit 3 (execution.substrate.workerPool.resources.limits.cpu) is below sandbox.resources.bounds.cpu.limit.max 4' "${wsub[@]}" --set-string execution.substrate.workerPool.resources.limits.cpu=3 "${pool[@]}"
grep -Fq 'or lower sandbox.resources.bounds.cpu.limit.max' "$out/sub.err" || fail "the cpu capacity error must name both values to change"
capacity_refused "a gvisor worker memory limit below 8256Mi" 'is below 8256Mi' "${wsub[@]}" --set execution.substrate.workerPool.resources.limits.memory=8255Mi "${pool[@]}"
has "a gvisor worker memory limit of 8256Mi renders" 'memory: 8256Mi' "${wsub[@]}" --set execution.substrate.workerPool.resources.limits.memory=8256Mi "${pool[@]}"
capacity_refused "a microvm worker memory limit below 8324Mi" 'is below 8324Mi' "${wsub[@]}" --set execution.substrate.workerPool.sandboxClass=microvm --set execution.substrate.workerPool.resources.limits.memory=8323Mi "${pool[@]}"
has "a microvm worker memory limit of 8324Mi renders" 'memory: 8324Mi' "${wsub[@]}" --set execution.substrate.workerPool.sandboxClass=microvm --set execution.substrate.workerPool.resources.limits.memory=8324Mi "${pool[@]}"
has "a lower sandbox maximum allows a smaller worker" 'memory: 2Gi' "${wsub[@]}" --set-string sandbox.resources.bounds.cpu.limit.max=1 --set sandbox.resources.bounds.memory.limit.max=1Gi --set sandbox.resources.bounds.memory.request.max=1Gi --set-string execution.substrate.workerPool.resources.limits.cpu=1 --set execution.substrate.workerPool.resources.limits.memory=2Gi "${pool[@]}"
has "resources null checks the LimitRange default limits (8 CPU, 16Gi fit)" 'kind: WorkerPool' "${wsub[@]}" --set execution.substrate.workerPool.resources=null "${pool[@]}"
capacity_refused "resources null with a LimitRange default below the maximum" '(execution.substrate.workerLimits.limitRange.default.cpu) is below' "${wsub[@]}" --set execution.substrate.workerPool.resources=null --set-string execution.substrate.workerLimits.limitRange.default.cpu=2 "${pool[@]}"
capacity_refused "resources null and no workerLimits" 'the worker pods have no cpu limit' "${wsub[@]}" --set execution.substrate.workerPool.resources=null --set execution.substrate.workerLimits.enabled=false "${pool[@]}"
capacity_refused "a cpu limit from the LimitRange next to a pool memory limit" '(execution.substrate.workerLimits.limitRange.default.cpu) is below' "${wsub[@]}" --set execution.substrate.workerPool.resources.limits.cpu=null --set-string execution.substrate.workerLimits.limitRange.default.cpu=2 "${pool[@]}"
# The runtime class label: Substrate copies the WorkerPool labels to its workers, and the substrate driver selects
# workers on koinon.dev/runtime-class with the runtime class of the sandbox (openshell.runtimeClassName).
has "the pool carries the runtime class of the sandboxes" '^    koinon.dev/runtime-class: "kata"$' "${wsub[@]}" "${pool[@]}"
has "the label follows openshell.runtimeClassName" '^    koinon.dev/runtime-class: "gvisor-x"$' "${wsub[@]}" --set openshell.runtimeClassName=gvisor-x --set 'sandboxPolicy.runtimeClasses={gvisor-x}' "${pool[@]}"
capacity_refused "the vm tier on a gvisor pool" 'the vm tier needs a microVM per sandbox' "${wsub[@]}" --set execution.substrate.runtimeClassTiers.kata=vm "${pool[@]}"
has "the vm tier on a microvm pool renders" '^    koinon.dev/runtime-class: "kata"$' "${wsub[@]}" --set execution.substrate.runtimeClassTiers.kata=vm --set execution.substrate.workerPool.sandboxClass=microvm "${pool[@]}"
has "the hardened tier on a gvisor pool renders" '^    koinon.dev/runtime-class: "kata"$' "${wsub[@]}" --set execution.substrate.runtimeClassTiers.kata=hardened "${pool[@]}"
long=$(printf 'k%.0s' {1..64})
capacity_refused "a runtime class that is not a label value" 'is not a valid label value' "${wsub[@]}" --set "openshell.runtimeClassName=$long" --set "sandboxPolicy.runtimeClasses={$long}" "${pool[@]}"
# The default image is a tag: without the interim exemption of the ReplicaSet controller (#1140) the render fails.
if render "${wsub[@]}" --set-json 'execution.substrate.workerPolicy.digestExemptIdentities=[]' "${pool[@]}" >/dev/null 2>"$out/sub.err"; then fail "a tag pool image without the digest exemption must not render"; fi
grep -q 'has no digest' "$out/sub.err" || fail "the digest error must say that the image has no digest"
has "a digest pool image renders without the exemption" 'workerImage: registry.example/w@sha256:a{64}$' "${wsub[@]}" --set-json 'execution.substrate.workerPolicy.digestExemptIdentities=[]' --set "execution.substrate.workerPool.workerImage=registry.example/w@sha256:$(printf 'a%.0s' {1..64})" "${pool[@]}"
# kubeconform checks the pool against the CRD of the pinned Substrate release (schemas/ate.dev/workerpool_v1.json).
[[ -s hack/chart-check.d/schemas/ate.dev/workerpool_v1.json ]] || fail "the WorkerPool schema for kubeconform is missing"
grep -Fq '"sandboxClasses"' hack/chart-check.d/schemas/ate.dev/workerpool_v1.json || fail "the WorkerPool schema must be the WorkerPool CRD"
has "the default WorkerPool" 'kind: WorkerPool' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
pooldocs=$(render "${wsub[@]}" "${wp[@]}" "${pool[@]}")
grep -Eq 'namespace: "ate-workers"' <<<"$pooldocs" || fail "the WorkerPool must be in the worker namespace"
if grep -Fq 'ate-system' <<<"$pooldocs"; then fail "nothing of the pool belongs in the control-plane namespace"; fi
[[ $(grep -c 'namespace: "ate-workers"' <<<"$pooldocs") == 2 ]] || fail "the WorkerPool and its ServiceAccount must be in the worker namespace"
has "the WorkerPool carries workerImage" 'workerImage: registry.example/ateworker-gvisor:v1.0.1' "${wsub[@]}" "${wp[@]}" "${pool[@]}"
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

# The CA hook Job of issue #1191 may stamp the atelet pod template and change nothing else. Every spec field that the rule
# does not name is compared here, so a weaker rule fails (the cases that deny each change are in test/escape).
cadocs=$(render "${wsub[@]/imageCache.enabled=false/imageCache.enabled=true}" "${pol[@]}" | tr -d '\134')
for f in spec.minReadySeconds spec.revisionHistoryLimit spec.template.metadata.finalizers metadata.ownerReferences metadata.finalizers spec.selector spec.updateStrategy; do
	grep -Fq "object.$f == oldObject.$f" <<<"$cadocs" || fail "the CA stamp rule must keep $f unchanged"
done
grep -Fq "matches('^[0-9a-f]{64}\$')" <<<"$cadocs" || fail "the CA stamp rule must check the format of the stamp"
grep -Fq "variables.caMetaNew.all(k, k == 'deprecated.daemonset.template.generation'" <<<"$cadocs" || fail "the CA stamp rule must compare the annotations of the DaemonSet, except the generation mirror of the API server"

# NOTES say when a runtime class runs at defaultTier (issue #1245). NOTES are printed by install, not by template.
tier_notes() {
	local out rc=0
	out=$("$HELM" install koinon "$CHART" -n koinon --dry-run=client --set "$ADDRESS" "${wsub[@]}" "$@" 2>&1) || rc=$?
	if [[ $rc -ne 0 ]]; then
		fail "helm install --dry-run=client for the Substrate tier NOTES failed: $(head -1 <<<"$out")"
		return 0
	fi
	printf '%s\n' "$out"
}
tn_default=$(tier_notes)
grep -Fq 'run at defaultTier' <<<"$tn_default" || fail "NOTES say the runtime class runs at defaultTier when it has no tier entry"
tn_entry=$(tier_notes --set "execution.substrate.runtimeClassTiers.kata=hardened")
if grep -Fq 'run at defaultTier' <<<"$tn_entry"; then fail "NOTES stay quiet when runtimeClassTiers names the runtime class"; fi

# The vm tier needs a microVM pool, whether the tier comes from an entry or from defaultTier (ADR 0075, issue #1245).
vmtier() { "$HELM" template koinon "$CHART" -n koinon --set "$ADDRESS" "${wsub[@]}" "$@" 2>&1 || true; }
vm_dt=$(vmtier --set execution.substrate.defaultTier=vm)
grep -Fq 'execution.substrate.defaultTier' <<<"$vm_dt" && grep -Fq 'needs a microVM' <<<"$vm_dt" || fail "defaultTier=vm on a gvisor pool must fail the render and name defaultTier"
vm_ok=$(vmtier --set execution.substrate.defaultTier=vm --set execution.substrate.workerPool.sandboxClass=microvm)
grep -Fq 'kind: WorkerPool' <<<"$vm_ok" && ! grep -Fq 'needs a microVM' <<<"$vm_ok" || fail "defaultTier=vm on a microvm pool must render a WorkerPool"
# A sandbox with no runtime class has no worker selector: defaultTier=vm needs the pool to be a microVM then too.
vmnc=(--set openshell.runtimeClassName=null --set sandboxPolicy.enabled=false --set execution.substrate.defaultTier=vm)
vm_nc=$(vmtier "${vmnc[@]}")
grep -Fq 'openshell.runtimeClassName is empty' <<<"$vm_nc" && grep -Fq 'needs a microVM' <<<"$vm_nc" || fail "defaultTier=vm with no runtime class on a gvisor pool must fail the render"
vm_ncok=$(vmtier "${vmnc[@]}" --set execution.substrate.workerPool.sandboxClass=microvm)
grep -Fq 'kind: WorkerPool' <<<"$vm_ncok" && ! grep -Fq 'needs a microVM' <<<"$vm_ncok" || fail "defaultTier=vm with no runtime class on a microvm pool must render a WorkerPool"
# One sandbox class per pool (CRD maxItems 1).
vm_two=$(vmtier --set 'execution.substrate.workerPool.spec.sandboxClasses[0].name=gvisor' --set 'execution.substrate.workerPool.spec.sandboxClasses[1].name=microvm')
grep -Fq 'takes exactly one sandbox class' <<<"$vm_two" || fail "two sandbox classes in one pool must fail the render"
# The error names the class the check used when spec.sandboxClasses overrides sandboxClass.
vm_ov=$(vmtier --set execution.substrate.defaultTier=vm --set execution.substrate.workerPool.sandboxClass=microvm --set 'execution.substrate.workerPool.spec.sandboxClasses[0].name=gvisor')
grep -Fq 'has sandboxClass gvisor' <<<"$vm_ov" || fail "the vm error must name the class of spec.sandboxClasses when it overrides sandboxClass"
vm_en=$(vmtier --set execution.substrate.runtimeClassTiers.kata=vm)
grep -Fq 'runtimeClassTiers.kata is vm' <<<"$vm_en" || fail "an explicit vm entry on a gvisor pool must still fail the render"
