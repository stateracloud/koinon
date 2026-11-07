# 0075. Isolation tiers: vm, hardened and dev

- Status: Accepted (pending qualification on Bottlerocket)
- Date: 2026-10-28
- Amends: [0045](0045-openshell-hardening-defaults.md), [0048](0048-openshell-spike-results.md) (decision 2)
- Relates to: [0002](0002-namespace-per-org-isolation.md), [0006](0006-identity-layers.md), [0023](0023-observability-and-audit.md), [0025](0025-testing-strategy.md), [0042](0042-ask-tier-park-and-execute.md), [0076](0076-database-modes.md), [0077](0077-pinned-openshell-and-installer.md)
- Pending verification: the Bottlerocket qualification run (decision 12)

## Context

ADR 0045 makes Kata the default runtime class. The chart ships
`openshell.runtimeClassName: kata`, and the sandbox VAP admits `[kata, kata-qemu, kata-qemu-runtime-rs]`
(since the 2026-11-01 amendment, old issue 442; `charts/koinon/values.yaml:380`, `:512`). In practice Kata is the only supported path: the docs
allow `runc` only on kind ("Do not use it for real work", `prerequisites.md:101`).

procoduck's target users are platform teams on managed clusters. Most EKS organisations run Bottlerocket,
and Kata is not available there.

- **Bottlerocket ships one container runtime.** Its containerd 2.2 Kubernetes template defines only
  `containerd.runtimes.runc` (`bottlerocket-core-kit`
  `packages/containerd-2.2/containerd-config-toml_k8s_containerd_sock:65-70`). No setting adds a
  handler.
- **Bottlerocket will not add Kata to its standard variants.** The Kata request (bottlerocket#4070)
  is open. A maintainer ruled out adding Kata to existing variants: it "would warrant its own
  variant".
- **EKS Auto Mode** nodes are Bottlerocket variants too (EKS Auto Mode docs).
- **Fargate** gives each pod its own kernel, but it fails three of OpenShell's or Koinon's needs:
  - VPC CNI network policies cannot be applied to Fargate pods (EKS `cni-network-policy` docs);
  - Fargate has no dynamic persistent volumes and no EBS, so the workspace PVC cannot be
    provisioned;
  - Fargate has no DaemonSets (EKS Fargate docs).

What OpenShell 0.1.5 needs from the kernel that the workload sees:

- Landlock ABI 3 or newer, enabled;
- seccomp user notification with `SECCOMP_ADDFD_FLAG_SEND`;
- same-UID task-memory access;
- `SO_BINDTODEVICE`.

It checks all of these before the agent starts, and it fails closed when one is missing
(`docs/about/support-matrix.mdx@v0.1.5:171-190`).

Bottlerocket's kernels on the `aws-k8s` variants (Bottlerocket v1.68.0; `bottlerocket-kernel-kit`
v9.6.1):

| Variants | Kernel | Landlock ABI _(inferred from the kernel version)_ |
| --- | --- | --- |
| `aws-k8s-1.32` | 6.1 | 2 |
| `aws-k8s-1.33` to `-1.35` | 6.12 | 6 |
| `aws-k8s-1.36` and `-1.37` | 6.18 | 6 or newer |

All three kernels put `landlock` in `CONFIG_LSM` (`packages/kernel-*/config-bottlerocket:115-118`),
and none overrides it with `lsm=`. So OpenShell can run under `runc` on Bottlerocket for Kubernetes
1.33 and later, and fails closed on 1.32.

One question stays open. Bottlerocket enforces SELinux and starts unprivileged containers as
`container_t` with an MCS pair (`packages/selinux-policy/rules.cil:95-109`). Whether that policy lets
OpenShell's broker read its child's memory is unverified.

What a shared kernel gives up compared with Kata is not measured by the escape suite.

- **A kernel bug in the allowed syscalls becomes a node compromise.** OpenShell narrows that attack
  surface with Landlock, seccomp, privilege drop and no capabilities, but a shared kernel is still
  shared.
- **Credential holders are what a node compromise reaches.** The supervisor pod holds the real
  provider credentials and the per-sandbox interception CA key (ADR 0048, "Consequences";
  "The credential mounts only in the separately scheduled supervisor Pod",
  `docs/kubernetes/setup.mdx@v0.1.5:369`).
