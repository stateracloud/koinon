# 0045. OpenShell hardening defaults

- Status: Accepted; amended by [0048](0048-openshell-spike-results.md) and [0075](0075-isolation-tiers.md)
- Date: 2026-10-21
- Amends: none (implements ADR 0002, ADR 0006, ADR 0016 and "Execution layer details")
- Verification: Pending: OpenShell spike (old issue 3)

## Context

This ADR records small, reversible configuration choices that Claude made under the bootstrap's
"decide small reversible things yourself" rule. procoduck can overrule any of them.

Research on OpenShell `main` (2026-10-21) found defaults that weaken Koinon's model if left alone:

- L7 endpoints default to `enforcement: audit`, which logs and **allows** requests.
- Plain environment variables can be set through `SandboxSpec.environment` and template `--env`;
  they are not placeholders.
- `allow_uninspected_credentials` and `tls: skip` let a credentialed endpoint bypass inspection.
- Workspaces default to `shared` mode; decision 2 wants one namespace per org (`managed`).
- Caller-chosen RuntimeClasses need admin approval labels; a gateway default exists.
- AppArmor configuration was removed for Kubernetes (RFC 0012).
- Ephemeral containers are added through the `pods/ephemeralcontainers` subresource, which a
  ValidatingAdmissionPolicy matching only `pods` does not see.

## Decision

The reconciler and Helm chart always set:

