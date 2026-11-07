# 0039. Development setup: a maintainer's Mac plus a Talos+Kata cluster

- Status: Accepted
- Date: 2026-10-21
- Decision: [design.md#39](../design.md)

## Context

Koinon's core promise is isolation: Kata VMs, the OpenShell supervisor, and no credentials in the
sandbox. That can only be tested properly on a cluster with a VM runtime. Free GitHub runners can't
run Kata, and cloud clusters cost money.

procoduck has a Talos cluster with the Kata Containers system extension installed. Development
is done with Claude Code on procoduck's Mac, which has full cluster and GitHub access. Some actions are
hard to undo and need a human check first.

## Decision

- **Code:** development happens in a local git repository on procoduck's Mac.
- **Remote:** pushed to the private `stateracloud/koinon` (amended 2026-10-26; it was `procoduck/koinon`, see [0034](0034-project-identity.md)). Work goes on branches with PRs, one per
  deliverable during the bootstrap.
- **Tests:** tests that need real isolation deploy to the **maintainer's Talos+Kata cluster** with Kata
  (RuntimeClass `kata`). That cluster is for development and local testing only, never public CI
  ([0026](0026-home-lab-and-license.md)).
- **What Claude Code may do:** it has full cluster and GitHub access, but **asks before**:
  - making a repository public
  - deleting repositories or branches others use, or force-pushing
  - cluster-wide changes that affect workloads other than Koinon's
  - anything that costs money
- **Koinon's footprint on the cluster:** dedicated namespaces (prefix `koinon-` plus OpenShell's
  managed workspace namespaces). Cluster-scoped objects are limited to CRDs, RuntimeClass use and
  ValidatingAdmissionPolicies scoped to Koinon namespaces _(inferred)_.

## Alternatives considered

- **Cloud dev clusters** (EKS or GKE with nested virtualization) _(reconstructed)_: closer to
  production, but they cost money and need setup.
- **kind or k3d only** _(reconstructed)_: free and fast, but no Kata. Fine for envtest/e2e logic,
  not for escape tests. Still used in CI ([0025](0025-testing-strategy.md)).
- **Self-hosted CI runners on the home lab** _(reconstructed)_: would run escape tests on PRs, but
  exposes the home lab to untrusted PR code. Rejected for public CI.

## Consequences

- Positive:
  - Real Kata isolation at no cloud cost.
  - Fast iteration with Tilt ([0037](0037-tooling.md)).
- Negative:
  - The Kata escape tests run on the home lab, not in public CI. CI runs them against OpenShell
    sandboxes without Kata, so the Kata layer is tested less often than on every PR.
  - Agent Sandbox's upstream Kata examples cover AKS and GKE only, so **Talos plus Kata is untested
    upstream** and the spike must check it.
  - The home lab is a single environment: no multi-zone tests of home restore
    ([0032](0032-agent-home-checkpoints.md)) _(inferred)_.
- Follow-ups:
  - The OpenShell spike ([koinon-private#3](https://github.com/stateracloud/koinon-private/issues/3)) installs Agent Sandbox
    and OpenShell on the home lab with `default_runtime_class_name` set to the Kata RuntimeClass.
  - Document the home-lab prerequisites (Talos version, Kata extension, registry) in
    `CONTRIBUTING.md` for other contributors with their own clusters _(inferred)_.

## Open questions

- Should a home-lab escape-suite run be required before releases (manually triggered), given it
  can't run in public CI?
- Which RuntimeClass name does the cluster use? Talos's `kata` handler (Cloud Hypervisor) or
  `kata-qemu`? OpenShell docs use `kata-containers` in examples.

## Upstream verification

Checked 2026-10-21:

- **Talos Kata extension:**
  - It registers handlers `kata` (Cloud Hypervisor) and `kata-qemu`.
  - The example RuntimeClass is `kata`, with `overhead.podFixed` memory 130Mi and CPU 250m.
  - https://github.com/siderolabs/extensions/blob/main/container-runtime/kata-containers/README.md
- **OpenShell runtime class settings:** `runtime_class_name` per sandbox or template, and
  `default_runtime_class_name` per gateway. Caller-chosen RuntimeClasses need admin approval labels.
  `docs/how-it-works/sandboxes/templates.mdx` in https://github.com/NVIDIA/OpenShell
- **Agent Sandbox:** Kata examples exist only for AKS and GKE (`examples/kata-*`).
  https://github.com/kubernetes-sigs/agent-sandbox

## Links

- [0025 Testing](0025-testing-strategy.md), [0026 Home lab and license](0026-home-lab-and-license.md)
- [0037 Tooling](0037-tooling.md), [0045 OpenShell hardening defaults](0045-openshell-hardening-defaults.md)
- [koinon-private#3 OpenShell spike](https://github.com/stateracloud/koinon-private/issues/3)

Note (2026-11-07): private infrastructure detail removed; maintainer decision 2026-11-07.