- **Cache and timing side channels across tenants** need dedicated nodes, with or without Kata.

These mitigations work on Bottlerocket:

- **User namespaces.** `hostUsers: false` is stable since Kubernetes 1.36 and needs containerd 2.0
  or newer and idmap mounts (Kubernetes user-namespaces docs). OpenShell 0.1.5 supports it through
  `drivers.kubernetes.enable_user_namespaces` (off by default) and recommends it on non-GPU clusters
  (`docs/security/best-practices.mdx@v0.1.5:66-78`).
- **SELinux.** Bottlerocket enforces it, with an MCS pair per container.
- **A dedicated sandbox node pool.** A RuntimeClass's `scheduling` field adds a node selector and
  tolerations to every pod that uses the class. Supervisor pods use the default runtime, so they stay
  off a tainted pool.

gVisor is not an option. Its syscall table lists no `landlock_*` calls, and its `seccomp` supports
"only SECCOMP_SET_MODE_FILTER … with flags set to 0 or SECCOMP_FILTER_FLAG_TSYNC", with no user
notification (gVisor amd64 compatibility page). GKE says seccomp and other LSMs "aren't supported in
sandboxed Pods". OpenShell therefore fails closed under gVisor _(inferred)_.

## Decision

### The tiers

1. **Every install has one isolation tier,** set by `isolation.tier` in the chart: `vm` (the
   default), `hardened` or `dev`. The tier applies to the whole install. Every org runs at the
   install's tier.

2. **`vm`, the golden path.** Sandboxes run in a VM per sandbox:
   - Kata (QEMU or Cloud Hypervisor) with guest seccomp enabled;
   - AKS Pod Sandboxing (`kata-vm-isolation`);
   - GKE Sandbox `microvm`.

   Needs:
   - a RuntimeClass of one of these, labelled `openshell.ai/sandbox-attachable: "true"`;
   - `sandboxPolicy.runtimeClasses` listing it;
   - enforced NetworkPolicy.

   Allows everything.

   Each managed offering (AKS, GKE) and each Kata flavour (QEMU, Cloud Hypervisor, Firecracker) is
   qualified by its own escape-suite run before the docs name it supported. Until then the docs name
   it unqualified.

3. **`hardened`, a shared kernel with every available layer.** Needs:
   - `runc` on a node kernel that passes OpenShell's qualification;
   - Kubernetes user namespaces (OpenShell `enable_user_namespaces: true`, rendered by the chart);
   - SELinux enforcing where the node OS provides it (Bottlerocket and EKS Auto Mode do);
   - enforced NetworkPolicy;
   - **a dedicated, tainted sandbox node pool**.

   The pool:
   - The chart renders a RuntimeClass (default name `koinon-hardened`, handler `runc`, labelled
     attachable) whose `scheduling` carries the pool's node selector and toleration, from
     `isolation.hardened.pool`.
   - Supervisor pods use the default runtime and do not tolerate the taint. So **a supervisor pod
     never runs on a sandbox node**, and an escape from a workload does not land next to a
     credential holder.
   - The preflight fails when the pool's selector matches no node or any of those nodes lacks the
     taint.

   Allows everything that `vm` allows, including several orgs (decision 6).

4. **`dev`, kind and CI only.** Uses `runc` on ordinary nodes, with no user namespaces and no pool.
   Setting `isolation.tier: dev` is the explicit mark: there is no default that lands here.
   - The UI shows it in red on every page.
   - The install refuses to create a second org.
   - The e2e tests and the quickstart's kind path use this tier.

5. **What stays the same in every tier.** None of these depends on the tier:
   - `enforcement: enforce` on every L7 endpoint;
   - placeholder-only secrets, with no secret in sandbox env, images, home volumes or logs;
   - no `allow_uninspected_credentials` or `tls: skip`;
   - managed workspaces;
   - `landlock.compatibility: hard_requirement`;
   - OpenShell telemetry off;
   - ask-tier calls executed by Koinon outside the sandbox ([0042](0042-ask-tier-park-and-execute.md));
   - egress only through the supervisor and agentgateway;
   - the sandbox-namespace VAP: requester identity, allowed runtime classes, no `hostNetwork`, no
     `io.katacontainers.*` annotations, a pinned supervisor image, and ephemeral containers denied.

   The VAP's allowed runtime classes are the tier's: the `vm` classes for `vm`, only the rendered
   `koinon-hardened` class for `hardened`, and `runc` for `dev`.

