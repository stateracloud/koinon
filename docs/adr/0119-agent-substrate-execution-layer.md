---
status: proposed
date: 2026-11-18
area: runtime-sandbox
tags: [sandbox, isolation, kubernetes, egress, credentials, openshell]
amends: ["0002", "0006", "0009", "0015", "0016", "0023", "0029", "0032", "0040", "0045", "0048", "0051", "0061", "0075", "0077", "0078", "0080", "0083", "0097", "0106", "0112", "0113"]
supersedes: ["0101"]
relates: ["0011", "0042", "0044", "0073", "0079", "0081", "0110", "0111"]
---
# 0119. Agent Substrate is Koinon's execution layer

- Status: Proposed (decisions D1-D11 settled by the maintainer on 2026-11-18; acceptance waits for the phase 0 result, see Verification)
- Date: 2026-11-18
- Area: runtime-sandbox
- Tags: sandbox, isolation, kubernetes, egress, credentials, openshell
- Supersedes: [0101](0101-agent-substrate-and-kagent-not-adopted.md)
- Amends: [0002](0002-namespace-per-org-isolation.md), [0006](0006-identity-layers.md), [0009](0009-agent-runtime.md), [0015](0015-agent-environment-and-browser.md), [0016](0016-harness-and-deployment.md), [0023](0023-observability-and-audit.md), [0029](0029-run-level-resume.md), [0032](0032-agent-home-checkpoints.md), [0040](0040-agentgateway-model-and-mcp-gateway.md), [0045](0045-openshell-hardening-defaults.md), [0048](0048-openshell-spike-results.md), [0051](0051-session-home-generations.md), [0061](0061-image-pull-secrets-and-registry-mirrors.md), [0075](0075-isolation-tiers.md), [0077](0077-pinned-openshell-and-installer.md), [0078](0078-sandbox-observer.md), [0080](0080-tool-set-matches-egress-and-escape-hosts.md), [0083](0083-sandbox-resources.md), [0097](0097-koinon-owned-gateway.md), [0106](0106-install-mark-owns-openshell-objects.md), [0112](0112-harness-definitions.md), [0113](0113-agent-package-access.md)
- Relates to: [0011](0011-agent-to-agent.md), [0042](0042-ask-tier-park-and-execute.md), [0044](0044-in-sandbox-credential-exception.md), [0073](0073-agent-passport.md), [0079](0079-dependency-cooldown.md), [0081](0081-run-limits.md), [0110](0110-held-copy-isolation.md), [0111](0111-ci-requirements.md)
- Issue: [#1083](https://github.com/stateracloud/koinon/issues/1083), part of [#1082](https://github.com/stateracloud/koinon/issues/1082)
- Verification: Pending the phase 0 spike result (numbers and the proofs P-A, P-B, P-C), see Verification below

## Context

ADR 0101 (2026-11-08) kept OpenShell on Agent Sandbox and did not adopt Agent Substrate. It named four
revisit conditions. This ADR records the maintainer's decision of 2026-11-18 to make Agent Substrate
(`agent-substrate/substrate`, Apache-2.0, CNCF Sandbox) Koinon's execution layer and to drop OpenShell and
Agent Sandbox after a one-minor window. Rules in ADRs and AGENTS.md record why a choice was right at the
time; they are not blockers, and this ADR changes them as a normal step.

The state of the four revisit conditions of 0101:

1. Substrate 1.0 with a compatibility promise: **met**. v1.0.0 shipped on 2026-11-05 with a stated API
   compatibility promise in its README. Before 1.0 it broke upgrades twice in two weeks (0101); kagent
   pins a fork and told its users to plan a fresh install.
2. OpenShell supports Substrate: **not met, and not needed**. Option b of the decision below drops the
   OpenShell supervisor.
3. Usable off GKE without beta APIs: **partly met**. Kubernetes 1.37 serves `certificates.k8s.io/v1`
   `PodCertificateRequest` and `ClusterTrustBundle` without beta gates. There is still no self-managed
   install profile (upstream issue 1782); the installer runs in kind mode.
4. A measured density or cold-start problem: **not met by Substrate's case**. The measured 55-76 s start
   of a new agent is the reconciler waiting for the kubelet ConfigMap sync of the gateway config (see the
   hot-pool measurement under ADR 0097); Substrate does not touch that cause. This ADR rests on other
   reasons: one control plane that Koinon does not have to patch around; a VM per sandbox with
   credentials kept outside the guest; a gVisor tier for nodes without KVM; warm pools that skip the
   scheduler; and a 1.0 runtime under the CNCF.

Where Koinon touches OpenShell and Agent Sandbox today: the `execution.Backend` seam
(`internal/execution/backend.go`) is shaped by OpenShell (provider profiles, per-binary rules, Landlock
paths); `internal/execution/openshell/` is the only implementation; the reconciler, agentd (ACP over
`Exec`), the chart (sandbox policy, network policy, observer, limit range), the escape suite and about 25
ADRs depend on it.

Properties Koinon keeps, because each is good on its own: no credentials inside the sandbox, enforced
default-deny egress in every tier, approvals executed outside the runtime (ADR 0042), and a VM boundary
as the golden path.

## Decision

D1 to D11 are the maintainer's decisions of 2026-11-18. Each has its reason.

1. **D1: drop the OpenShell supervisor; do not run it inside the microVM.** Substrate gives the VM
   boundary, the L4 egress fence and out-of-guest credential injection. Koinon's own gateway (ADR 0097)
   becomes the single L7 point for model, MCP, forge and package-registry traffic. A small Koinon guest
   launcher applies Landlock, seccomp, `no_new_privs` and a capability drop inside the guest.
   *Reason.* Keeping the supervisor in the VM leaves three egress layers and two control planes for one
   sandbox, needs an OpenShell compute driver for Substrate that does not exist (and that upstream has not
   signalled), and puts per-sandbox session state and a CA key into guest memory, which makes memory
   snapshots unsafe. Substrate with gVisor only (no microVM) saves 6-8 person-weeks but drops the VM golden
   path and returns later as a second migration. Staying on OpenShell keeps Koinon tied to a 0.1.x upstream
   that it pins and patches around (exec stdin cap, ConfigMap gateway config, no volume add).
2. **D2: accept the loss of per-binary credential binding.** OpenShell matched the calling executable;
   from outside a VM the gateway cannot see the process. The replacement is two rules: no binary in the
   guest ever holds a credential, and each gateway route carries its own method and path rules for that
   agent, so any process that reaches the gateway can do only what the route allows. This narrows defence
   in depth and is accepted knowingly. *Reason.* Keeping the binding needs an in-guest broker that holds
   credentials, which is what D1 removes.
3. **D3: tier map.** `vm` is a Substrate `microvm` class; `hardened` is a Substrate `gvisor` class (the
   tainted node pool of ADR 0075 stays an operator option, see D7); `dev` is `gvisor` on kind (or `microvm`
   where the runner has KVM). Plain containers do not exist in Substrate, so `dev` becomes stronger. The tier
   rules of ADR 0075 stay: one tier per install, visible badge, audit field, metric, preflight probes,
   "not applicable" as its own escape result, a qualification run per runtime. The map is final after the
   phase 3 qualification (`vm` on a KVM cluster, `hardened` on Bottlerocket). *Reason.* gVisor was out only
   because OpenShell needs Landlock and seccomp user notification; under D1 gVisor's user-space kernel is a
   stronger boundary than runc with user namespaces and it runs where KVM is absent.
4. **D4: Substrate is a pinned cluster prerequisite, not part of the chart.** Koinon ships an installer
   step (the ADR 0077 pattern) that runs a pinned `ate-setup` with a Koinon profile (S3-compatible object
   store from values, Postgres from Koinon's database mode, no bundled Prometheus or OpenTelemetry), and a
   preflight that checks the version range, the CRDs, a ready worker pool per tier class, the bootstrap
   owners, the object store and `/dev/kvm` for `vm`. An upstream contribution towards a self-managed
   install profile is drafted by an agent and posted by a human. *Reason.* Substrate's namespaces are
   fixed, so it is a cluster singleton that another product (kagent) may already run; its CA and Postgres
   set-up are imperative, which the chart cannot render deterministically (chart renders stay
   deterministic).
5. **D5: disk and memory snapshots from the start.** Suspend and resume use all three fidelities
   (volumes, rootfs, memory) from the first Substrate release of Koinon, not disk first. Because memory
   snapshots clone everything in the guest, the design must carry four things from day one:
   - short-lived actor identity tokens (300-3600 s, audience the Koinon gateway), minted outside the guest
     and never stored in it, so a memory snapshot holds no credential and a clone cannot reuse a live one;
   - snapshot encryption and access control: snapshots are encrypted at rest with a per-org key, and the
     object-store credential is scoped per org (on S3 Substrate otherwise shares one credential to the
     bucket);
   - an early verification path: the reconciler records a digest of every snapshot and checkpoint it makes
     and refuses a restore that does not match, because Substrate has no snapshot digest or signature
     before restore (upstream T-25 is open); Koinon's own tar checkpoints with digest (ADR 0032) stay as the
     export and restore format;
   - a restore-clone test: a restored actor must not share per-sandbox secrets or sessions with its
     source (single use, as Fly Machines does, or a proven rotation).
   *Reason.* The maintainer wants the density and the speed that memory snapshots give, and no later
   re-architecture. The cost is that D5 is the largest single risk of the plan: memory restore is eager on
   the current Cloud Hypervisor, so the speed gain is smaller than documented, and the restore bugs listed
   under Consequences apply. A cold path (boot from rootfs and volumes, then ACP `session/load`) always
   exists.
6. **D6: worker pools.** Shared across orgs on `vm` (the VM is the boundary); a per-org pool is an
   option, like `hardened.orgPools` in ADR 0075. An org is an atespace (ADR 0002). *Reason.* A pool per org
   costs idle workers per org.
7. **D7: privileged worker pods are accepted on any node.** Substrate worker pods run as root with
   `SYS_ADMIN`, `NET_ADMIN`, `SETUID`, `SETGID`, `DAC_OVERRIDE`, unconfined seccomp and AppArmor, and
   host paths for `/var/lib/ate` and `/dev/net/tun`; cloud-hypervisor runs without a jailer and without
   `--seccomp`. They need a `privileged` Pod Security namespace. Koinon accepts this on any node. A tainted
   node pool for workers is an operator option, not a requirement. Koinon's own controls are native
   ValidatingAdmissionPolicy only (never Kyverno): worker and atelet pods only in the Substrate namespace
   and only from the Substrate controller identity; image digests pinned; no `hostNetwork`; no extra
   hostPath beyond the documented set; ephemeral containers and `pods/exec` into workers denied; the
   `WorkerPool`, `SandboxConfig` and `CSIDriverConfig` objects writable only by the installer identity. Koinon
   follows upstream T-21 (privilege reduction) and drops capabilities when Substrate allows. *Reason.* No
   Substrate install works without them. *Cost.* The per-node trusted base grows: an escape from the guest
   into the VMM lands in a root pod. On a node pool shared with Koinon's own pods, that pod sits next to
   credential holders. The mitigation is operator choice (taints, a dedicated pool) and the single L7 point
   of D1: credentials live in the gateway, not on worker nodes.
8. **D8: minimum Kubernetes 1.37** for the Substrate driver (1.36 with the documented gates is
   unsupported). *Reason.* It avoids beta feature gates on managed clusters.
9. **D9: a one-minor dual-driver window.** OpenShell and Substrate both exist behind an install-wide
   switch `execution.driver: openshell | substrate` (no per-org mix); the default flips at a minor
   boundary; OpenShell and Agent Sandbox are deleted one minor later. *Reason.* Two drivers double the
   admission, tier, escape and CI matrix, so the window is as short as migration of live agents allows.
   Existing installs move per agent (suspend, Koinon tar checkpoint, create the actor, restore the home,
   resume through ACP `session/load`, ADR 0029) in an observable, stoppable reconciler mode.
10. **D10: spend.** KVM runs happen only in the lab (a cluster of the maintainer's own, never a CI or
    release gate). CI runs on GitHub standard runners with the fake model server. No paid runner, no paid
    model call (ADR 0111 and the billing rules). A paid KVM runner is a separate ask.
11. **D11: order with ADR 0097.** The Koinon gateway step 2 (revisioned config stream, model and MCP
    paths) comes before Substrate phase 2, with two new gateway jobs: forge and registry L7 egress routes
    and actor identity tokens. *Reason.* Without the gateway the L7 rules and the agent identity have no
    home, and the 55-76 s hot-pool start stays.

### The two invariants

**No credentials inside the sandbox.** Provider keys, forge tokens and MCP credentials live only in the
Koinon gateway (from the secret store). The only value the execution layer injects is a short-lived actor
identity token, outside the guest. The guest sees placeholders only, has no ServiceAccount token, no
SPIFFE socket and no Kubernetes route. Git over HTTPS uses a placeholder that the egress layer swaps.
ADR 0044 (the gated in-sandbox exception) stays as it is.

**Enforced egress in every tier.** L4: Substrate's `atenet` is default-deny in every tier, with no switch;
the only allowed destinations are the Koinon gateway and what an org explicitly grants as L4-only;
wildcard hosts stay refused (ADR 0075 amendment 13). L7: every inspected or credentialed route is a
gateway route, and the gateway has no audit-only mode. A plain NetworkPolicy on the worker namespace is a
second fence; the preflight fails `vm` and `hardened` without a NetworkPolicy engine. No CNI-specific
features.

### Seam and driver shape

The `execution.Backend` seam is reshaped first, while OpenShell is the only driver and behaviour does not
change: `Compute` (workspace, sandbox lifecycle, `Exec`, `Upload`, `Snapshot`) and `Egress` (L4 allowlist
and identity audience; no binaries, no L7). L7 rules and credentials leave the seam for the gateway seam
of ADR 0097. Filesystem policy moves into the guest launcher config (part of the image contract). One
contract suite runs against the OpenShell fake, the Substrate fake and the harness test backend.
Substrate has no exec in its control API; `Exec` runs over the in-guest `ProcessService`, `Upload` over
`FileSystemService`; this ACP path is proof P-A below.

## Phase plan

Sizes are judgement, not measurement (about 24-38 person-weeks for one senior engineer with agent lanes).

| Phase | Content | Exit |
| --- | --- | --- |
| 0 Spike (lab, not a public issue; 3-4 pw) | Finish the trial; measure cold boot (n >= 5) and resume (n >= 12) with p50 and p90; proofs P-A (ACP stream over `ProcessService` for 30 minutes, 2 000 ACP messages, a 10 MiB upload, no lost first output after resume), P-B (the actor reaches an in-cluster gateway Service through `atenet` with an injected actor token and nothing else; direct IPs, DNS, metadata and the API server fail), P-C (a launcher applies Landlock and seccomp in the microVM and in gVisor; the escape probes pass); probe `/dev/kvm` in kind on a standard runner | Numbers recorded; P-A, P-B, P-C pass or each failure has a cause and an upstream issue; the maintainer says go or stop |
| 1 Decide and reshape (4-6 pw) | This ADR; ADR "execution hardening defaults" (#1084); seam reshape and contract suite; gateway step 2 with forge, registry and identity jobs | ADRs accepted; OpenShell passes the contract suite; the gateway serves forge and registry routes for OpenShell sandboxes too |
| 2 Parallel backend (8-12 pw) | `internal/execution/substrate`, fakes, guest launcher, rebuilt flavor images, `execution.driver`, preflight, worker VAPs and NetworkPolicy, object store and ateapi database values, chart-check and chart-smoke files, actor-state observer, keep-warm mapping, checkpoints with digests, an advisory kind e2e leg | Quickstart e2e, harness contract tests and the restore drill green on the substrate leg; stable for 2 weeks |
| 3 Escape-suite parity (4-6 pw) | Driver-neutral `test/escape`; new specs (actor cannot call ateapi or atelet; actor token audience and TTL; a restore with a wrong digest is refused; cross-org isolation on one worker; worker-pod VAP denials; restore-clone test); qualify `vm` and `hardened` | Every spec passes or is "not applicable" with a reason on both tiers; a fresh security review finds no open blocker; the maintainer accepts the report |
| 4 Default flip (3-5 pw) | New installs default to `substrate`; per-agent migration; docs in the docs repository | Queue quickstart green on the new default; a migration drill moves live agents with no lost home; one rc soaks for 2 weeks |
| 5 Removal, one minor later (2-3 pw) | Delete the OpenShell backend, protos, fake gateway, reconciler parts, chart templates and VAPs, spike leftovers and checks; AGENTS.md rules; mark ADRs superseded | No OpenShell or `agents.x-k8s.io` reference outside `docs/adr` history |

The work breaks into the issues listed under #1082; this ADR is its first.

## ADRs affected

Amendments are recorded in the front matter of this ADR (`amends`) and named in each older ADR's header.
The text of an older ADR is not rewritten: it stays as the record of its time.

| ADR | Action |
| --- | --- |
| 0101 | Superseded by this ADR |
| 0045 | Amended; superseded by "Execution hardening defaults" (#1084) |
| 0048 | Amended: its execution, install and reconciler decisions go with #1084; its gateway decisions stay with 0040 and 0097 |
| 0075 | Amended: tier map of D3, on top of the PR 1081 amendment (the `vm` tier is any microVM) |
| 0077, 0106 | Amended: pin Substrate; an atespace label replaces the install mark. Both become superseded at phase 5 when OpenShell is removed |
| 0078 | Amended: an actor-state observer replaces the pod observer. Superseded at phase 5 |
| 0002 | Amended: an org is an atespace; worker pools shared or per org (D6) |
| 0006, 0040, 0097 | Amended: actor token identity; forge and registry L7 routes join the gateway (D1, D11) |
| 0009 | Amended: `keep_warm` maps to resumed actors and pool sizing |
| 0015, 0016 | Amended: flavor images carry the guest server and launcher, not OpenShell base images |
| 0029 | Amended: idempotency records live only in the gateway |
| 0032, 0051, 0112 | Amended: home is a Substrate durable volume; Koinon tar checkpoints with digest; upload over `FileSystemService`; snapshots per D5 |
| 0080, 0113 | Amended: egress hosts and registry rules are gateway routes |
| 0083 | Amended: resources map to actor template and worker pool sizes |
| 0023 | Amended: Substrate metrics are scraped with a label allowlist that drops actor IDs |
| 0061 | Amended after open question Q4: how the Substrate node agent pulls actor images and uses mirrors |
| 0044, 0110 | Reviewed, relates only; wording changes only ("provider records" stop existing) |

## AGENTS.md text changes

These rules are part of this decision. The phase 1 rows (No secrets, hardening, upstream) are applied to
AGENTS.md in this change; this change merges only when 0119 is accepted. The hardening rule names the
hardening ADR by issue (#1084) until that ADR has a number. The later rows land in their phases:

| Rule today | New text | When |
| --- | --- | --- |
| "**No secrets in sandboxes.** Credentials stay in OpenShell provider records, agentgateway or the secret store; never put them in sandbox env, images, home volumes or logs. The only exception is ADR 0044 and it is gated." | "**No secrets in sandboxes.** Credentials stay in Koinon's gateway or the secret store; the execution layer injects only a short-lived actor identity token, outside the guest. Never put a credential in sandbox env, images, home volumes, snapshots or logs. The only exception is ADR 0044 and it is gated." | Phase 1 |
| "Don't weaken OpenShell defaults from ADR 0045 (`enforcement: enforce` in every tier) or the isolation tiers of ADR 0075 (...Kata...)" | "Don't weaken the execution hardening defaults of ADR NNNN (default-deny egress and enforced L7 rules on every inspected or credentialed route, in every tier; Landlock and seccomp in the guest where the tier supports them) or the isolation tiers of ADR 0075 (a microVM per sandbox for the `vm` tier)." (NNNN is the number of the hardening ADR) | Phase 1 |
| "**OpenShell upstream (NVIDIA/OpenShell).** ..." | "**Agent Substrate upstream.** Issues need a first-hand human user story and a repro on a plain Substrate install. A human posts; an agent drafts. No AI attribution upstream." | Phase 1 |
| "**OpenShell exec stdin is capped.** ..." | Deleted | Phase 5 |
| Commands: "Sandbox admission policy ... `make test-escape-kind`" | Point at the worker-namespace VAPs and the driver-neutral escape target | Phase 3 |
| "Never Kyverno"; "No CNI-specific features" | Unchanged: Substrate uses a native VAP and plain NetworkPolicy | - |

## Alternatives considered

- **Keep the OpenShell supervisor inside the microVM** (option a). Rejected under D1: two control planes,
  three egress layers, unsafe memory snapshots, an OpenShell driver for Substrate that needs an upstream
  vouch, and untested anywhere.
- **Substrate with gVisor only, no microVM tier.** Rejected: it saves 6-8 person-weeks now, but a security
  review or a customer brings the microVM work back, plus a migration of live installs.
- **Keep OpenShell and Agent Sandbox and copy Substrate's lessons** (the recommendation of the 2026-11-17
  research note). Rejected by the maintainer: Koinon stays tied to a 0.1.x upstream it cannot direct.
- **Wait for an OpenShell Substrate driver.** Rejected: no signal it is coming, and it would land option a
  on someone else's timetable.
- **Bundle Substrate in the chart.** Rejected under D4.
- **Disk snapshots first, memory later** (the plan's recommended answer). The maintainer chose both from
  the start (D5); the cost is the extra safeguards listed there.
- **Adopt kagent 1.x.** Still rejected, as in 0101: it covers the execution layer, not the work layer, and
  its approval gate runs inside the agent runtime.

## Consequences

- Positive: one compute control plane that Koinon does not patch around; a VM per sandbox on `vm` and a
  gVisor tier for nodes without KVM; credentials out of the guest, so memory snapshots hold none; one L7
  enforcement point that Koinon owns and tests; the exec stdin cap and the OpenShell upload relay go away;
  the OpenShell, Agent Sandbox and gateway-ConfigMap workarounds can be deleted one minor after the flip.
- Negative: the per-binary credential binding is lost (D2); the per-node trusted base grows with
  privileged workers and a VMM without a jailer (D7); Koinon depends on Substrate's in-guest server for
  exec and files, since the control API has no exec; Koinon owns glue against one young upstream (created
  2026-05, 1.0 on 2026-11-05, Google-led roadmap); a Substrate install is a new cluster prerequisite with
  an object store, a database and Kubernetes 1.37; the work is the largest in the project to date.
- Plan risks and mitigations: the gateway is not built yet (D11 orders it first). Open upstream restore
  bugs matter for ACP and D5: 3801 (restore inside the CPU limit is slow), 3654 (unlinked file mappings fail
  after resume), 3537 (the first output after restore can be lost; ACP reconnects with `session/load`),
  3250 (Cloud Hypervisor version). The autoscaler cannot scale a KVM pool from zero (2691), so a minimum
  pool size is documented. Substrate broke upgrades before 1.0: Koinon pins one Substrate minor per
  release, tests against a real Substrate on kind in CI, suspends with volumes only across a Substrate
  upgrade (memory snapshot formats changed between release candidates) and reads every release note inside
  the dependency cooldown (ADR 0079).
- Follow-ups: the hardening ADR (#1084); the AGENTS.md rows of phases 3 and 5; the issue breakdown of #1082 (seam reshape,
  gateway jobs, guest launcher, driver, install and chart, homes and warm start, CI leg, escape suite,
  migration and flip, removal); ADR status updates at phase 5; `docs/design.md` prior art for Substrate.

## Open questions

- Q1: does ACP run over `ProcessService` for hours without loss? (proof P-A)
- Q2: can the actor reach an in-cluster Service only through `atenet` with an injected token? (P-B)
- Q3: is Landlock active at boot in the Kata guest kernel, and what does gVisor support? (P-C)
- Q4: image pull secrets and registry mirrors for the Substrate node agent (ADR 0061).
- Q5: is KVM available inside kind on a GitHub standard runner?
- Q6: Kubernetes 1.37 availability and nested KVM on EKS and AKS node types, before the default flip.
- Q7: does a worker host one actor at a time or several? (affects D6 and the cross-org escape spec)
- Q8: snapshot encryption design for D5: who holds the per-org key and where the restore verification
  runs.

Each gets a tracking issue under #1082.

## Verification

Pending: the phase 0 spike. State at the time of writing, from the lab trial of 2026-11-17 and 2026-11-18 on
a Talos cluster with Kubernetes 1.37.1:

- Passed: the Kubernetes prerequisites (the certificates API serves `PodCertificateRequest` and
  `ClusterTrustBundle`, the kubelet gates are on, both workers have `/dev/kvm`, `/dev/net/tun` and
  vhost-vsock); the Substrate installer completed in about 6 minutes; the four microVM assets matched their
  sha256 pins; the `microvm` class applied and 3 of 3 worker pods were ready in 12 s; cleanup left nothing
  behind.
- Not passed: the measurement step stopped at the first API call (`create atespace`) with "invalid bearer
  token". The suspected cause is that the cluster's service-account issuer is a non-default address that
  the Substrate API server cannot reach; this is a hypothesis, not confirmed.
- No timing numbers exist yet (cold boot, resume), and P-A, P-B and P-C have not run.

This ADR stays Proposed until the maintainer has read the phase 0 result (numbers with p50 and p90, and the
three proofs, each passing or with a cause and an upstream issue) and said go. Acceptance then cites that
result here and changes the status.

## Upstream verification

Checked 2026-11-18 against Substrate v1.0.0: the compatibility promise, the CRD `sandboxconfigs.ate.dev`,
the fixed namespaces, the privileged worker pod spec, no exec in the control API (upstream issues 185 and
2042), no self-managed install profile (1782), the open restore bugs and T-21 and T-25 named above.

## Prior art

A quick check (about 15 minutes) of comparable sandbox platforms:

- E2B: Firecracker microVMs with an in-VM daemon for exec and files; a filesystem-only snapshot mode that
  reboots on resume. We borrow the in-guest daemon pattern (`ProcessService`) and the cold path.
  <https://e2b.dev/docs/sandbox/snapshots>
- Daytona: a VM class added beside the container default, not replacing it. We borrow the dual-driver
  window (D9). <https://www.daytona.io/dotfiles/vms-pause-and-fork>
- Modal: gVisor as a production boundary, memory snapshots opt-in with an expiry, default-deny egress.
  We borrow gVisor for `hardened` (D3). <https://modal.com/docs/sdk/py/latest/Sandbox.md>
- Fly Machines: single-use suspend snapshots against clone attacks, always a cold path. We borrow both for
  D5. <https://www.fly.io/docs/reference/suspend-resume/>
- kagent: moved to a fork of Substrate with no in-place upgrade; users recreate resources. We differ: we
  migrate agents in place (D9).
  <https://kagent.dev/docs/kagent/concepts/agent-substrate/>

None of these publishes a benchmark that we rely on; Substrate's "resume under 500 ms" stays unverified
until the phase 0 spike measures it. The full research workflow is not run for this ADR; it was settled
by the maintainer after a plan that cites these sources.

## Links

- [0101](0101-agent-substrate-and-kagent-not-adopted.md), the decision this one supersedes.
- [0045](0045-openshell-hardening-defaults.md), [0048](0048-openshell-spike-results.md),
  [0075](0075-isolation-tiers.md), [0077](0077-pinned-openshell-and-installer.md),
  [0097](0097-koinon-owned-gateway.md), [0106](0106-install-mark-owns-openshell-objects.md).
- Agent Substrate: <https://github.com/agent-substrate/substrate> (release v1.0.0, 2026-11-05).
- Epic [#1082](https://github.com/stateracloud/koinon/issues/1082); related [#1080](https://github.com/stateracloud/koinon/issues/1080),
  [#67](https://github.com/stateracloud/koinon/issues/67), watch issue [#53](https://github.com/stateracloud/koinon/issues/53),
  hardening ADR [#1084](https://github.com/stateracloud/koinon/issues/1084).
