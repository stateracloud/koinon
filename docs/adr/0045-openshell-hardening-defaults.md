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