### Several orgs on hardened

6. **Several orgs are allowed on `hardened`, with a warning.**
   - When a second org is created on a `hardened` install, the platform admin sees: "Agents of
     different orgs share node kernels. A kernel exploit in one org's sandbox can reach another
     org's sandboxes on the same node."
   - The warning stays on the platform overview, and the org's creation is audited with it.
   - **An org can get its own pool:** `isolation.hardened.orgPools` maps an org to its own node
     selector and toleration. The chart renders one RuntimeClass per entry, and the reconciler sets
     that class on the org's sandboxes. An org with its own pool is shown without the shared-kernel
     warning. This is the one per-org runtime class Koinon supports, and only within `hardened`.

### Checks

7. **The preflight checks the tier.**
   - **All tiers:** a **Landlock and seccomp probe**. One short-lived pod under the tier's
     RuntimeClass reports the Landlock ABI (`landlock_create_ruleset` with
     `LANDLOCK_CREATE_RULESET_VERSION`) and whether a seccomp user-notification filter with
     `SECCOMP_ADDFD_FLAG_SEND` installs. ABI below 3, or no notification, fails. This catches
     Bottlerocket on Kubernetes 1.32 and runner kernels without LSMs (old issue 159) at install time
     instead of at the first sandbox.
   - **`vm`:**
     - the existing guest-seccomp probe (ADR 0048 decision 2) keeps its warning;
     - a new hypervisor check fails when the probe pod sees the host kernel (the same test as the
       escape spec).
   - **`hardened`:**
     - the user-namespace check (the probe runs with `hostUsers: false` and reports its UID map);
     - the pool check (decision 3);
     - SELinux mode reported, without failing where the OS has none.
   - **`vm` and `hardened`:** a missing NetworkPolicy engine **fails** the install instead of
     warning, because the OpenShell fence depends on it (`docs/how-it-works/sandboxes/runtimes.mdx@v0.1.5:281-283`).
     `dev` keeps the warning.

### How the tier is shown

8. **Visible everywhere it matters:**
   - **UI badge.** The badge in the header and on the platform overview reads "VM isolation",
     "Hardened (shared kernel)" or "Dev (not isolated)". It is red for `dev`, amber for `hardened`
     with shared pools, and neutral otherwise.
   - **Agent page.** Shows the tier and, on `hardened`, whether the agent's org has its own pool.
   - **Audit.** Every `run.started` and `sandbox.created` event carries the field `isolation_tier`,
     plus `isolation_pool` on `hardened`.
   - **Metric.** `koinon_install_isolation_tier{tier}`, a fixed label set
     ([0023](0023-observability-and-audit.md)).
   - **Refusals.** Every refusal that the tier causes names the tier and the setting that would lift
     it. Example: "this install's isolation tier is dev, which allows one org; see
     isolation.tier".

### The escape suite

9. **"Not applicable" is a result of its own.** The suite reports each spec as passed, failed,
   skipped or **not applicable**. The spec "runs in a Kata guest, not on the host kernel"
   (`test/escape/sandbox_test.go:433-442`) is not applicable on `hardened` and `dev`, and it is
   never reported as passed there. Its report names the tier. A `hardened` qualification adds
   specs:
   - the sandbox runs with a non-identity UID map;
   - no supervisor pod shares a node with a workload pod;
   - the workload lands only on the pool.

10. **Release gates by tier.**
    - `vm` is qualified on the home lab's Kata, as today (ADR 0045).
    - `hardened` is qualified by the run in decision 12.
    - A release names the tiers it qualified.

### Amendments

11. **ADR 0045 is amended:**
    - Kata is the golden path, not the only one.
    - `default_runtime_class_name` follows the tier.
    - Decision 6's allowed runtime classes are the tier's.
    - `enforcement: enforce` and every other 0045 default stay in every tier.
    - The Kata runtime class stays required for the `vm` tier.

    AGENTS.md's hard rule names this ADR.

### Qualification plan