1. `enforcement: enforce` on every rendered L7 endpoint.
2. No secrets in `SandboxSpec.environment` or template env; secrets only as provider records.
3. Never `allow_uninspected_credentials` or `tls: skip` on credentialed endpoints.
4. `workspace_mode: managed` for the Kubernetes driver.
5. `default_runtime_class_name: kata` (the Talos extension's handler name); operators may change it.
6. The sandbox-namespace ValidatingAdmissionPolicy matches `pods` **and** `pods/ephemeralcontainers`
   and checks allowed runtime classes, no `hostNetwork` and a pinned supervisor image.
7. Rendered values are verified by envtest tests and by the escape suite.

## Alternatives considered

- Accept upstream defaults and document them: leaves audit-only enforcement in place by accident.
- Enforce via a mutating policy: MutatingAdmissionPolicy is GA in Kubernetes 1.36, but rendering
  correct values in the reconciler is simpler and testable. _(reconstructed)_

## Consequences

- Positive: misconfiguration fails safe; the defaults are visible in one place.
- Negative: stricter than upstream; some OpenShell examples will not work unchanged.
- Follow-ups: tests asserting each default; VAP manifest in `charts/koinon`; spike verifies
  enforcement.

## Open questions

- Should the VAP also reject sandboxes that set `allow_uninspected_credentials`, as a second line of
  defence?

## Upstream verification

Checked 2026-10-21: OpenShell `docs/how-it-works/policies/schema.mdx`,
`docs/how-it-works/providers/overview.mdx`, `docs/kubernetes/workspaces.mdx`,
`docs/how-it-works/gateways/configuration.mdx`, `docs/kubernetes/migrate-gateway-config.mdx`;
Kubernetes admission `Rule.resources` subresource semantics; Talos Kata extension README.

## Links

- https://github.com/NVIDIA/OpenShell/blob/main/docs/how-it-works/policies/schema.mdx
- https://github.com/kubernetes/api/blob/master/admissionregistration/v1/types.go
- https://github.com/siderolabs/extensions/blob/main/container-runtime/kata-containers/README.md
- ADR 0002, ADR 0006, ADR 0016, ADR 0026

## Amendment (2026-10-28): isolation tiers

[0075](0075-isolation-tiers.md) makes Kata the golden path, not the only one. Every install has an
isolation tier: `vm` (Kata, AKS Pod Sandboxing or GKE `microvm`; the default), `hardened` (runc with
user namespaces, SELinux where present, a dedicated tainted sandbox pool and enforced NetworkPolicy)
or `dev` (kind and CI only).

- Decision 5: `default_runtime_class_name` follows the tier. The Kata runtime class stays required
  for the `vm` tier.
- Decision 6: the VAP's allowed runtime classes are the tier's.
- Decisions 1 to 4 and 7, and the 0048 additions (`stable_placeholder`, Landlock `hard_requirement`,
  telemetry off, requester-identity VAP rules, `io.katacontainers.*` denied), hold in every tier.

## Amendment (2026-11-10): the policies' own scope (old issue 475)

The policies select sandbox namespaces by label, so three paths could move a sandbox out of them.

- **Namespace labels are protected.** A fourth policy, `<namespace>-<fullname>-sandbox-namespaces`,
  refuses any change to the labels the selectors use (`openshell.ai/managed-by`,
  `openshell.ai/gateway-id` and every key of `sandboxPolicy.namespaceSelector`) on a namespace the
  policies cover, except from `sandboxPolicy.identities.namespaces` (default: the OpenShell gateway,
  which creates the namespaces in managed mode). Cluster admins are denied. It binds to every
  namespace update and reads the old object's labels itself, because a namespace selector reads the
  new object's labels, and the new object is the one that has just left the selection. Moving a
  namespace into the policies (adding the labels) stays open.
- **Gateway ID drift is reported.** The reconciler compares `openshell.gatewayId` with the gateway's
  `gateway_id` (read from OpenShell's gateway ConfigMap) on every pass, the first included, and sets
  `koinon_reconciler_openshell_gateway_id_mismatch`. The preflight still fails the install or upgrade.
- **Sandbox pods mount only the claim their Sandbox made.** A pod rule sees the pod's volumes, not
  the PersistentVolume behind a claim, and admission has no object lookup, so a claim bound to a
  `hostPath` or `csi.spiffe.io` PV would pass the volume rules. The pods policy therefore refuses
  every `persistentVolumeClaim` volume whose `claimName` is not `<volume name>-<sandbox name>`,
  where the sandbox is the one `Sandbox` controller owner reference (`agents.x-k8s.io`) on the pod
  itself. A pod with no such owner, a claim of another name and every generic `ephemeral` volume
  are denied (supervisor pods too). `<template>-<sandbox>` is ambiguous when a template name may
  hold `-` (volume `workspace-sb` on Sandbox `a`, volume `workspace` on Sandbox `sb-a`), so the
  pods policy refuses a `-` in the volume name and the objects policy refuses a `-` in any
  `Sandbox` `spec.volumeClaimTemplates[].metadata.name` and a set
  `spec.volumeClaimTemplates[].spec.volumeName` (it would bind the claim to a chosen PV). The
  claim check runs on CREATE only: pod volumes cannot change after create, and an update that
  drops the owner reference (the garbage collector orphaning the pod) must not be denied. Other
  spec changes stay denied to everyone but the controller. The facts it rests on:
  - Agent Sandbox v1.0.7 names the claim of `volumeClaimTemplate` T for Sandbox S `T-S`, mounts it
    as volume T, creates it with S as controller owner, and refuses a claim owned by another
    controller. It adopts an unowned one only with the sandbox tracking label or the pool label:
    [pod volumes](https://github.com/kubernetes-sigs/agent-sandbox/blob/v1.0.7/controllers/sandbox_controller.go#L1507-L1521),
    [claim reconcile](https://github.com/kubernetes-sigs/agent-sandbox/blob/v1.0.7/controllers/sandbox_controller.go#L1718-L1785),
    and the pod's owner reference
    ([L1532](https://github.com/kubernetes-sigs/agent-sandbox/blob/v1.0.7/controllers/sandbox_controller.go#L1532)).
  - OpenShell 0.1.7 asks for one template, `workspace`, so its claim is `workspace-<sandbox>`, and
    does not use generic ephemeral volumes
    ([template](https://github.com/NVIDIA/OpenShell/blob/v0.1.7/crates/openshell-driver-kubernetes/src/driver.rs#L7562-L7590)).
    Its `driver_config` volumes (existing claims named by the caller, `driver.rs#L774-L822`) do not
    match the rule and are denied; Koinon does not use them.
  - A pod rule cannot read the claim object or the Sandbox, so the rule checks names the pod
    carries; it does not check who owns the claim.

  Remaining limit: an identity that can create a PersistentVolumeClaim in a sandbox namespace could
  pre-create `workspace-<sandbox>` with the tracking label and have the controller adopt it, and the
  claim could be bound to a PV an admin made (hostPath or `csi.spiffe.io`). The guard for that is
  outside pod admission (RBAC on claims, or a policy on `persistentvolumeclaims`); this rule
  assumes no non-admin identity has claim create rights in a sandbox namespace.
