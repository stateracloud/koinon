# 0101. Agent Substrate and kagent 1.x evaluated, not adopted for v0.x

- Status: Accepted (maintainer decision 2026-11-04)
- Date: 2026-11-08
- Relates to: [0011](0011-agent-to-agent.md), [0042](0042-ask-tier-park-and-execute.md), [0045](0045-openshell-hardening-defaults.md), [0073](0073-agent-passport.md), [0075](0075-isolation-tiers.md), [0081](0081-run-limits.md)
- Issue: [#130](https://github.com/stateracloud/koinon/issues/130)

## Context

Two projects now overlap with Koinon's execution layer (sandbox, suspend and resume, credential
injection outside the sandbox, default-deny egress). The facts below are from a research pass on
2026-11-04 (primary sources listed at the end).

- **Agent Substrate** (`agent-substrate/substrate`, Apache-2.0, Google-led CNCF Sandbox project, also
  offered on GKE). An actor runtime: many mostly idle actors share a small pool of warm worker pods,
  and idle actors are snapshotted (memory, rootfs or volumes) and restored. Every actor runs in gVisor
  or a Cloud Hypervisor microVM; plain containers are not supported. Latest release v1.0.0-rc.2
  (2026-11-04); the README still says pre-1.0 with no compatibility guarantee. rc.2 cannot upgrade
  from v0.6.0 (2026-10-22), and rc.1 (2026-10-29) to rc.2 breaks gVisor memory snapshots: two broken
  upgrades in about two weeks. On GKE it needs Standard 1.36 with beta flags or 1.37+ and Cloud
  Storage, and GKE documents it for evaluation and non-production use.
- **kagent** (`kagent-dev/kagent`, Solo.io origin, Apache-2.0, CNCF Sandbox since 2025-05-22).
  v1.0.0-rc1 (2026-10-30) and v1.0.0-rc2 (2026-11-04) are rebuilt on a kagent-dev fork of Substrate,
  with a new `kagent.dev/v1alpha3` API and Claude and Codex harnesses. rc1 removed public
  suspend and resume controls and made pre-RC API breaks. No GA date; the v0.10.x line stays
  maintained (v0.10.4, 2026-10-21). Credentials are injected outside the sandbox by Substrate's
  egress gateway, so kagent now has a sandbox credential model.

The overlap is the execution layer. It is not the work layer: orgs, triggers, tasks, inbox, code
host, spend ledger and run caps (ADR 0081), and approvals that Koinon executes outside the runtime
(ADR 0042). kagent's `requireApproval` is enforced inside the agent runtime (its ADK).

## Decision

1. **Koinon keeps OpenShell on Agent Sandbox** as its execution layer for v0.x. Agent Substrate and
   kagent 1.x are evaluated and not adopted.
2. **Reasons.**
   - OpenShell needs Landlock and seccomp user notification, which gVisor lacks (ADR 0075, gVisor is
     a non-goal). Substrate's other runtime is Cloud Hypervisor; whether the OpenShell supervisor
     works inside a Substrate microVM is unverified, and OpenShell drives Agent Sandbox, not
     Substrate actors. Adopting Substrate would mean dropping OpenShell (and the L7 `enforce` of ADR
     0045 and its escape suite) or asking OpenShell upstream for a Substrate driver.
   - Substrate is pre-1.0 and its API broke upgrades twice in two weeks; kagent pins a fork of it and
     has its own breaking rcs.
   - Substrate's strongest features are GKE-first (beta APIs, Cloud Storage); Koinon runs on any
     conformant Kubernetes with no CNI-specific features.
   - The roadmap of both is weighted to Google and Solo.io.
3. **Integrate at the edges, not in the core.** A2A v1.0 at the external edge (ADR 0011) and MCP
   through agentgateway (a kagent `/mcp` endpoint can be registered as a shared MCP server with
   Koinon grants and risk tiers). Pin versions and test against the code: kagent's docs site
   disagrees with its rc2 code in places.
4. **Borrow, do not depend.**
   - Actor JWT bounds (audience list, 300 to 3600 s lifetime, injected by the gateway, never in the
     runtime) for the agent passport of ADR 0073.
   - The snapshot fidelity vocabulary (volumes, rootfs, memory) for future checkpoint ADRs.
   - Per-binding approval defaults for the risk-tier presets.
5. **Revisit when any of these holds.**
   - Substrate reaches 1.0 GA with a compatibility promise.
   - OpenShell supports Substrate (a driver, or a microVM qualification of its supervisor).
   - Substrate is usable off GKE without beta APIs.
   - Koinon measures a density or cold-start problem that suspended sandboxes cannot meet.

## Alternatives considered

- **Adopt Agent Substrate in place of Agent Sandbox** _(reconstructed)_. Rejected: OpenShell needs
  Landlock and seccomp user notification, which gVisor lacks, and OpenShell drives Agent Sandbox, not
  Substrate actors. Substrate is pre-1.0 and broke upgrades twice in two weeks, and its strongest
  features are GKE-first.
- **Adopt kagent 1.x** _(reconstructed)_. Rejected: its rcs are rebuilt on a fork of Substrate with
  pre-RC API breaks and no GA date. It covers the execution layer but not the work layer, and its
  `requireApproval` is enforced inside the agent runtime rather than outside it.
- **Ask OpenShell upstream for a Substrate driver** _(reconstructed)_. Not pursued now: whether the
  OpenShell supervisor works inside a Substrate microVM is unverified, and Substrate has no
  compatibility promise yet. This stays a revisit condition (see Decision 5), not a current request.

## Consequences

- `docs/design.md` prior art is corrected: kagent has a sandbox credential model through Substrate;
  it lacks the work layer, approvals executed outside the runtime, spend and run caps and org
  tenancy. Agent Substrate gets its own row.
- Koinon keeps paying for one suspended sandbox and a PVC per agent instead of memory snapshots and
  dense multiplexing. The Substrate README claims resume under 500 ms and over 500 activations per
  second, and 10 to 30 times oversubscription; these are unverified and not a reason to switch
  without a measured Koinon problem.
- kagent adds coding-agent harnesses; if it adds triggers, tasks and an inbox it competes with
  Koinon's core. Koinon's position is the work layer, inspected L7 enforcement, approvals outside the
  runtime and the spend ledger.
- Interop tests (a kagent agent as an external A2A peer, and the reverse; `input-required` aligned
  with parked runs) are follow-up work, not part of this decision.

## Sources

- Agent Substrate: https://github.com/agent-substrate/substrate (README at commit `17fa8586`;
  releases v1.0.0-rc.2, v1.0.0-rc.1, v0.6.0, v0.1.0); https://www.cncf.io/projects/agent-substrate/;
  https://docs.cloud.google.com/kubernetes-engine/ai-ml/about-agent-substrate
- kagent: https://github.com/kagent-dev/kagent/releases/tag/v1.0.0-rc2 and `/tag/v1.0.0-rc1`;
  https://www.cncf.io/projects/kagent/; https://kagent.dev/blog/kagent-agent-substrate-sandboxes
- Research report: "kagent and Agent Substrate, compared with Koinon" (2026-11-04), in the Koinon
  research and decisions store.