12. **Bottlerocket qualification, on the home lab.**
    - **Cluster:** one Bottlerocket `aws-k8s-1.33` or later VM (kernel 6.12 or newer) joined to a
      Kubernetes 1.33 or later cluster, with a tainted sandbox pool of that node and enforced
      NetworkPolicy.
    - **Run:**
      - the preflight probes;
      - SELinux against OpenShell's broker (task-memory access under `container_t` with an MCS
        pair, with user namespaces on);
      - the full escape suite;
      - the quickstart.
    - **Pass:** every spec passed except the Kata spec, which reports not applicable.
    - **On a failure:** an SELinux denial is recorded with its AVC message and raised with
      Bottlerocket or OpenShell. It is never fixed by relaxing the policy.

    The platform-team request is drafted after the rc.6 re-run (procoduck, 2026-10-28). The status stays
    "pending qualification" until the run passes.

### Amendment 2026-10-30: no wildcard egress hosts

13. **The `vm` and `hardened` tiers refuse wildcard egress hosts.**
    - OpenShell documents that a wildcard host rule (`*.example.com`, `**`, any glob) reopens a
      DNS-label channel: a sandbox can resolve `<secret>.attacker.example` and the lookup leaks the
      data without a connection (`network-rules.mdx@v0.1.5:634-656`, old issue 266).
    - **Where it is refused.** Every host that reaches a sandbox policy is an exact name today:
      connector hosts come from a base URL that must be a bare `https` host
      (`connectors.Locate`) and from `connector.schema.json`, whose `networkRule.host` now has the
      same pattern as `host.host`; the gateway and the npm registry are constants. The checks:
      1. the API refuses a pattern base URL, provider URL or MCP server URL with a message that
         names the host and says to write it exactly (`catalog.URLPolicy`, `connectors.Locate`);
      2. koinon-api and koinon-reconciler refuse to start with a pattern in
         `egress.allowedInternalHosts` (`catalog.ValidateAllowedHosts`);
      3. the OpenShell backend refuses to render a policy or provider profile with a pattern host
         (`execution.CheckEgressHost`), as defence in depth.
    - **Today the refusal is unconditional.** The tier setting is not wired into the Go services
      yet, and no feature needs a wildcard. When a tier needs one, only `dev` may admit it, by an
      explicit setting that names the tier; the refusal message then names the tier and the setting
      (decision 8, "Refusals").

## Consequences

- **Koinon runs on the clusters platform teams have.** Bottlerocket, EKS Auto Mode and ordinary
  node groups are supported without Kata, at a tier that is stated rather than hidden.
- **Isolation is visible.** Admins, auditors and receivers of audit events can see the tier of every
  run.
- **The hardened tier costs nodes.** It needs a dedicated, tainted pool, and per-org pools cost more.
  It also needs user-namespace support: Kubernetes 1.33 or later, containerd 2.0 or later, kernel
  6.3 or later.
- **Release work grows.** There are two qualification paths per release, and a tier-aware VAP,
  preflight and reconciler.
- **The shared-kernel risk stays.** On a pool shared by several orgs, a kernel exploit crosses orgs.
  The warning says so; it does not remove the risk _(inferred)_.
- **Clusters without a NetworkPolicy engine stop installing** at `vm` and `hardened`. They worked
  with a warning before.
- **Follow-ups:**
  - the preflight probes and the "not applicable" result;
  - tier rendering in the chart (RuntimeClass, VAP, OpenShell values);
  - the reconciler's per-org RuntimeClass for `orgPools`;
  - the UI badge;
  - the audit field;
  - the qualification run and the platform-team request.

## Alternatives

- **Keep Kata required.** This keeps the strongest boundary, but it excludes most EKS organisations
  (Bottlerocket) and EKS Auto Mode, and it pushes teams to unsupported workarounds. Rejected.
- **gVisor (GKE Sandbox `gvisor`, `runsc`).** It has no Landlock and no seccomp user notification,
  so OpenShell fails closed. Supporting it would need OpenShell to relax its baseline. **A stated
  non-goal while OpenShell needs Landlock and seccomp notification.** Revisit if OpenShell gains an
  isolation backend that does not need them (`docs/extensibility/isolation-backends.mdx@v0.1.5`).
