# 0039. Development setup: a maintainer's Mac plus a Talos+Kata cluster

- Status: Accepted
- Date: 2026-10-21
- Area: engineering-process
- Tags: testing, isolation, tooling
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
- **Remote:** pushed to the private _(changed: the repository is public since 2026-11-05, see [0094](0094-work-claims-on-the-public-repository.md))_ `stateracloud/koinon` (amended 2026-10-26; it was `procoduck/koinon`, see [0034](0034-project-identity.md)). Work goes on branches with PRs, one per
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
  - The OpenShell spike (old issue 3) installs Agent Sandbox
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

## Amendment (2026-11-11, issue 77): what the RuntimeClass overhead covers

The 130Mi and 250m above are right for the `kata` (Cloud Hypervisor) handler, but this ADR did not
say what they cover. `podFixed` does not cover Kata's `default_memory`. The model, checked against
Kata Containers 4.3.1 and the Kubernetes Pod Overhead docs:

- **Guest size.** The Go runtime gives the guest `default_memory` (2048 MiB by default) plus the
  sum of the container memory limits: hot-plugged after boot, or at boot with
  `static_sandbox_resource_mgmt`. runtime-rs with static sizing (its default) uses the limits plus
  `overhead_memory`, and `default_memory` only when no limits are set. vCPUs follow the same rule
  with `default_vcpus` and `overhead_vcpus`.
- **What the host charges.** Kubernetes sets the pod cgroup limit to the sum of the container
  limits plus `podFixed`, and adds `podFixed` to scheduling and ResourceQuota. Guest RAM is charged
  to the host only when the guest touches it (unless `enable_mem_prealloc` or hugepages are on). With
  `sandbox_cgroup_only = true` the shim, VMM and virtiofsd also run in the pod cgroup; with `false`
  (the Go runtime's build default) only the vCPU threads do and the rest goes to `/kata_overhead`.
- **So `podFixed` is** what one sandbox uses on the node beyond its container limits: the guest
  kernel and kata-agent plus the host-side processes. It is not `default_memory`. Kata's sizing guide
  says to set it to the guest-side plus host-side overhead and to measure it.
- **Example.** kata-deploy 4.3.1 ships memory 130Mi, cpu 250m for Cloud Hypervisor (Talos's
  `kata` handler) and 320Mi, 250m for QEMU (raised from 160Mi, which let the VMM get OOM-killed in
  small pods). A sandbox with a 2Gi, 1-cpu limit on `kata` then gets a pod cgroup of 2178Mi and
  1250m; the guest has 4096 MiB nominal, of which the host charges only the pages it uses.
- **cpu too.** The preflight warns when either resource is missing. Missing memory can OOM-kill the
  VMM; missing cpu cannot kill, but the scheduler undercounts and the guest's and VMM's work comes out
  of the workload's cpu limit.

Sources: [Kata sizing guide](https://github.com/kata-containers/kata-containers/blob/4.3.1/docs/how-to/how-to-size-sandbox-overhead-runtime-rs.md),
[Kata host cgroups](https://github.com/kata-containers/kata-containers/blob/4.3.1/docs/design/host-cgroups.md),
[static sizing in the Go runtime](https://github.com/kata-containers/kata-containers/blob/4.3.1/src/runtime/pkg/oci/utils.go#L1334-L1339),
[hot-plug sizing](https://github.com/kata-containers/kata-containers/blob/4.3.1/src/runtime/virtcontainers/sandbox.go#L2589-L2594),
[kata-deploy RuntimeClass defaults](https://github.com/kata-containers/kata-containers/blob/4.3.1/tools/packaging/kata-deploy/helm-chart/kata-deploy/templates/runtimeclasses.yaml#L1-L40),
[Kubernetes Pod Overhead](https://kubernetes.io/docs/concepts/scheduling-eviction/pod-overhead/).

## Links

- [0025 Testing](0025-testing-strategy.md), [0026 Home lab and license](0026-home-lab-and-license.md)
- [0037 Tooling](0037-tooling.md), [0045 OpenShell hardening defaults](0045-openshell-hardening-defaults.md)
- [old issue 3 OpenShell spike](old issue 3)

Note (2026-11-07): private infrastructure detail removed; maintainer decision 2026-11-07.
