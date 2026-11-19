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
6. Org isolation does not rely on the guest controls. The boundary between two orgs is the VM or
   sandbox boundary plus the actor identity and the rules of sections 1 and 2.

### 5. Snapshots (D5)

A suspend or checkpoint can keep disk state (volumes, rootfs) and memory state. Memory state holds
whatever the agent had in RAM: file contents, model output and org data. It holds no credential, because
of section 1, but it is org data of the highest class. These rules apply to every fidelity and every
driver. A driver may enable the `MEMORY` fidelity only when all of them are met and tested.

1. **No credential in a snapshot.** Section 1 keeps credentials and the actor token out of the guest, so
   a snapshot cannot hold them. A restore always gets a fresh actor token. An old token is never
   restored. The 0044 exception is refused for a sandbox with snapshots on, or its path is excluded
   from the snapshot.
2. **Short lifetime.** A snapshot has an expiry. A memory snapshot restores once: a restore consumes it,
   and a second restore needs a new snapshot. This stops two clones with the same memory and the same
   random state. Tokens stay short-lived for the same reason.
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
     it false unless the values name a ServiceAccount (that one must set it). The capabilities that a
     container adds have a ceiling (`SYS_ADMIN`, `NET_ADMIN`). `privileged`, an unmasked `/proc`, `hostPort` and Unconfined seccomp or AppArmor profiles are denied.
     Ephemeral containers are denied.
   - **Control-plane namespace.** Pods come from Kubernetes workload controllers. A pod may mount a
     hostPath only when the DaemonSet controller creates it for a DaemonSet (a controller
     `ownerReference`) and it runs as the ServiceAccount `atelet`, and only for the node agent's own list: the hostPath volumes of atelet's manifest in Substrate v1.0.0
     (`/var/lib/ate`, the kubelet `plugins` and `device-plugins`, `/dev` and two GKE credential paths), which
     is not the worker list.
     Every other pod there gets no hostPath. Digests pinned and no `hostNetwork`, as for workers.
   - **Both.** `pods/exec`, `attach`, `portforward`, `pods/proxy` and `services/proxy` are denied to every
     identity. The worker pool, sandbox config and storage driver objects, and every workload kind, are
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
   traffic reaches the gateway through atenet. NetworkPolicies add up, so the per-pool policy that
   Substrate's controller writes can widen worker egress, and no admission rule guards it yet (exit condition: [#1158](https://github.com/stateracloud/koinon/issues/1158)). The control-plane
   namespace has a policy keyed on an explicit list of `app` labels (the control-plane pods), not on
   "every pod without the worker label": a rule of the second form fails open for a pod that loses its
   label. A pod with another label is not selected, so the list follows the Substrate manifests.
7. **No internet egress for atelet.** The gVisor runtime asset comes from the cluster object store, not from
   the internet. Koinon stages the pinned gVisor tarball in the object store and ships a `SandboxConfig` with
   an `s3://` source and the same SHA-256. With that, atelet needs no egress to the internet, and default-deny
   holds in the control-plane namespace too. Status: the staging is built
   ([#1157](https://github.com/stateracloud/koinon/issues/1157); `substrate-install.md`). The read by atelet from
   the cluster store, with egress denied, is not yet proven. The kind check must prove it, and must restart every
   DaemonSet in `ate-system` after the policies apply.

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
  a non-root user ([#1087](https://github.com/stateracloud/koinon/issues/1087), an upstream request).
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