- **Fargate as a VM tier.** It has no NetworkPolicy, no dynamic volumes and no DaemonSets. Rejected.
- **A custom Bottlerocket variant with Kata.** Bottlerocket's maintainers steer Kata to custom
  variants (#4070), and the kernel builds KVM as a module. Koinon would own an operating-system
  variant. Rejected for Koinon; an organisation may build one and run `vm`.
- **One org only on hardened.** Safer by default, but it blocks teams that want several orgs and
  accept the risk. procoduck chose a warning plus optional per-org pools.
- **Per-org tiers** (some orgs `vm`, some `hardened`). Rejected for now. The tier is install-wide;
  only the pool varies per org.

## Upstream verification

- 2026-10-28, OpenShell `v0.1.5`:
  - `docs/about/support-matrix.mdx:171-190` (kernel requirements, fail closed);
  - `docs/security/best-practices.mdx:66-78` (user namespaces);
  - `docs/kubernetes/setup.mdx:369` (credentials only in the supervisor pod);
  - `docs/how-it-works/sandboxes/runtimes.mdx:279-285` (NetworkPolicy required);
  - `docs/how-it-works/gateways/configuration.mdx:760-777, 954-956` (RuntimeClass admission and
    default).
- 2026-10-28, Bottlerocket v1.68.0 variant `Cargo.toml`s (kernels by variant). `bottlerocket-kernel-kit`
  v9.6.1 `packages/kernel-{6.1,6.12,6.18}/config-bottlerocket:115-118`; `CONFIG_KVM=m` in the full
  configs. `bottlerocket-core-kit` at HEAD:
  - `packages/containerd-2.2/containerd-config-toml_k8s_containerd_sock:65-70`;
  - `packages/selinux-policy/rules.cil:95-109`.

  Issues bottlerocket#4070 (Kata, open) and #812 (Firecracker, open).
- 2026-10-28:
  - gVisor syscall compatibility (amd64);
  - GKE Sandbox concepts (`gvisor`, `microvm`);
  - AKS Pod Sandboxing (`kata-vm-isolation`, Azure Linux, nested virtualisation);
  - EKS Fargate considerations;
  - EKS Auto Mode;
  - EKS `cni-network-policy`;
  - Kubernetes user namespaces (stable in 1.36).
- Research notes: the session report `research/standalone.md`, sections 3 and "Follow-up
  2026-10-28".

## Links

- [0045](0045-openshell-hardening-defaults.md), [0048](0048-openshell-spike-results.md),
  [0077](0077-pinned-openshell-and-installer.md)
- https://gvisor.dev/docs/user_guide/compatibility/linux/amd64/
- https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods
- https://learn.microsoft.com/en-us/azure/aks/use-pod-sandboxing
- https://docs.aws.amazon.com/eks/latest/userguide/fargate.html
- https://docs.aws.amazon.com/eks/latest/userguide/automode.html
- https://kubernetes.io/docs/concepts/workloads/pods/user-namespaces/
- https://kubernetes.io/docs/concepts/containers/runtime-class/#scheduling
- https://github.com/bottlerocket-os/bottlerocket/issues/4070

## Amendment (2026-11-01): kata-qemu-runtime-rs in the vm tier (old issue 442)

Kata 4.x's kata-deploy (4.3.1) creates `kata-qemu-runtime-rs` (the Rust runtime) by default and marks
the Go-runtime `kata-qemu` deprecated. The `vm` tier's
`sandboxPolicy.runtimeClasses` now defaults to `[kata, kata-qemu, kata-qemu-runtime-rs]`, so a
cluster that uses it needs no override. The values schema only requires DNS-style names and a non-empty list, so the other
decisions here (GKE Sandbox `microvm`, the rendered `koinon-hardened` class, per-org pool classes)
stay expressible. What refuses an unknown RuntimeClass (runc, gVisor) is the policy: it admits only
the listed names, and the chart default plus the escape suite (`runc` and `gvisor` denied,
`kata-qemu-runtime-rs` admitted) cover that. Enforcement stays `enforce`
(ADR 0045). The reconciler still requests `openshell.runtimeClassName` (default `kata`), which must
be one of the admitted classes (the chart fails the render otherwise); its default is unchanged.

Note (2026-11-07): private infrastructure detail removed; maintainer decision 2026-11-07.
