---
status: accepted
date: 2026-11-18
area: runtime-sandbox
tags: [sandbox, isolation, egress, credentials, security, kubernetes]
amends: ["0075", "0097"]
supersedes: ["0045", "0048"]
relates: ["0042", "0044", "0080", "0110", "0119"]
---
# 0120. Execution hardening defaults

- Status: Accepted (decisions D1, D2, D5 and D7 settled by the maintainer on 2026-11-18)
- Date: 2026-11-18
- Area: runtime-sandbox
- Tags: sandbox, isolation, egress, credentials, security, kubernetes
- Supersedes: [0045](0045-openshell-hardening-defaults.md) (wholly); supersedes in part [0048](0048-openshell-spike-results.md) (its execution-layer hardening parts: decision 2 install requirements and decision 3 hardening defaults)
- Amends: [0075](0075-isolation-tiers.md) (the hardening that holds in every tier), [0097](0097-koinon-owned-gateway.md) (the gateway is the only L7 enforcement point)
- Relates to: [0042](0042-ask-tier-park-and-execute.md), [0044](0044-in-sandbox-credential-exception.md), [0080](0080-tool-set-matches-egress-and-escape-hosts.md), [0110](0110-held-copy-isolation.md), [0119](0119-agent-substrate-execution-layer.md) (the ADR that names this one)
- Issue: [#1084](https://github.com/stateracloud/koinon/issues/1084), part of [#1082](https://github.com/stateracloud/koinon/issues/1082)
- Verification: Phase 0 proofs P-B and P-C of [0119](0119-agent-substrate-execution-layer.md) (numbers under Verification); the gVisor class and the snapshot verification path are not yet proven (Open questions)

## Context

ADR 0045 fixed the hardening defaults of the OpenShell driver: `enforcement: enforce` on every L7
endpoint, no secrets in the sandbox environment, Kata as default runtime class. ADR 0048 added the
OpenShell install requirements and more defaults. All of those rules name OpenShell objects (provider
records, endpoints, the supervisor, Kata). ADR 0119 makes Agent Substrate the execution layer, with a
one-minor window in which two drivers exist, and gVisor for the `hardened` and `dev` tiers. A rule that
names one runtime cannot guard three.

What the rules protect does not change. An agent runs code that Koinon does not trust. The code must not
read a credential, must not reach a destination that nobody granted, must not decide its own approval and
must not escape to the host. What changes is who enforces each part.

Phase 0 of ADR 0119 proved two of the parts on a real microVM guest (see Verification). The maintainer's
decisions of 2026-11-18 add three constraints:

- D2: per-binary credential binding is lost; per-route rules in the gateway replace it.
- D5: disk and memory snapshots exist from the start, so the design carries short-lived tokens, snapshot
  encryption, snapshot access control and a verification path from the first day.
- D7: worker pods with extra capabilities (not `privileged`) are accepted on any node; a tainted pool is an operator option.

## Decision

These invariants hold for every driver and every tier. A driver is OpenShell during the dual-driver
window, Agent Substrate with a microVM, or Agent Substrate with gVisor. A new driver or tier must pass
the same contract and escape suites before it ships. A tier or driver may add controls. It must not
remove or weaken one, and no setting, flag or values file turns one off. The one exception is the
NetworkPolicy second fence of 2.4, which keeps its chart switch. The OpenShell driver meets items 1.2,
1.3 and the egress-layer part of 2.1 only through the transition profile of section 7, until the
ADR 0119 phase 1 gateway routes for OpenShell exist.

### 1. No credential inside the guest

1. Provider keys, forge tokens, registry credentials and MCP credentials live only in Koinon's gateway
   ([0097](0097-koinon-owned-gateway.md)) or the secret store. They never appear in guest environment
   variables, images, home volumes, snapshots or logs.
2. The execution layer injects one value: a short-lived actor identity token. It is minted and injected
   outside the guest. Its audience is the Koinon gateway. Its lifetime is 300 to 3600 seconds.
3. The guest sees placeholders only. Git over HTTPS uses a placeholder that the egress layer or the
   gateway swaps on the way out. A credential that Koinon rotates keeps a stable placeholder.
4. The guest has no ServiceAccount token, no SPIFFE socket, no node or API server route and no cloud
   metadata route. The escape suite keeps a spec for each.
5. Per-binary credential binding is gone (D2). The gateway decides per route: which host, method, path
   and tool a credential may reach. One credential serves one route class, and the gateway rejects a
   call outside it.
6. The only exception is [0044](0044-in-sandbox-credential-exception.md). It stays gated and is the only
   path that puts a credential in a home volume. It is never allowed in a snapshot (section 5).

### 2. Default-deny egress, with enforced L7 on every inspected or credentialed route

1. L4: the egress layer denies all traffic by default in every tier. No switch turns this off. The
   allowed destinations are the Koinon gateway, and what an org grants explicitly as L4-only. Wildcard
   hosts stay refused in every tier ([0075](0075-isolation-tiers.md) amendment 13).
2. L7: every route that Koinon inspects, or that carries a credential, is a gateway route. The gateway
   enforces method, path, query and MCP tool rules. The gateway has no audit-only mode. A rule that
   fails to load closes the route. A credentialed route never uses `tls: skip` or an uninspected
   exemption.
3. During the dual-driver window the OpenShell driver keeps `enforcement: enforce` on every L7 endpoint
   it renders. That is a driver fence on top of the gateway rule. It does not replace it.
4. A plain `networking.k8s.io/v1` NetworkPolicy on the sandbox or worker namespaces is a second fence.
   It is on by default and allows egress only to the egress layer, the Koinon control plane (the API and
   gateway Services, never the Kubernetes API server or the kubelet), the object store and DNS. The chart
   keeps its switch (`sandboxNetworkPolicy.enabled`, ADR 0045 amendment 2026-11-11) for clusters whose
   CNI cannot enforce NetworkPolicy. This fence is exempt from the rule that no setting turns a control
   off, because the first fence (2.1) and the guest controls (section 4) do not depend on it. Turning it
   off is reported by the preflight and in the audit log. No CNI-specific feature is used. The preflight fails the `vm` and `hardened` tiers on a
   cluster without a NetworkPolicy engine.
5. Egress hosts and registry rules are gateway routes ([0080](0080-tool-set-matches-egress-and-escape-hosts.md)).
   An org's grant is one record that the gateway reads. Neither driver has a second list.

### 3. Approvals outside the runtime

1. The decision to allow a risky action is made by a human or a policy in the Koinon API, never inside
   the guest or by a process the guest can write to ([0042](0042-ask-tier-park-and-execute.md)).
2. An ask-tier call parks at the gateway. The gateway executes it only after the approval record exists.
   The agent receives the result. It cannot write the record.
3. Each driver has the same rule for control calls into the guest (exec, upload, process streams): only
   agentd's identity may call them, and the identity is checked outside the guest.

### 4. A VM boundary on the golden path, and the guest controls inside it

1. The golden path is a microVM per sandbox (the `vm` tier of [0075](0075-isolation-tiers.md) as
   amended). The `hardened` tier is gVisor on a tainted node pool. The `dev` tier is
   gVisor on kind and CI. A tier below the `vm` tier is never the default of a production install.
2. Inside the guest, the Koinon guest launcher ([#1087](https://github.com/stateracloud/koinon/issues/1087))
   applies, before it starts the payload: `no_new_privs`; a capability drop to the smallest set the
   payload needs; a seccomp filter; and Landlock filesystem rules, with the compatibility level set
   to hard requirement where the tier supports Landlock. The filesystem rules are part of the image
   contract and live in the launcher config.
3. "Where the tier supports them" has one meaning: the guest kernel offers the feature. The launcher
   reports which controls it applied. If the `vm` tier cannot apply one, the sandbox does not start.
   For another tier the launcher records the gap in the audit log and in a platform-admin warning.
   The gVisor tiers have a writable root filesystem by decision (2026-11-19): Substrate v1.0.0 sets the
   root of an actor container writable, and a read-only root breaks `MEMORY` snapshots. Koinon carries no
   Substrate patch. So where Landlock is not available on these tiers, nothing limits writes to the root.
   Issue [#1150](https://github.com/stateracloud/koinon/issues/1150) tracks this; each Substrate update
   reviews it. The read-only root requirement of 2026-11-18 (in `docs/dev/guest-launcher.md`, from #1087) is withdrawn.
4. Whenever the driver passes a seccomp profile to the guest, the preflight checks that the profile
   works in the guest. A probe that must fail with `EPERM` (for example `keyctl`) is the check. For
   OpenShell on Kata this keeps the old guest-seccomp check of 0048 (warn, do not block). For the
   Substrate microVM the launcher installs its own filter, because the actor's OCI profile does not
   reach the guest.
5. A guest runs as a non-root user once the launcher can drop to one. Until then the payload runs as
   root with a reduced capability set and the same controls (see Open questions).
   On the gVisor tiers the amendment of 2026-11-26 (#1387) supersedes this: the payload must run as a
   non-root user, or it does not start.
6. Org isolation does not rely on the guest controls. The boundary between two orgs is the VM or
   sandbox boundary plus the actor identity and the rules of sections 1 and 2.
7. The guest API is closed to agent code (decision of 2026-11-25, issue
   [#1309](https://github.com/stateracloud/koinon/issues/1309)). This applies rule 3.3 to Substrate in every
   tier. The upstream guest server has no socket that the payload can reach: the Koinon guest front
   (`koinon-guest-front`) embeds it, unchanged, and owns the only listener in the pod. The front refuses a
   call without a valid credential and starts no process for it. The credential is an Ed25519 JWS that
   agentd and the reconciler sign for each call. Its lifetime is 60 seconds and its audience is the actor.
   The actor template carries only the public key. The private key stays in a Kubernetes Secret outside
   the sandbox, and the front removes the key variable from its environment before it starts. The front
   pins the audience of the first call that it accepts. It moves the pin only on a signed claim that names
   the old pin (a restore from a snapshot). Koinon carries no Substrate patch for this: the front imports
   the upstream guest package of the commit in `go.mod`. The escape suite proves the rule: a process in
   the sandbox calls `StartProcess` on loopback, and the call fails.

### 5. Snapshots (D5)

A suspend or checkpoint can keep disk state (volumes, rootfs) and memory state. Memory state holds
whatever the agent had in RAM: file contents, model output and org data. It holds no credential, because
of section 1, but it is org data of the highest class. These rules apply to every fidelity and every
driver. A driver may enable the `MEMORY` fidelity only when all of them are met and tested.

1. **No credential in a snapshot.** Section 1 keeps credentials and the actor token out of the guest, so
   a snapshot cannot hold them. A restore always gets a fresh actor token. An old token is never
   restored. The 0044 exception is refused for a sandbox with snapshots on, or its path is excluded
   from the snapshot.
2. **Short lifetime.** A snapshot has an expiry. Substrate resumes a suspended actor from its own
   snapshot. It does not make a second actor from that snapshot, so no two sandboxes share one memory
   state. Single use (a restore consumes the snapshot) is a Koinon rule: the snapshot record of the
   v0.2 design enforces it (amendment of 2026-11-25). Substrate does not enforce it. Tokens stay
   short-lived for the same reason.
   **Golden snapshots are off by default in every tier** (issue #1492, amendment of 2026-11-27). A golden snapshot clones the memory
   of one sandbox into many, which this item forbids. The Koinon template sets
   `GOLDEN_SNAPSHOT_MODE_DISABLED`. An admin can turn golden mode on (`execution.substrate.goldenSnapshots`).
   That is an explicit exception to the no-memory-clone rule: the admin accepts that sandboxes of one
   template start from the same memory and the same random state. Golden mode is not needed for speed
   (see the numbers in [the Substrate install document](../dev/substrate-install.md#golden-snapshots)).
3. **Encryption at rest.** Every snapshot object is encrypted at rest under a key scoped to one org,
   with a key that Koinon controls (the envelope scheme of the secret keys, with a DEK per org and a
   KEK from the secret store). If the driver's own store does not give that, Koinon encrypts the
   export (the tar checkpoints of [0032](0032-agent-home-checkpoints.md)) before it leaves the host.
4. **Access control.** One org's snapshots sit under one prefix or bucket. Only the execution layer's
   own identity can read or write that prefix. One exception: the Koinon reconciler holds a read-only key
   for the snapshot bucket, to compute the content digest of item 5. It is limited to one bucket, not
   to one org's prefix; a check that the prefix belongs to the workspace is open work (#1090). No Koinon API returns raw snapshot bytes to a user. A
   restore is allowed only into the same org (the same atespace or namespace) and the same actor
   template digest. A cross-org restore is refused.
5. **Verification before restore.** Koinon records the SHA-256 digest of each snapshot when it is
   created, with the org, actor, template digest and time, in its own database. Before every restore it
   checks the stored object against that record. A mismatch refuses the restore, raises an audit event
   and an alert, and starts the sandbox cold from the last verified home checkpoint. The same check
   guards the disk-only fidelity. The upstream integrity gap (Substrate T-25) is closed by this
   Koinon-side record. Koinon does not wait for upstream.
6. **A cold path always exists.** A sandbox must be able to start without any snapshot. No run depends
   on a snapshot being restorable.
7. **Deletion.** Deleting an agent, an org or a run's retention window deletes its snapshots and their
   digest records. Deleting the org DEK makes the rest unreadable.

### 6. Admission of workers and sandbox pods (D7)

1. Admission policy is native `ValidatingAdmissionPolicy` only. Never Kyverno. No CNI-specific feature.
2. **Worker pods run with extra capabilities, and Koinon accepts that on any node** (D7). The pods of the
   Substrate worker need root, `SYS_ADMIN`, `NET_ADMIN` and hostPath mounts, and a `privileged` Pod
   Security namespace. They run in their own namespace (the chart value `execution.substrate.workerNamespace`,
   default `ate-workers`), which the chart creates. That namespace holds nothing else. Substrate's
   control plane and the node agent (atelet) stay in `ate-system`. A WorkerPool in another namespace
   needs no Substrate change (checked on kind with Substrate v1.0.0).
3. Koinon's policies, as native `ValidatingAdmissionPolicy` objects, one rule set for each namespace:
   - **Worker namespace.** Only the ReplicaSet controller creates pods, and only for a ReplicaSet
     (the Substrate controller applies the pool Deployments). Image digests pinned. No `hostNetwork`,
     `hostPID` or `hostIPC`. hostPath only from a short list: `/dev/kvm`, `/dev/net/tun`,
     `/dev/vhost-vsock`, `/dev/vhost-net` and `/var/lib/ate`. Volumes of the kinds hostPath, emptyDir,
     configMap, downwardAPI and projected only (no PVC, CSI, ephemeral or network volume). No projected
     `serviceAccountToken` source, and `automountServiceAccountToken` not true: the worker has no
     Kubernetes client (section 2.4). A rule "must be false" would refuse the pool pods, which leave
     the field unset, so the rule is "not true". The chart always names a worker ServiceAccount that sets
     it false unless the values name a ServiceAccount (that one must set it). A container may add
     capabilities, and use Unconfined seccomp and AppArmor profiles, only in the exact shape that Substrate sets
     (amendment 6.8). `privileged`, an unmasked `/proc` and `hostPort` are denied.
     Ephemeral containers are denied.
   - **Control-plane namespace.** Pods come from Kubernetes workload controllers. A pod may mount a
     hostPath only when the DaemonSet controller creates it for a DaemonSet (a controller
     `ownerReference`) and it runs as the ServiceAccount `atelet`, and only for the node agent's own list: the hostPath volumes of atelet's manifest in Substrate v1.0.0
     (`/var/lib/ate`, the kubelet `plugins` and `device-plugins`, `/dev` and two GKE credential paths), which
     is not the worker list.
     Every other pod there gets no hostPath. Digests pinned and no `hostNetwork`, as for workers.
   - **Both.** `pods/proxy` and `services/proxy` are denied to every identity. `pods/exec`, `attach` and
     `portforward` are denied in the worker namespace to every identity. In the control-plane namespace only the
     installer identities may use them, with an audit annotation (amendment 6.8). The worker pool, sandbox config and storage driver objects, and every workload kind, are
     writable only by the installer identity and by Kubernetes' own controllers. The Substrate controller
     applies the pool Deployments in the worker namespace only.
4. **A tainted node pool for workers.** The `hardened` tier requires the tainted pool
   ([0075](0075-isolation-tiers.md)); for the other tiers it is an operator option. The chart documents
   it and the preflight reports whether Koinon's own pods (API, gateway, reconciler, database) can run
   on a node that also runs workers. For a tier where the pool is an option it warns and does not block. Dropping the extra capabilities follows upstream when it allows.
5. During the window the OpenShell sandbox namespaces keep their existing policy: only the Agent
   Sandbox controller creates sandbox pods (rules on the requesting identity, with no break-glass), the
   allowed runtime classes follow the tier, `hostNetwork`, ephemeral containers and
   `io.katacontainers.*` annotations are denied, and the supervisor image is pinned.

6. **Network fence.** The worker namespace has one policy for every pod: egress only to the atenet-egress
   pods and DNS. Workers get no route to the Kubernetes API server and none to the gateway: an actor's
   traffic reaches the gateway through atenet. NetworkPolicies add up, so a native ValidatingAdmissionPolicy guards every
   NetworkPolicy write in the worker namespace ([#1158](https://github.com/stateracloud/koinon/issues/1158)): only the installer and
   the Substrate controller write there, and the controller's policies (the per-pool policy) may allow egress only to the
   control-plane namespace and to DNS. The controller does not write or delete the chart's own policy. The control-plane
   namespace has a policy keyed on an explicit list of `app` labels (the control-plane pods), not on
   "every pod without the worker label": a rule of the second form fails open for a pod that loses its
   label. A pod with another label is not selected, so the list follows the Substrate manifests.
8. **Amendment (issue #1089, fix round 2).**
   - **Worker shape.** Substrate sets one privilege shape on the worker container (`workerpool_apply.go:377-468`):
     user and group 0, not privileged, no privilege escalation, drop `ALL`, add the capabilities of the class,
     seccomp and AppArmor Unconfined. The policy accepts this shape only on the container named `worker` whose
     image is a pinned worker image. Every other container in the namespace adds nothing and is never Unconfined.
     The namespace has no capability ceiling. A spec compares the sets with a vendored copy of the source at the
     pinned commit and fails on a change.
   - **One image source** (at the network layer for ports other than the apiServerPorts of amendment 10). atelet reaches one image source (a pull-through cache): a pod selector for a cache in
     the cluster, or an operator CIDR and port. Actor image references are rewritten by the driver (item 9; the operator values
     are `imageMirrors` and `imageRegistry`), with the digest kept. The mirror name is chosen once, because it is part of the actor
     template hash. The default in-cluster cache is in item 9.
   - **Installer connect.** The installer identities may run `exec`, `attach` and `portforward` in the control-plane
     namespace, because the installer opens a port-forward to the object store. Each request carries an audit
     annotation. No identity may do it in the worker namespace.
   - **Fence.** The control-plane namespace has one default-deny egress policy and per-app allow rules, so a pod with
     no rule has no egress (except the apiServerPorts rule, amendment 10, for the pods that carry it).
   - **Observer.** The Koinon reconciler may read the worker namespace, to say why a sandbox does not start: a
     `Role` and `RoleBinding` (`substrate-observer.yaml`, issue #1162) with `get`, `list` and `watch` on pods, events,
     Deployments, ReplicaSets and WorkerPools. No write, no Secret and no pod subresource. It replaces the OpenShell
     pod observer ([0078](0078-sandbox-observer.md)) for this driver.
9. **Amendment (issue #1170): the default image cache.**
   - **Decision.** With the Substrate driver the chart installs a Zot pull-through registry in the release namespace
     (`execution.substrate.imageCache`, on by default). It is the one image source of atelet. Only the cache has
     a purpose-made internet route (except the apiServerPorts rule, amendment 10): 443 to the internet, not to the private ranges, and only to the upstreams that are listed
     (`ghcr.io`, `docker.io`, `quay.io`; the list is a value). The `substrate-image-source` NetworkPolicy of
     item 8 selects the cache pods in the release namespace on the cache port.
   - **The cache needs no trust.** atelet checks the digest of every image. A reference without a digest is
     as trusted as it was before.
   - **Rewrite in the driver.** The driver rewrites the registry of every actor image to the cache
     (`execution.substrate.imageMirrors`, written to `config.yaml`). The path, the tag and the digest are kept.
     The rewrite is in Go, not in `global.registryMirrors`, because that value also changes how the kubelet pulls
     Koinon's own images. Zot pulls the blobs from its upstream itself, so atelet never meets a redirect host of a
     registry.
   - **The mirror name is part of the template hash.** It is `<release>-imagecache.<namespace>.svc:<port>/<host>`.
     A rename changes the hash of every actor template, so choose it once. Do not change the release name, the
     namespace or the port of a running install without a plan for the templates.
   - **TLS.** The cache serves TLS. The ensure-secrets hook Job makes the certificate (ECDSA P-256, 10 years, a CA
     and a leaf; the CA key is not kept) when it is missing, or the operator names a Secret
     (`imageCache.tls.existingSecret`). There is no rotation path yet.
   - **CA trust of atelet.** Substrate has no option for a CA. The installer patches the atelet DaemonSet: it mounts the
     optional ConfigMap `koinon-imagecache-ca` and sets `SSL_CERT_DIR` to it (the public roots stay). A chart hook Job
     fills the ConfigMap with `ca.crt` of the TLS Secret, never the key, and stamps a hash on atelet and on the cache
     pod, so a rotated CA or certificate restarts them at the next `helm upgrade`. The GKE route has no patch yet.
     Details: docs/dev/substrate-install.md (issue #1191).
   - **Upgrade.** An existing Substrate install re-hashes every actor template (the rewritten image is in the hash).
     The render fails when the values already set `imageSource.podSelector`, `imageSource.cidrs`, `imageMirrors` or
     `imageRegistry`: set `imageCache.enabled=false` to keep them. The hook Job fails, and does not regenerate, when
     the kept TLS Secret does not match the DNS names (a new namespace or cluster domain) or has expired. Delete the
     Secret to make a new one. A new certificate needs a restart of the cache pod.
   - **Zot.** The config sets `http.compat: [docker2s2]`: without it Zot refuses Docker media types.
   - **Open: not proven on a cluster.** The Zot sync configuration has not run against a live Zot, and the kind
     proof (an actor starts with atelet internet egress denied) is not done. Since amendment 10 atelet can open TCP 443 and
     6443 to any address, so that proof can show only that other ports are denied.
   - **Air gap.** The operator turns the cache off (`imageCache.enabled=false`) and sets `imageMirrors` (a map from
     a host to a mirror prefix) or `imageRegistry` for one mirror, and an `imageSource` CIDR for the mirror. The
     chart refuses both at once, so atelet has one source.
   - **Rejected.** Kyverno or a CNI feature to name a registry (a NetworkPolicy cannot name one; rules at the
     cluster edge are the operator's). `global.registryMirrors` for the rewrite (above). A certificate from the Helm
     random functions (not deterministic, and it changes on every render).
7. **No internet egress for atelet** (except the apiServerPorts rule, amendment 10: TCP 443 and 6443 to any address). The gVisor runtime asset comes from the cluster object store, not from
   the internet. Koinon stages the pinned gVisor tarball in the object store and ships a `SandboxConfig` with
   an `s3://` source and the same SHA-256. With that, atelet needs no egress to the internet (other than the apiServerPorts rule), and default-deny
   holds in the control-plane namespace too. Status: the staging is built
   ([#1157](https://github.com/stateracloud/koinon/issues/1157); `substrate-install.md`). The read by atelet from
   the cluster store, with egress denied, is not yet proven. The kind check must prove it, and must restart every
   DaemonSet in `ate-system` after the policies apply.
10. **Amendment (issue #1172): the API server route is a port rule.**
   - **Decision.** The fenced control pods that call the Kubernetes API server may open TCP on the ports in
     `execution.substrate.workerNetworkPolicy.apiServerPorts` (default 443 and 6443) to any address. These pods are the
     `apiServerApps` (default atelet, ate-controller, ate-api-server and atenet-router: their own policy, not the control policy), the `apiOnlyApps`, the certificate (podcert) controller
     and the Prometheus of `otel-system`. The rule names no destination. The value `apiServerCIDRs` is removed: an
     `ipBlock` is not a safe peer (below), so a narrowing value would only fail on some CNIs. An operator who wants a
     narrower rule disables the chart fence for a namespace and writes their own, or adds a rule with `extraEgress`.
   - **Why.** NetworkPolicy has no CNI-neutral peer for the API server. The `kubernetes` Service address and its
     endpoint addresses match as an `ipBlock` on some CNIs and not on others. On kindnet only the endpoint address
     matches. On Cilium in its default `policy-cidr-match-mode`, an `ipBlock` never matches a node address, so no
     `apiServerCIDRs` value works and the podcert controller lost the API server with `i/o timeout`. Other charts
     solve the same problem with ports and no destination: cert-manager allows TCP 80, 443 and 6443 with no `to`,
     and Bitnami charts have `kubeAPIServerPorts` (443, 6443, 8443) to any address
     (research note 2026-11-24, `apiserver-egress-networkpolicy`).
   - **Cost.** A compromised control pod can open a connection to any server on these ports, in the cluster or on the
     internet, not only to the API server. This is wider than the old address rule on a CNI that matched it. What
     stays: the rule is TCP only and covers only the pods named above. Actor and worker pods get no new egress (section
     2.4 holds).
   - **atenet-router is on the actor path.** `atenet-router` is in `apiServerApps`: its ingress mode
     (`--mode=ingress`, the `routerAddress` of the driver) watches EndpointSlices through client-go (Substrate v1.0.0,
     `cmd/atenet/internal/router/router.go:73-95`, Role in `manifests/ate-install/atenet-router.yaml:16-50`). It also
     handles actor traffic. It now has TCP 443 and 6443 to any address, the internet included. Before this amendment it had no
     internet route. A compromise of the router through actor traffic therefore gets this route out.
   - **atenet-egress.** `atenet-egress` is in `controlPlaneApps` but NOT in `apiServerApps`: it has no API server
     client (`atenet-egress.yaml:30-38`, no RBAC), so it gets no TCP route to any address and its egress to the
     gateway and API stays the only route out. The render fails when `apiServerApps` lists the app of the egress pods.
   - **Items 7 and 9 and the image source.** Items 7, 8 and 9 now hold with one exception: atelet (and every other pod
     of `apiServerApps`) can open TCP 443 to any address, so "no internet egress for atelet" and "one image source"
     hold at the network layer for other ports only. A TLS registry on 443 is reachable at L3, and only the image
     rewrite and the `SandboxConfig` source limit atelet to the cache. The other routes of the fence are unchanged.
     The pods still need a valid ServiceAccount token or a client certificate to use the API server, and the API
     server's own RBAC and the admission policies of item 6 bind what they can do.
   - **What would replace it.** A peer for the API server that works on every CNI. The Network Policy API
     (`ClusterNetworkPolicy`, sig-network) has a `nodes` peer, which is alpha, so the chart cannot use it. Track it
     and replace the port rule when it is generally available and the CNIs we run support it.
   - **Proof.** `test/e2e/kind/apiserver-egress.sh` (manual, not in CI) makes a kind cluster, applies the chart's fence,
     and shows that a fenced pod reaches TCP 6443 at an address that the policy does not name and cannot reach another
     port, and that a worker pod cannot reach 6443. The chart check `hack/chart-check.sh` asserts that only the expected
     policies carry an egress rule without a destination.

### 7. OpenShell driver profile for the window

The values that ADRs 0045 and 0048 rendered stay in force for the OpenShell driver until it is removed:
`enforcement: enforce` on every endpoint; no secrets in `SandboxSpec.environment` or template env;
no `allow_uninspected_credentials`; `workspace_mode: managed`; `stable_placeholder: true` for every
credential Koinon rotates; Landlock `hard_requirement`; OpenShell telemetry off; Kata guest seccomp
enabled; the runtime class follows the tier (`kata` for `vm`); the deny-all NetworkPolicy, which keeps
the deny-all shape with no allow rules, a stricter form of 2.4. They are a profile of this ADR, not separate decisions.

The install requirements of 0048 decision 2 that guard credentials and attachment also stay in force for
the OpenShell driver: the OpenShell `vault` credential driver on its own KV mount, outside any broadly
usable External Secrets store, reached over HTTPS, with a policy limited to create/read/update on
`<mount>/data/openshell/provider-credentials/*` and delete on the metadata path; the label
`openshell.ai/sandbox-attachable: "true"` on every RuntimeClass Koinon uses; and a CNI that enforces
egress NetworkPolicy (the fence of 2.4 depends on it). Preflight keeps checking them until the driver is removed.

Transition exceptions: today the OpenShell driver injects a gateway key, not the short-lived
actor token of 1.2, and credentials live in OpenShell provider records with the supervisor swapping
the placeholder inside the sandbox pod, which counts as its egress layer for 1.3 and 2.1 (this is why
`stable_placeholder: true` stays). Exit condition: the ADR 0119 phase 1 gateway routes for OpenShell
(forge and registry routes at the Koinon gateway) and the actor token; after that the driver meets
section 1 in full, and a change is checked against 1.2 and 1.3 as written. The profile goes when
ADR 0119 phase 5 removes the driver. Decision 4 and 5 of 0048's execution layer (flavor image contract, ACP over exec
stdin) are not hardening rules and stay with 0048 until the driver work (#1087, #1094) replaces them.

### 8. Proof

Every invariant above has a spec in the driver-neutral escape suite ([0119](0119-agent-substrate-execution-layer.md),
phase 3) and an assertion in the chart checks where the chart renders it. A change that removes such a
spec needs a new ADR.

## Alternatives considered

- **Keep ADR 0045 and amend it for Substrate.** Rejected. Each rule names OpenShell objects. Three
  drivers would need three readings of one text, and the text is the thing agents check against.
- **One ADR per driver.** Rejected. The invariants are the same. Two documents drift apart.
- **Enforce L7 at the egress layer (host and port rules) as well.** Rejected. The egress layer sees
  headers and not bodies. The gateway already reads method, path and tool. One enforcement point is
  easier to prove (0097).
- **Disk snapshots first, memory later.** Rejected by D5: it moves the hard part (encryption, access
  control, verification) to a time when data already exists. We design it now and gate the `MEMORY`
  fidelity on it.
- **Trust the driver's snapshot integrity.** Rejected. Upstream has an open item for it (T-25). A
  digest record in Koinon's database costs one row per snapshot.
- **Require a tainted worker pool.** Rejected by D7. It blocks small installs. It stays an operator
  option with a preflight report.
- **Reuse snapshots many times (clone).** Rejected as a default. Clones share random state and memory
  contents. A fork feature may add it later with fresh entropy and its own ADR.

## Consequences

- Positive: one hardening rule for every driver and tier; a new driver cannot weaken it by omission.
  The AGENTS.md hard rule names one ADR. The gateway is the one L7 point.
- Positive: snapshot rules exist before the first snapshot does.
- Negative: the guest launcher ([#1087](https://github.com/stateracloud/koinon/issues/1087)) becomes
  a hard dependency of the `vm` tier. Memory snapshots cost a digest, an encryption step and a
  single-use restore, so they are slower to roll out than plain suspend.
- Negative: workers with `SYS_ADMIN` enlarge the per-node trusted base (an escape from the guest into the
  VMM lands in a root pod). Section 6 limits the blast radius with policy and an optional pool. It does
  not remove the risk.
- Negative: the loss of per-binary credential binding is real. A process in the guest that can reach a
  route can use the credential that the route carries. The per-route rule is the only guard.
- Follow-ups: the launcher and the non-root drop (#1087); the gateway as the only L7 point (ADR 0097
  step 2, decision D11); the driver-neutral escape suite (phase 3); the worker-namespace policies and
  chart assertions; the snapshot digest table and its migration; AGENTS.md text of #1094 for the removal
  of the OpenShell rules.

## Open questions

- Substrate's own snapshot encryption and bucket layout: does its store meet 5.3 and 5.4, or does Koinon
  encrypt every export? The answer decides how much code the `MEMORY` gate needs. Tracked in
  [#1090](https://github.com/stateracloud/koinon/issues/1090).
- gVisor: Landlock, seccomp and capability drop have not run in the `hardened` guest. Phase 0 did only the
  microVM. Until it runs, the `hardened` tier's guest controls (section 4.2) are a requirement, not a proof. Tracked in [#1092](https://github.com/stateracloud/koinon/issues/1092).
- The guest runs payloads as root without `CAP_SETUID` or `CAP_SETGID`, so the launcher cannot drop to
  a non-root user ([#1087](https://github.com/stateracloud/koinon/issues/1087), an upstream request). Closed for the gVisor tiers by
  the amendment of 2026-11-26 (#1387); open for the `vm` tier ([#1462](https://github.com/stateracloud/koinon/issues/1462)).
- Snapshot expiry values and the retention of the digest record: set when the first snapshot feature
  lands. Tracked in [#1090](https://github.com/stateracloud/koinon/issues/1090).

## Upstream verification

Phase 0 on 2026-11-17 and 2026-11-18, against Agent Substrate v1.0.0 on a Talos cluster with nested KVM
(the full record is in ADR 0119, Verification). Documents read for the prior art were read on
2026-11-18 as documentation only; no upstream source was re-read at a commit for this ADR.

## Verification

- **P-C, guest hardening: 27 of 29 escape probes pass.** A Go launcher in the microVM guest applies
  Landlock (ABI 7), seccomp and `no_new_privs`. The same probes as root without the launcher: 17 open.
  The gap is the missing non-root drop (above). The gVisor class has not run.
- **P-B, gateway-only egress: 17 probes behave as expected.** The gateway by name returns 200 with the
  injected actor token. Blocked: the gateway ClusterIP, public IPs (HTTP and tcp/22), the metadata
  address, the API ClusterIP, node port 6443, kubelet port 10250 and outside DNS (UDP and TCP). Removing
  the egress policy closed the route in about 10 s.
- **Not yet proven:** snapshot encryption, access control and digest verification (section 5), which
  need the first snapshot feature; TLS to the gateway from the guest; the gVisor class.

## Prior art

A quick check (about 15 minutes), as ADR 0119 did. The full research workflow was not run; this ADR
records settled maintainer decisions. See the summary of the lane for that gap.

- gVisor security model: a second kernel in user space narrows the host syscall surface; the sandbox
  still needs a network and file policy. We borrow gVisor for `hardened` and `dev`, and we do not count
  it as the egress or credential control. <https://gvisor.dev/docs/architecture_guide/security/>
- Firecracker production host setup: the VMM runs under the jailer, with seccomp and a minimal
  capability set, and the guest is untrusted. We borrow "the guest is untrusted, the VMM is the target"
  as the reason for section 6. We differ: Substrate's VMM runs without a jailer or seccomp today (D7
  accepts it). <https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md>
- Landlock: unprivileged, stackable filesystem rules that need `no_new_privs`. We borrow the order
  (`no_new_privs`, then rules) and the hard-requirement level. <https://docs.kernel.org/userspace-api/landlock.html>
- Fly Machines suspend: single-use snapshots against clone attacks, always a cold path. We borrow both
  for section 5.2 and 5.6. <https://www.fly.io/docs/reference/suspend-resume/>
- Modal sandboxes: gVisor, default-deny egress, memory snapshots opt-in. We borrow the default-deny
  stance. <https://modal.com/docs/sdk/py/latest/Sandbox.md>
- E2B snapshots: a filesystem-only mode that reboots on resume. We borrow the cold path.
  <https://e2b.dev/docs/sandbox/snapshots>

## Links

- [0045](0045-openshell-hardening-defaults.md) and [0048](0048-openshell-spike-results.md), superseded
  in the parts above.
- [0075](0075-isolation-tiers.md), [0097](0097-koinon-owned-gateway.md),
  [0119](0119-agent-substrate-execution-layer.md), [0044](0044-in-sandbox-credential-exception.md),
  [0042](0042-ask-tier-park-and-execute.md).
- Issues: [#1084](https://github.com/stateracloud/koinon/issues/1084), epic
  [#1082](https://github.com/stateracloud/koinon/issues/1082), launcher
  [#1087](https://github.com/stateracloud/koinon/issues/1087), removal rules
  [#1094](https://github.com/stateracloud/koinon/issues/1094).

## Amendment (2026-11-25, issue #1090): what is built of the snapshot rules in v0.1

Maintainer decisions of 2026-11-24 and 2026-11-25. They change when section 5 is complete, not what it
requires.

- **Items 2, 3 and 5 for Substrate memory snapshots (tags) move to v0.2**, as one design: the snapshot
  record (org, actor, template digest, time), single use, expiry, the digest check before a restore, and
  the per-org encryption. No Koinon caller creates tags in v0.1. The driver's own digest check stays: it
  takes a digest of a tag, fails closed without a content digest and refuses a wrong digest (contract
  spec), and nothing outside `internal/execution` calls it yet.
- **v0.1.0 has no integrity check on Substrate suspend snapshots.** The security notes of the install
  document say so: the snapshot store is trusted, and write access to it gives control of suspended agents.
  Item 4 (a check that the prefix belongs to the workspace) moves with it. The installer default for the
  store stays 10Gi, and the install document gets a sizing rule (the memory in use of the agents that can
  be suspended at one time, with the limits as the worst case).
- **The home has a digest in v0.1.** The Koinon kopia checkpoint of the home records a SHA-256 digest and a
  restore refuses a mismatch ([0032](0032-agent-home-checkpoints.md), amendment of 2026-11-25). So the cold
  path of item 6 (start from the last verified home checkpoint) is a verified path.
- **`keep_warm` default becomes 0** (issue #1090, decision 1): a warm agent saves about 1 second and holds
  its full limits in the pool. The feature stays. The default change is a chart and reconciler change that
  another item carries.
- **The durable directory is in a snapshot.** It is part of every fidelity's snapshot, so a snapshot holds
  the home. Section 1 still applies: the home holds no credential, and the 0044 exception stays refused for
  a sandbox with snapshots on.

## Amendment (2026-11-26, issue #1086 part C2): the actor token file in the guest, an interim

Maintainer decision of 2026-11-26 (PR 1295). It is an INTERIM. Issue #1386 is its exit.

- **What "minted and injected outside the guest" (section 1.2) means until #1386.** koinon-agentd mints the
  actor token outside the guest. It writes the token through the driver's exec into exactly one guest file,
  `/tmp/.koinon-egress/token.gitconfig` (`codehost.SandboxEgressToken`). The file is outside the home. Git
  reads it and sends the token only to the gateway's egress prefix, as the `Koinon-Actor-Token` header.
- **Limits of the interim.**
  - The agent user can read the token while the run is live. Other processes of the guest can read it too.
  - Its lifetime is 15 minutes (inside the 300 to 3600 seconds of section 1.2). koinon-agentd writes a new
    token every 5 minutes and removes the file when the run ends.
  - Its audience is the Koinon gateway. It names the org, the agent and the run. The gateway accepts it on
    every egress route in scope for that org and that agent (`route.inScope`): the agent's own routes, the
    routes of its org and the platform routes, for forge and registry hosts. A token that leaks out of the
    guest therefore reaches what any egress of that agent could reach, and only until it expires. This PR
    does not narrow the routes.
  - The token is not a credential: it holds no forge, registry, provider or MCP credential. Section 1.1
    does not change.
- **Section 5.1 until #1386.** It reads: credentials stay out of the guest, and koinon-agentd never writes
  the actor token into the home. The token file is outside the home, so a home checkpoint does not hold it
  by itself. But the agent can read the file, so a hostile agent can copy the token into the home, and the
  next home checkpoint then holds it. The 15-minute lifetime limits this, and #1386 removes it. The write
  command refuses a token directory that is a symbolic link, is not a directory, or is not owned by the
  writer with mode 0700, and it renames the file into place with `mv -T`. These checks stop accidents. They
  do not stop a hostile agent. A memory snapshot of the whole guest would hold the token, so section 5 must
  handle the file before a driver enables the `MEMORY` fidelity.
- **The escape suite checks it.** The Substrate spec `holds the actor token only in the egress token file that
  koinon-agentd writes` writes a real token with koinon-agentd's command (`codehost.WriteEgressTokenCommand`).
  After koinon-agentd's write command, it fails if the token or another JWT-shaped value is in any other
  place that the scan reads: another file under the scanned roots (`/dev/shm` included), the environment or
  the command line of a process. The spec runs only the write command, not git or a harness.
- **Exit (#1386).** A hop outside the guest adds the identity header, and the guest holds no token. Then the
  token file, its command and this amendment are removed, and the escape spec expects to find no token.

## Amendment (2026-11-26, issue #1386): the egress layer adds the actor identity, the guest holds no token

Accepted (2026-11-26). The maintainer decided on 2026-11-26 that #1386 ships before the v0.1.0 final tag.
The maintainer reviewed this amendment before the code and accepted it on 2026-11-26, with items 5 and 6.
The interim amendment above (#1086 part C2) stays until the second pull request of #1386 merges. That pull
request removes it.

**Scope and order.** This amendment covers the Substrate driver only. The code of #1386 comes after three
other issues:

- #1094 removes the OpenShell driver. So #1386 writes no OpenShell code. Main's decision of 2026-11-26 (issue
  #1386) splits #1386 in two pull requests. The first does the Substrate path before #1094: the header rule, the
  Substrate verifier, the actor record, the chart fences and the escape spec. OpenShell keeps the interim token file.
  The second comes after #1094. It removes the token file, its writer, the Ed25519 path and the interim amendment.
- #1446 puts the agentd leg to the Substrate router on TLS. Today every Exec through the router fails.
- #1444 makes the Substrate driver write the actor's `EgressPolicy`: default deny plus the routes to the
  gateway. Every actor gets the model and MCP listeners. Only an actor whose agent has a code-host grant gets
  the egress listener, as on the OpenShell path. The policy is a separate object under the actor, not part of
  the template: the driver writes it before each create, restore, start and resume, and changes it in place
  when the grant changes. #1444 owns the policy. #1386 adds the identity header to the gateway rules of that
  policy. It does not write the policy itself.

### What section 1.2 means

"Minted and injected outside the guest" means this. A component outside the guest adds the actor identity
header to each request that goes from the guest to the gateway. No process in the guest can read the token.
The guest has no file, environment variable, command argument or memory that holds it. The guest sends only a
fixed placeholder in the `Koinon-Actor-Token` header. The placeholder is public and it is the same in every
sandbox. The egress layer replaces it. A request without the header is not changed, and the gateway refuses
it (`no_token`).

### Decision

1. **The gateway rule adds the header.** #1386 adds one effect to the gateway rule of #1444:
   `replace_headers` with the header `Koinon-Actor-Token`, from `actor_jwt` with the gateway audience and
   `expiration_seconds: 900`. The rule holds no org host and no credential. The gateway still owns the L7
   rules and the credentials.
2. **atenet-egress finds the actor from the transport.** In the worker pod, atunnel presents a certificate for the
   actor. Its SAN is `spiffe://substrate-actor.local/worker-for-actor/<atespace>/<name>`: it names the actor, with no
   UID. It is valid for 1 hour, and ateapi checks the UID only when it issues it
   (`cmd/ateapi/internal/workerservice/certificate.go`). atenet-egress accepts the certificate only for an actor that
   is placed on a worker (running or resuming). It checks this once for each tunnel connection (CONNECT), not for each
   request (`egress.go`, `validateActor`), and `MintActorJwt` does not check the state of the actor. So the identity
   comes from the connection, not from a value that the guest sets, but only Koinon's record (item 5) checks each
   request. For a request that carries the header, atenet-egress gets a token from ateapi (`MintActorJwt`). A request
   without the header goes out unchanged, with no token. It writes the token over the value of the guest
   (`OVERWRITE_IF_EXISTS_OR_ADD`). It keeps each token until a third of its lifetime remains. Each failure refuses the
   request: 403 for an actor that is not found, 503 when ateapi is unavailable, and 500 for all other errors, the
   4-second injection timeout and a canceled request included (`mapActorJWTError`). Source:
   `cmd/atenet/internal/router/egress/credentials.go` (`applyEffects`, `actorJWT`) and `actorjwt/actorjwt.go`.
   - atenet-egress can also read the certificate from the `x-forwarded-client-cert` header (`SANITIZE_SET`). Upstream
     has an open `TODO(identity)` to audit that the actor cannot set this header (`egress.go`). A control spec proves
     that a guest that sends its own `x-forwarded-client-cert` does not change the actor that the gateway sees. If it
     fails, the code of #1386 does not ship.
3. **The actor JWT goes only to the gateway egress listener, and its URL is `http`.** `replace_headers` works on
   an `HttpRule` and also on an `HttpsRule` (MITM): the lab test of 2026-11-27 showed that an `HttpsRule` replaces
   the header too, and then the upstream host, or the guest through an echo, sees the token. So the driver puts
   `actor_jwt` on exactly one rule: the clear-text `HttpRule` of the code-host egress route, with its one host and
   its one port. It refuses to write a policy where any other rule, of any kind, has `replace_headers`
   (`checkActorTokenEffects`, `internal/execution/substrate/egresspolicy.go`). The driver writes no `HttpsRule`.
   An `https` egress URL would not match that one rule, so `CheckEgressURL` (`internal/agentd/actortoken.go`)
   refuses it on Substrate with a clear error. The chart URL is `http` already.
4. **The gateway verifies the Substrate token.** The Substrate verifier replaces the Ed25519 verifier. Until #1094
   removes the OpenShell driver, the execution driver of the install selects the verifier: Substrate gets this
   verifier, and OpenShell keeps the Ed25519 verifier and the interim token file. The second PR of #1386 deletes the
   Ed25519 path.
   - It trusts exactly one issuer: the `--actor-jwt-issuer` of the Substrate install (the installer default is
     `https://idp.<substrate namespace>.svc`). The value comes from the chart values, never from the `iss` of
     a token. The gateway reads `<issuer>/.well-known/openid-configuration`. The `issuer` of that document
     must be equal to the configured issuer. Then the gateway reads the `jwks_uri` and keeps the keys in a
     cache. A token with an unknown `kid` makes the gateway read the keys again, at most once each minute.
     So a key rotation needs no restart. If the gateway cannot read the keys, it refuses the request. It
     never allows a request on an error.
   - The gateway verifies the TLS certificate of the issuer with a CA bundle from the chart values (the CA of
     the Substrate install, which signs the certificate of `ate-idp-server`). There is no option to skip the
     check. Without the CA bundle, the gateway does not start the Substrate verifier.
   - The gateway accepts only `ES256`: ate-idp-server signs `ES256` only (lab test of 2026-11-27). It refuses
     all other values of `alg`, `RS256`, `none` and `HS*` included, with an error that names the `alg`, and
     it does not use a published key with another `alg`.
   - The token must have the gateway audience in `aud`, a valid `exp` and `nbf`, and a lifetime of 300 to
     3600 seconds (section 1.2). The lifetime is `exp - iat`. ateapi sets `nbf` to 5 minutes before `iat`
     (`cmd/ateapi/internal/controlapi/actor.go`), so `exp - nbf` is the lifetime plus 300 seconds. Its `sub` must be `actor/<atespace>/<name>`, and the claim `actor.ate.dev`
     must name the same actor. The gateway refuses a `worker-for-actor` token.
   - The gateway refuses a request that has more than one value of `Koinon-Actor-Token`. Today
     `route.Request` reads only the first value (`Header.Get`, `internal/gateway/route/route.go`).
5. **Koinon's own record names the agent.** The gateway does not trust the token for the org or the agent.
   It finds them in Koinon's record of the actor, for each request:
   - The driver creates the actor in the org's workspace (the atespace, ADR 0002). The actor name is the
     sandbox name plus a random part (item 6). `agent_sandboxes` records the workspace next to the org and
     the agent. #1386 adds two columns: `actor_name` and `actor_uid`. The driver writes both for each actor
     that it creates. A new column is necessary, because the CHECK of `sandbox_name` allows at most 19
     characters, and `ka-` plus 16 hex characters already uses all 19.
   - The gateway gets this map through the State stream, as it gets the routes. The map gives the org slug,
     because `actortoken.Actor.Org` is the slug, not `org_id`. The `actorUID` of the token must be equal to the
     recorded UID. An actor that is not in the map, or a UID that is not equal, is refused. As in kagent, the
     check is on the row: the workspace, the actor name, the actor UID, and a sandbox that is not deleted or
     archived. So an archived or
     deleted sandbox loses its identity at the next State update. The route check (`route.inScope`) does
     not change.
   - A bad row in the map (a missing field, or two rows with one identity) is skipped and reported, and the
     other rows stay (issue #1485). The actor of a skipped row is refused. Two rows with one identity are both
     dropped, because the gateway cannot know which is right.
   - A new actor is not in the map until the next State update. So its first request can be refused. This
     fails closed. koinon-agentd waits until the gateway knows the actor before it starts the run, as
     `forgewait.go` waits for the forge credential.
   - The run: `agent_sandboxes` has no run today. The gateway log names the run when the State stream gives
     the run that holds the sandbox. Else the run claim stays empty, as `actortoken.Actor` already allows.
6. **Each actor gets its own name.** atenet-egress keys its token cache by actor name, and it reuses a token
   for up to two thirds of its lifetime. Sandbox names come from a hash of the org and the agent (and the
   task, for an ephemeral agent, `internal/execution/names.go`). So a new actor can have the name of an old
   one and get a token with the old UID. Item 5 refuses that token, so it fails closed, but the new actor
   cannot reach the gateway for up to 10 minutes. To prevent this, each new actor gets a name that no earlier
   actor had: `<sandbox name>-<16 random hex characters>` (64 bits; issue #1485: with 8 hex characters, a lab test on 2026-11-27 got the UID of a deleted actor for a reused name), recorded in `agent_sandboxes.actor_name` (item 5).
   A new name is made for each new actor and is never used again. Kagent does the same with `session-<uuid>`.
7. **Network and install.** The gateway needs a route to the issuer and to its `jwks_uri`. The chart adds
   one NetworkPolicy egress rule from the gateway for this route, and a chart check asserts it. The rule
   allows the pod port of `ate-idp-server` (8443, named `https`), not the Service port 443, because a
   NetworkPolicy matches the port after the Service maps it. This rule is
   for the verifier. It is not the guest's route to the gateway, which #1444 owns. Substrate's own
   `ate-idp-server` policy admits any source on its `https` port (`ate-system-networkpolicies.yaml`). The
   preflight checks that the gateway can read the discovery document. The gateway does not get a route to
   ateapi.
8. **No route skips the hop.** The egress listener of the gateway accepts Substrate traffic only from
   atenet-egress. Today the egress listener port admits all pods of the Substrate namespace
   (`charts/koinon/templates/networkpolicy.yaml`). The chart NetworkPolicy of the gateway then admits that
   port only from the atenet-egress pods (a `namespaceSelector` and a `podSelector`, no CNI-specific
   feature). A guest that connects to the gateway in a different way gets no answer. A chart check asserts
   this. This rule is the second fence of section 2.4. It stops working when `sandboxNetworkPolicy` is off or
   the CNI does not enforce NetworkPolicy. The first fence is the `EgressPolicy` of #1444.

### What is removed

- The guest token file `/tmp/.koinon-egress/token.gitconfig` and the interim amendment above.
- In `internal/codehost`: `WriteEgressTokenCommand`, `RemoveEgressTokenCommand`, `SandboxEgressDir`,
  `SandboxEgressToken`, `EgressTokenGitConfig`, the `[include]` of the token file in `EgressGitConfig`, and
  `egresstokenscript_test.go`.
- The refresh loop in koinon-agentd that writes into the guest (`writeActorToken`, `keepActorToken`).
- The koinon-agentd minter and the Ed25519 path. Today the only user of the minter is the guest token file
  (`internal/agentd/actortoken.go`). The forge-wait probe (`forgewait.go`) reads the database and does not
  call the gateway. So #1386 removes:
  - `Config.ActorTokens`, the coupling check in `actortoken.go` and the Ed25519 verifier of the gateway. The
    check in `egressGit` (`actortoken.go`) then uses `EgressURL`, not `ActorTokens`.
  - The `actor-token-key` Secret: its seed in `_helpers.tpl`, the ensure-secrets kind `ed25519-key`
    (`internal/ensuresecrets/ed25519.go`, `ensure-secrets.yaml`) and the value
    `gateway.mcp.koinon.egress.actorToken.existingSecret`.
  - The key mounts: `--actor-token-keys-dir` of the gateway (`gateway-koinon-mcp.yaml`) and the mount of
    koinon-agentd (`deployments.yaml`).
  - The flags `actorTokenIssuer` and `actorTokenKeyID` of koinon-agentd and koinon-gateway
    (`cmd/koinonagentd/main.go`, `cmd/koinongateway/main.go`).
- The git configuration of the guest keeps the `insteadOf` prefix and gets the placeholder header. It holds
  no secret.

### Alternatives

- **(2) A Koinon credential provider behind atenet-egress.** atenet-egress calls a Koinon service that adds
  a Koinon token. Later cost: a Koinon service is on the path of every egress request, it needs a new route,
  and Koinon keeps two minters.
- **(3) Keep the token file (the interim).** Later cost: the token is in the guest and in each home copy,
  memory snapshots stay blocked, and a leaked token is valid for up to 15 minutes.

### Proof

- The escape spec `holds the actor token only in the egress token file that koinon-agentd writes` becomes:
  the guest holds no actor identity token. First the spec sends a real request through the egress layer (a
  git fetch with the placeholder) and checks that it succeeds. Then it scans the same places (the roots,
  `/dev/shm`, the environment and command line of each process). It finds no JWT-shaped value, with no
  exception. A scan without the request proves only that nothing was written before it.
- A control spec: a request from the guest with the placeholder reaches a route of its own agent. The gateway
  accepts the added token.
- A control spec: the same request with a token that the guest made itself does not reach the gateway as
  sent. atenet-egress writes over the value, so the gateway sees only the minted token.
- A gateway test (not a guest spec): a request with two values of the header is refused. A guest spec
  cannot test this, because atenet-egress writes one value over the header.
- A control spec: a guest of another org sends the same request, and the gateway refuses it.
- A control spec: after the sandbox is archived, a token of its actor is refused.
- Section 5.1 holds as written: no credential and no token are in the guest, so no home checkpoint and no
  snapshot can hold one. The token is only in the memory of atenet-egress. So D5 of ADR 0119 holds.

### Threat model

- **Who attacks:** code in the guest (a hostile agent or a prompt injection), and a reader of a home
  checkpoint or a snapshot.
- **What changes:** the guest can still send requests as its own agent through the egress layer. That is the
  identity's purpose, and the routes limit what it can do (section 2). But the guest cannot read the token. So
  it cannot copy the token out of the sandbox, put it in the home, or use it after the sandbox stops.
- **New trust:** the gateway trusts atenet-egress, ateapi and the signing key of the Substrate issuer. A
  holder of that key can act as any actor. ADR 0119 already trusts Substrate's control plane with the guest
  itself. So this adds no new party. The gateway maps an actor only through Koinon's own record (item 5).
  Two pods hold the signing keys: ate-api-server signs with them, and ate-idp-server mounts the same Secret
  `actor-id-jwt-pool`, with the private keys, to publish the key set (`manifests/ate-install/ate-idp-server.yaml`).
- **What is left:**
  - The leg from atenet-egress to the gateway is HTTP inside the cluster, as the leg from the guest is today.
    A reader of the pod network can see the token while it is valid (15 minutes). TLS to the gateway is not
    proven yet (Verification). It stays an open question.
  - All processes in the guest have the same identity. The gateway cannot tell them apart.
  - When ateapi is down, atenet-egress cannot mint, and gateway requests get 503 until it is back.

### Open points for the code

- Where ate-idp-server gets the public keys of ateapi, and how it publishes a new key at a rotation.
  Answered from the source (v1.0.1): it serves `/.well-known/openid-configuration` and `/openid/v1/jwks`
  (`cmd/ate-idp-server/internal/server/server.go`). It reads the key pool from the Secret that ateapi signs
  with, again at most once each minute, and publishes the active and the inactive keys
  (`internal/localjwtauthority/localjwtauthority.go`). A rotation is `kubectl ate admin add-jwt-key`, then
  `activate-jwt-key`, then `remove-jwt-key`. So a new key is in the key set before ateapi signs with it. The
  code of #1386 still proves the discovery path with a spec before it depends on it.
- Where the key of the actor certificate is on the microVM tier (ADR 0075). It must be outside the guest
  kernel. The upstream source puts it in the `BrokerCertificateSource` of atunnel, which makes the key and
  sends only the CSR (`internal/atunnel/egress/credential.go`). ateworker-microvm runs atunnel in its own
  process on the host side (`cmd/ateworker-microvm/main.go`). This is confirmed in the source only. A spec
  confirms it before the code of #1386 depends on it.
- **Corrections of 2026-11-26.** Lane c1386 read the Substrate source and corrected items 2, 4, 5, 6 and 7,
  the threat model and these open points. Main approved the corrections (issue #1386).
- **Corrections of 2026-11-27.** The lab test and maintainer decision D4 (PR #1480; the maintainer's research store,
  `2026-11-27-substrate-verified-reference.md`, section 1.3, C7, C8, N6, I5) corrected item 3: an `HttpsRule`
  also replaces the header, so "clear-text `HttpRule` only" was false, and the driver now enforces the one rule in
  code. They confirmed item 6: the atenet-egress token cache is keyed by actor name
  (`cmd/atenet/internal/router/egress/actorjwt/actorjwt.go`), and a new actor with a reused name got a token with
  the UID of a deleted actor for up to a third of the lifetime, also after its atespace was made again. So a name
  is never used again, and the gateway keys an actor by atespace, name and UID together, never by UID alone. The
  JWT is ES256 only in the lab, with `exp - iat` = 900 s, so the gateway now accepts ES256 only (main's
  decision). The guest cannot steer the identity with fake XFCC or `Ate-Target-Actor` headers. The lane review added
  two rules: the State carries no actor of a sandbox that is deleted or being deleted (item 5), and the
  reconciler recreates an actor that runs under its sandbox name (made before this amendment), so that the
  new actor gets its own name and its UID.

### Prior art

The research of 2026-11-26 (the maintainer's research store, `2026-11-26-identity-outside-guest.md`).

- **Agent Substrate v1.0.1** (commit `20778a6dbb01f13024642851bb592bf67c9fec05`): the per-actor
  certificate of atunnel, and `HttpRuleEffects.replace_headers` with `actor_jwt` in
  `api/ateapi/v1/ateapi.proto` and `cmd/atenet/internal/router/egress`. We use it as it is. Section 1.2 took
  its 300 to 3600 second bounds from this feature.
- **The common pattern.** Products that keep tokens out of the sandbox find the sandbox from the transport,
  not from a header that the guest sets: Cloudflare (the container ID), Modal (a proxy for each sandbox),
  Substrate (a certificate for each actor) and Fly.io (the signed `Fly-Src` header, with a 10 second window).
  We borrow this pattern.
- **Fly.io Tokenizer** (ADR 0097 already cites it): a hop outside the workload replaces a placeholder. We
  borrow the placeholder.
- **Kubernetes projected ServiceAccount tokens and SPIFFE JWT-SVIDs:** short-lived, audience-bound JWTs that
  a relying party verifies with OIDC discovery. We borrow the verification. We differ: the token never enters
  the workload.
- **What we do not copy.** E2B fails open when injection fails; Koinon fails closed. Modal blocks memory
  snapshots when injection is on; here the token is not in the guest, so snapshots stay possible.
- **What does not fit.** A source IP is not an identity for a microVM behind a shared pod network. SPIRE with
  a Kata vsock proxy needs `hostPID` and `CAP_SYS_ADMIN`, gives only a pod-level identity and puts the SPIFFE
  socket in the guest. Istio ambient gives a pod identity, not an actor identity.

## Amendment (2026-11-26, issue #1446): agentd has a router identity, and only agentd

Maintainer decision of 2026-11-26.

- **Fact.** The atenet router of Substrate v1.0.1 serves TLS only. Its Service port 443 goes to container port 8443.
  It has no plain port and no port 80. A client must show a certificate whose SPIFFE ID is on the router flag
  `--ingress-allowed-spiffe-ids`. Any allowed ID can reach any actor
  (`cmd/atenet/internal/router/README.md`, "Ingress client authentication and authorization"). The driver used
  `atenet-router.<ns>.svc:80` in plain text, so every Exec failed at the connection.
- **Decision.** koinon-agentd is the ONLY Koinon component with a router identity.
  - The chart gives the agentd pod a `podCertificate` projected volume (signer `podidentity.podcert.ate.dev/identity`,
    ECDSA P-256) and a `clusterTrustBundle` volume for the CA of the router's serving certificate (signer
    `servicedns.podcert.ate.dev/identity`). No other Koinon pod gets either volume. The chart check asserts this.
  - The driver dials the router on TLS 443, checks the serving certificate against that CA, and presents the pod
    certificate. It reads both files again when they change, so a kubelet rotation needs no restart.
  - The installer and the preflight put `spiffe://cluster.local/ns/<koinon namespace>/sa/<agentd service account>` on
    the router allow-list, next to the atenet client. The preflight also checks that the router Service serves the
    port of `routerAddress`.
  - The control ingress for Koinon's namespace in the control-plane namespace opens only TCP 443 and 8443
    (`workerNetworkPolicy.koinonIngressPorts`), not every port of every control-plane pod.
- **Why only agentd.** The router allow-list has no per-actor scope: an allowed ID reaches every actor of every org.
  The reconciler, the API and the provisioner call only the control API and need no router access. One identity
  keeps this wide grant small. A second consumer needs a new decision, not a list entry.
- **Cost.** A compromised agentd can reach every actor through the router. Before this amendment it could not reach
  any actor, because no run worked. Substrate's allow-list cannot narrow this. The narrowing belongs upstream or in a
  later Koinon routing layer.
- **Failure behaviour.** When the router connection fails before a stream opens, agentd fails the run at once with
  the dial error. It does not treat the failure as a lost harness and does not take over the lease: a retry cannot
  help until an operator fixes the install.

## Amendment (2026-11-27, issue #1489): the actor JWT on every gateway listener

Maintainer decisions of 2026-11-27 (issue #1489, comments 7220445863 and 7221191962). This amendment changes items 1,
3, 5 and 8 of the #1386 amendment for the Substrate driver. The OpenShell driver does not change until #1094.

- **Fact.** The #1386 amendment put `actor_jwt` on the egress listener only. The model and MCP listeners read the
  agent's gateway key from the guest (`KOINON_MODEL_KEY`). That is a credential inside the guest, against section 1.1.
- **One rule for each listener.** The driver writes one `HttpRule` for each gateway host and port: the model, the MCP
  and (with a code-host grant) the egress listener. Each rule has `replace_headers` with `actor_jwt`,
  `expiration_seconds: 900`, and exactly one audience: `koinon-gateway-model`, `koinon-gateway-mcp` or
  `koinon-gateway-egress`. A token for one listener is refused on every other listener. Two listeners on one host and
  port are refused (`ValidateGatewayRoutes`, `internal/execution/substrate/egresspolicy.go`). The rule of item 3 stays:
  only these `HttpRule`s carry `replace_headers`, and the driver writes no `HttpsRule`.
- **The header.** The model and MCP listeners read the JWT from `Authorization`, with the prefix `Bearer `, where the
  harness client sends its key. The egress listener keeps `Koinon-Actor-Token`: the git and HTTP clients of the guest
  set `Authorization` for the upstream host, and the OpenShell Ed25519 gate uses the same header until #1094. This is
  a deviation from decision 1 of comment 7220445863. Main can change it when #1094 removes the Ed25519 path.
- **Section 1.2 and 1.3 on Substrate.** The guest gets the fixed public placeholder `koinon-actor-placeholder` in
  `KOINON_MODEL_KEY` (`actortoken.Placeholder`). It is the same in every sandbox and it is not a secret. atenet-egress
  writes over it. The guest holds no gateway key.
- **The verifier on every listener.** koinon-gateway has one Substrate verifier, with one key cache, and a verifier
  for each audience (`WithAudience`). On each listener:
  - It accepts `ES256` only.
  - It refuses the placeholder, an empty bearer, a token it cannot verify and no token with 401 and the do-not-retry
    marker. There is no fallback to a gateway key for these values. A real gateway key still works on the model
    listener for koinon-api.
  - It checks `iss`, `exp`, `iat`, `nbf` and `jti`, a lifetime of 300 to 3600 seconds, and an `aud` that is exactly
    the audience of the listener.
  - The actor must be in the actor table of the State and its UID must be equal to the recorded UID (item 5). An
    unknown actor gets 403.
  - When the issuer keys are not available, the listener answers 503 without the do-not-retry marker.
  - The gateway never sends the JWT upstream.
- **Key policy.** The verifier uses fetched keys for at most 10 minutes (`KeyMaxAge`). When the issuer is down for more
  than 10 minutes, every call on every listener gets 503 until the issuer is back. This fails closed. We accept it: an
  older key set could still accept a key that Substrate removed.
- **Model calls on Substrate go to koinon-gateway.** Only koinon-gateway checks the actor JWT. So the chart refuses
  `execution.driver=substrate` without `gateway.model.driver=koinon` (the default with Substrate, ADR 0097 section 16)
  and `gateway.mcp.driver=koinon`, with a message that names the value. The koinon model driver needs `gateway.mode=standalone`, so Substrate needs it too. The chart
  sets the agentd gateway URL and the reconciler `gateway.modelUrl` to the model listener. agentgateway keeps its
  own policy for the OpenShell driver.
- **Item 8 on every listener.** The chart NetworkPolicy admits the model, MCP and egress listener ports from the
  Substrate namespace only for the atenet-egress pods (a `namespaceSelector` and a `podSelector`). It does not admit
  the whole namespace. An empty `workerNetworkPolicy.atenetEgressSelector` fails the render. The model gateway also gets the egress rule to
  the `ate-idp-server` pod port (item 7). A chart check asserts each rule.
- **The run waits for its actor.** koinon-reconciler writes a `gateway_agent_forge` row for every active agent on
  Substrate, also without a forge grant. The row waits for the later of the credential's revision and the first State
  revision that holds the actor's UID. koinon-agentd starts the harness only when every live gateway copy acked that
  revision (`waitForge`). The wait goes through the database and the State stream. No kubelet ConfigMap or volume sync
  is on its path. Kind has no Substrate agent path, so the added start time is not measured there. Main measures it
  in the lab.
- **Restore.** A restore can keep the old token. The token cache is in atenet-egress, keyed by actor name, not in the
  guest. When the restored actor keeps its name and its UID, its cached token stays valid for it. When it gets a new
  actor, item 6 gives it a new name, so no old token applies. The guest and its
  snapshot hold no token (section 5.1).
- **The #1386 rule for this amendment.** No listener trusts a value that the guest sets for the identity. Each
  listener trusts only the JWT that atenet-egress adds, checked against Koinon's own record.
- **Lab risks.** These are not proven in this amendment. Main proves them in the lab.
  - A reused actor name gets a token with the old UID for up to about 9 minutes (two thirds of 900 seconds, less the
    time since the mint). Item 6 makes each name new, and the UID check refuses the old token, so this fails closed.
  - A deleted actor's token passes the signature check for up to about 10 minutes. The actor table drops the actor at
    the next State update, and then the UID check refuses the token.

## Amendment (2026-11-26, issue #1387): a non-root payload on the gVisor tiers

Accepted (2026-11-26). The maintainer decided on 2026-11-26 to replace the root payload of 2026-11-18 with a
required non-root payload in v0.1, on the gVisor tiers (`hardened` and `dev`). The `vm` tier follows in
[#1462](https://github.com/stateracloud/koinon/issues/1462).

**What it supersedes.** Section 4, item 5 ("Until then the payload runs as root with a reduced capability set")
and the open question on `CAP_SETUID` and `CAP_SETGID` ([#1087](https://github.com/stateracloud/koinon/issues/1087))
no longer apply to the gVisor tiers. They stay for the `vm` tier until #1462 closes.

### Decision

1. **The template gives the launcher what the drop needs, and nothing more.** On the gVisor tiers the actor
   template of the Substrate driver drops every capability of the agent container (`drop: [ALL]`) and adds only
   `SETUID`, `SETGID`, `SETPCAP` and `KILL`. The launcher uses `SETUID` and `SETGID` for the drop and `SETPCAP` to
   shrink the bounding set before it. The guest front uses `KILL` to signal the processes that it starts. The set
   is part of the template hash, so a sandbox of an older template gets a new template.
2. **The payload runs as uid and gid 10001.** The launcher policy of the gVisor tiers (`policy-gvisor.json`) and
   the built-in default policy set `user.mode` to `require` with uid and gid 10001. The capability allowlist of
   the payload stays empty: after the drop the payload has no capability in any set, and `no_new_privs` is set.
3. **No drop, no start.** When the drop fails, the launcher exits 70 and does not start the payload. This is the
   fail-closed rule of section 4.3, now for the user drop on the gVisor tiers too. A spec in
   `internal/guestlaunch` proves it: the gVisor policy in a guest without `CAP_SETUID` exits 70 and starts no
   payload.
4. **The `vm` tier keeps its current set.** Its template keeps the Substrate default set and `policy-vm.json`
   keeps `user.mode: best_effort`. Nobody has proved the drop or the front listener on the microVM guest with the
   new set. #1462 does that and then changes the `vm` tier to `require`.

This amendment weakens no control of this ADR or the tiers of [0075](0075-isolation-tiers.md): it removes the
root payload from two tiers and leaves the third as it was.

### Proof

- **Lab, 2026-11-26 01:50Z (koinon-lab, gVisor worker).** With the template set above and `user.mode=require`,
  the launcher drops the payload to uid 10001. `CapEff` is 0. `NoNewPrivs` is 1. `setuid(0)` returns `EPERM`, also
  in child processes.
- **The guest front on port 80.** The front keeps its listener on port 80 without `NET_BIND_SERVICE`. The gVisor
  netstack has no privileged-port check (gVisor commit `84708bc9b0a3ceeee1357c3621ae25f9d9ba3c5f`, read on
  2026-11-26: `pkg/sentry/socket/netstack` and `pkg/tcpip/stack` do not test the capability or
  `ip_unprivileged_port_start`). The lab run above started the front with this set. This is true only when runsc
  uses its own netstack (`--network=sandbox`, the runsc default). With `--network=host` the host kernel checks the
  port, and this set is not sufficient.
- **Upload with a non-root payload** ([#1478](https://github.com/stateracloud/koinon/issues/1478)). The front stays
  root but has no `CAP_DAC_OVERRIDE`. Upload under the Substrate driver failed on main before #1478. Lab,
  2026-11-26: `WriteFile` failed with `PermissionDenied: path "/tmp/koinon-upload-..." is outside sandbox root
  directory "/sandbox"`, and the root front wrote the archive with mode 0600, so `tar` as uid 10001 could not read
  it. The fix of #1478: the driver runs `mkdir -p`, then `tar -x -f - -C DIR` as the payload user (behind the
  launcher), and sends the archive on the stdin of `tar`. The guest front starts with the file service off
  (`EnableFileSystem: false`). The root front never writes into a path that the payload can write. A staging
  directory that the payload owns would let the payload swap a symlink while the root front writes, so there is
  none. The command reads every byte of stdin, so the exec stdin cap (about 4 MiB that the command does not read)
  does not apply. The home restore in `internal/agentd/checkpoint.go` uses the same Upload path. The `-log-dir` of
  the front must not be a directory that the payload can write.
- **Lab proof run, 2026-11-26 21:00Z (koinon-lab, gVisor worker).** Head `41a2400b5`, through the real Substrate
  driver and the real front image. The run showed:
  - runsc network mode `--network=sandbox`;
  - Exec in `/sandbox` as uid 10001 with `CapEff` 0;
  - `/sandbox` at run time is `0:0` mode 777, so the front enters it without `CAP_DAC_READ_SEARCH`;
  - the escape suite: 14 of 18 specs pass (13 of 18 on main). The 4 network failures are the same on main.
    Their cause in the lab setup is not confirmed.
- **Not proved here.** The lab run above used the staging design that #1478 replaced, so it does not cover the
  current Upload and checkpoint restore code. A lab run of the tar stream (uid 10001 owner on gVisor, a 1 GiB
  archive, a suspend in the middle of an upload) is still open.
  The maintainer has not yet accepted the 4 network failures in writing, so this change does not close #1387.
  It uses `Refs #1387`.

### Prior art

- **Kubernetes `runAsNonRoot`.** The kubelet refuses to start a container whose user resolves to uid 0, and it
  refuses a non-numeric image user that it cannot check. We take the same rule: a payload that would run as root
  does not start. We differ in where the check runs: the launcher in the guest does the drop and the check,
  because Substrate has no field for the payload user.
- **Docker and the Kubernetes "restricted" Pod Security Standard.** Both use `drop: [ALL]` and then add back a
  small list. The restricted profile allows only `NET_BIND_SERVICE` to be added. We add `SETUID`, `SETGID` and
  `SETPCAP` because the drop happens inside the container, after the start, and not in the runtime.
- **gVisor (runsc).** runsc runs the container process with the user and capabilities of the OCI spec, and the
  sentry checks capabilities against its own user namespace. A drop inside the sandbox through `setresuid` is the
  normal Linux behaviour in the sentry.

## Amendment (2026-11-27, issue #1492): golden snapshots off by default, admin exception

The maintainer decided on 2026-11-27: golden snapshots are an option that is off by default in every tier.

- **Text fix, section 5 item 2.** Substrate resumes a suspended actor from its own snapshot. It does not make a
  second actor from it. Single use of a snapshot is a Koinon rule that the snapshot record of the v0.2 design
  enforces. Substrate does not enforce it.
- **Default.** The Koinon template sets `GOLDEN_SNAPSHOT_MODE_DISABLED`. Golden mode clones the memory of one
  sandbox into many, which section 5 item 2 forbids.
- **Exception.** An admin can turn golden mode on with `execution.substrate.goldenSnapshots`. The admin then accepts
  that sandboxes of one template start from the same memory and the same random state.
- **Evidence.** In the lab, golden mode was not faster: cold start to the first Exec was p50 1.03 s (off) and 1.05 s
  (on) on gVisor, and 2.65 s (off) and 3.50 s (on) on the vm tier. A golden build takes 36-38 s. The numbers are
  in [the Substrate install document](../dev/substrate-install.md#golden-snapshots).

## Amendment (2026-11-27, issue #1496): a root payload on the `vm` tier in v0.1

Accepted (2026-11-27). The maintainer decided on 2026-11-27 that the payload may run as root in the microVM
guest of the `vm` tier in v0.1. A non-root option can come later ([#1462](https://github.com/stateracloud/koinon/issues/1462)).
The amendment of 2026-11-26 (#1387) stays: the gVisor tiers require a non-root payload.

**What it changes.** Section 4, item 5 ("the payload runs as root with a reduced capability set and the same
controls") is the accepted state of the `vm` tier in v0.1, and not an open gap. This amendment names the reduced set
and adds one control.

### Decision

1. **The VM is the host boundary.** The guest has its own kernel under hardware virtualisation (ADR 0075). Egress is
   enforced outside the guest (section 2), and the guest holds no secret (amendment of 2026-11-26, #1386). Root in the
   guest gives no route to the host, to the credentials or to the network.
2. **The payload holds no capability.** `policy-vm.json` keeps an empty allowlist. The launcher removes every
   capability from the permitted, effective, inheritable and ambient sets, and from the bounding set when the guest
   gives it `CAP_SETPCAP`. `no_new_privs` is set, so no later `execve` gives a capability back. The launcher reads the
   sets back after `capset` and does not start the payload when a capability outside the allowlist remains.
3. **A policy can give a root payload only two capabilities.** A policy whose user drop is not `require` fails
   validation when its allowlist names any capability except `CAP_NET_BIND_SERVICE` and `CAP_AUDIT_WRITE`. The rule
   is an allowlist, so `CAP_MKNOD`, `CAP_NET_ADMIN`, `CAP_BPF`, `CAP_PERFMON`, `CAP_SETPCAP`, `CAP_SYS_CHROOT` and
   every capability that a later kernel adds are refused by default.
4. **The payload cannot signal the Koinon processes of the guest.** A uid 0 process needs no capability to signal
   another uid 0 process, and the guest front runs as root. So `policy-vm.json` adds the Landlock signal scope
   (`landlock.scoped: ["signal"]`, ABI 6): no signal reaches a process outside the payload's Landlock domain. The
   mode is `require`, so a guest kernel below ABI 6 does not start the payload. Landlock also refuses a `ptrace` of a
   process outside the domain, and the seccomp filter denies `ptrace`, `process_vm_readv`, `process_vm_writev`,
   `pidfd_getfd`, `mount`, the module calls and `reboot` in every tier.
5. **The home owner differs between tiers.** The home files are owned by uid 0 in the Substrate guest of v0.1 (the
   guest has no `CAP_SETUID`, [#1462](https://github.com/stateracloud/koinon/issues/1462)). They are owned by uid
   10001 when the drop succeeds, and on the gVisor tiers.
6. **The job log directory is out of the payload's reach** (issue
   [#1503](https://github.com/stateracloud/koinon/issues/1503)). The guest front runs as root and writes the job logs.
   See "The root front writes only where the payload cannot write" below.

The harnesses need none of the removed capabilities: OpenCode, git and package installs into the home (npm, pip, go)
run without one. The table in `docs/dev/guest-launcher.md` ("Root on the `vm` tier") gives each capability, the
control that removes it and the harness need.

This amendment weakens no control of this ADR or the tiers of [0075](0075-isolation-tiers.md): the `vm` tier keeps
its microVM, its default-deny egress, Landlock in `require` mode and seccomp, and it gets two more controls: the Landlock signal scope and the job log directory rule (consequences).

### Consequences

- A process of one exec cannot signal a process of another exec on the `vm` tier: each launcher run makes its own
  Landlock domain. The payload and its children share one domain. An agent that starts a server in one exec and stops
  it from another exec must keep both in one exec (or one process tree).
- The guest kernel of the `vm` tier must offer Landlock ABI 6 (Linux 6.12). Phase 0 measured ABI 7 in the Substrate
  guest.
- **The root front writes only where the payload cannot write** (issue #1503). The upstream process tracker opens
  each job log with `os.OpenFile` (`O_CREATE` and `O_TRUNC`) and makes the directory with `MkdirAll`. Both follow
  symlinks. The upstream default directory, `/var/log/ate-jobs`, is under `/var/log`, and the payload can write
  there (`read_write` in `policy-vm.json`). A root payload could plant a symlink with the name of a job log, and the
  front would then truncate or create the target file. The rule is: every path that the root front opens for writing
  is under a directory that the payload cannot write. The front uses `/run/koinon/ate-jobs`. On the `vm` tier, Landlock
  (mode `require`) is the control: `/run` is in `read_exec`, so Landlock refuses create, symlink, rename and remove
  beneath it. On the gVisor tiers, Landlock is best effort. There the control is the non-root payload (uid 10001) and a
  directory that root owns, with mode 0750, under a `/run` that root owns. The front makes the directory before the
  payload exists. It walks the path from `/` with `openat` and `O_NOFOLLOW`, and makes a missing part with `mkdirat`
  on the open parent. So it never follows a symlink and has no gap between a check and an open. It refuses a part
  that is not a directory, that another user than root or the front owns, or that group or others can write (a parent
  with the sticky bit excepted). For the last part it also refuses any access for others and any group write (mode
  0750 or stricter), so a directory made earlier with mode 0755 is refused. It also refuses a group other than the front's group. A new path that the front writes needs a
  policy spec that no `read_write` entry covers it.
- **The payload cannot read the job logs** (issue #1516). `/run` is in `read_exec`, so a root payload on the `vm` tier
  could read the job logs in `/run/koinon/ate-jobs`. We close this with Landlock and not with a note. Landlock has no
  deny rule, so the policy has a `hidden` list (both image policies hold `/run/koinon`). The launcher replaces a
  `read_exec` entry that holds a hidden path with its entries, and goes down to the hidden path. It leaves symlinks
  out, because Landlock would grant their targets. The launcher reads the directories when it starts the payload,
  after the front has made `/run/koinon`. A file that a service makes later in `/run` is not readable for the payload.
  `/run` itself is not granted any more, so a tool that lists `/run` fails, and a symlink in `/run` that points
  outside the granted paths does not work. The payload makes no file in `/run` itself (it cannot write there). The
  front is not under Landlock, so it is not affected. The policy refuses a `hidden` path that is inside a `read_write`, `devices` or `list_dirs` entry, because
  Landlock cannot take back a right of a parent. A read error of a directory stops the start. Reads under `/var/log`
  stay open on purpose: `/var/log` is `read_write` for the payload, and the front writes no log there.

### Proof

- **Docker, `internal/guestlaunch` (container specs).** `policy-vm.json` in the capability shape of the Substrate
  guest (root with only `KILL`, `NET_BIND_SERVICE` and `AUDIT_WRITE`): the payload runs as uid 0 with `CapPrm`,
  `CapEff` and `CapAmb` 0 and `NoNewPrivs` 1. Signal 0 to an outside root process (`kill` and
  `pidfd_send_signal`), `ptrace` of that process, `mount`, `finit_module`, `reboot` and a raw socket are refused. A
  signal to its own child works. No committed spec clears the signal scope. The review ran that case by hand and saw both signal attempts open.
- **Docker, issue #1503.** In the container specs, with the image policies, a write of a file and a symlink under `/run`
  are refused, and the control run without the launcher shows both open. The policy specs check that no `read_write`
  entry of either image policy covers `/run/koinon/ate-jobs`. The unit specs of `guestfront.PrepareLogDir` refuse a
  symlink as the directory or in its path, a directory that others can write or read (0755), a file, a relative path
  and `/`. The policy specs also check `hidden` in both image policies, and `ReadExecVisible` on a temporary tree.
  The container probe lists `/run/koinon/ate-jobs` (a directory that anyone can read in the test image) and expects
  `EACCES` with both image policies; the control run without the launcher shows it open.
- **Not proved here.** The run in a microVM guest needs KVM. The lab runs it on a `vm` worker, with a normal agent
  run (clone, build, push).

### Prior art

- **Landlock scopes (Linux 6.12, ABI 6).** `LANDLOCK_SCOPE_SIGNAL` limits signals to the sandbox's own domain and its
  children. It is the same rule as the Landlock `ptrace` restriction since ABI 1. We use it because it covers every
  signal path (`kill`, `tgkill`, `pidfd_send_signal`, `SIGIO`) in one place. A seccomp filter on the pid argument
  cannot cover `kill(-1)`, process groups or a pidfd.
- **Kata Containers.** The kata-agent runs as root in the guest, next to the container processes, and Kata treats the
  VM as the boundary to the host. We keep the VM as the boundary too, and we add the capability drop and the signal
  scope so that the payload cannot stop or trace the guest front.
- **Firecracker jailer and the Kata agent.** Both make the directories of the privileged process before the
  untrusted workload starts, and give the workload no write access to them. We do the same for the job log
  directory, and we move it from `/var/log` to `/run`, which the payload cannot write.
- **Docker's default capability set.** Docker keeps `KILL`, `NET_RAW` and `SETUID` for a root container. We remove
  every capability, because the harnesses need none.

## Amendment (2026-11-27, issue #1515): on Substrate the deny comes after the CONNECT, not at the TCP handshake

Section 2 item 1 says that the egress layer denies all traffic by default. The sentence "L4 denies all traffic" does
not mean that a TCP handshake fails on Substrate. This amendment says where the deny happens. It changes no control.

- **What happens.** Each actor has its own network namespace. The nftables rules there send every TCP connect to a
  local atunnel listener. The listener accepts the connection first. Then it opens a CONNECT tunnel to atenet-egress.
  atenet-egress decides on the Host or the SNI, and it never dials the IP address that the guest chose. So on
  Substrate the TCP handshake always completes. The CONNECT is accepted. The deny comes after atenet-egress reads the Host or
  the SNI: the tunnel closes, or the gateway answers 403 `address_denied`, and no byte comes back from the destination.
- **What follows for a test.** A `connect()` proves nothing about a deny or an allow on Substrate. A deny spec sends
  bytes (a TLS ClientHello or an HTTP request) and expects no byte from the destination. It runs a positive control in
  the same probe: a request to a gateway route that must get an answer. The specs of issue #1515 do this.
- **What stays.** Default-deny egress in every tier, the enforced L7 rules, the isolation tiers of ADR 0075 and the
  positive control are unchanged. The address `169.254.17.2` is the same constant in every actor. Each actor has it in
  its own network namespace. It is not a collision, and it is not a route to another actor.
- **Open until the lab run.** The 4 specs of issue #1515 fail on a lab run of 2026-11-27 because they probed with
  `connect()`. This amendment does not claim that the product holds on these 4 targets. A lab run of the new specs,
  with the positive control passing, is the evidence. Until then, "no product hole" is a hypothesis.
- **Docs.** [escape-substrate.md](../dev/escape-substrate.md#a-connect-proves-nothing-on-substrate) gives the
  denial results that a spec accepts.

## Amendment (2026-11-28, issue #1482): a spec change on a suspended actor repoints it, the home stays

Section 5.4 says that a changed sandbox spec deletes the sandbox and restores the home from kopia. On Substrate
that is now the fallback. The first choice keeps the home where it is.

- **Repoint.** A spec change on the same tier gives a new template. The driver suspends the actor with synchronous
  durability, calls UpdateActor with the new template, and resumes the actor. The actor name, the home and the
  snapshot stay. The driver then takes one more synchronous suspend, so a revert does not go back to a snapshot of
  the old template. The seal is best effort. When it or the resume fails, the driver keeps a mark in memory and takes
  the seal at the next resume. A restart of the driver loses the mark. A repoint resumes an idle sandbox too. The
  recreate cap limits how many repoints one pass makes.
- **Invariants.** A repoint is legal only when these stay equal: the container names, the volumes, the volume
  mounts of every container, the snapshot storage location, the isolation tier and the runtime class. On gVisor the home checkpoint
  is keyed by the container name. A rename leaves the home empty, and Substrate gives no error. Golden tests in
  `internal/execution/substrate` fail when one of these changes in a rendered template.
- **Fallback.** A tier change, a Substrate refusal (FailedPrecondition or InvalidArgument) or a lost old template
  gives `ErrRepointRefused`. The reconciler then deletes the actor and restores the home from kopia, as before.
  A failed resume after the repoint gives `ErrRepointResume`. The actor names the new template and holds its home,
  so the reconciler does not delete it.
- **Templates.** The driver never deletes a template that an actor names. `DeleteUnusedTemplate` checks
  ListActors first. No production path calls it before v0.2.
- **What stays.** Default-deny egress, the isolation tiers of ADR 0075 and the snapshot rules of section 5 are
  unchanged. The metric `koinon_reconciler_sandbox_repoints_total{result}` counts repoints by result.
