# 0002. One namespace per org, mapped to one OpenShell workspace

- Status: Accepted
- Date: 2026-10-21
- Area: runtime-sandbox
- Tags: multi-tenancy, openshell, kubernetes, isolation
- Decision: [design.md#2](../design.md)

## Context

Orgs ([0001](0001-full-multi-tenancy.md)) need a boundary in Kubernetes for quotas, RBAC and secret
references. The real security boundary for an agent is not the namespace. It is the OpenShell
supervisor (Landlock, seccomp, the egress proxy) inside a Kata VM. Namespaces are still the natural
unit for ResourceQuota, LimitRange, RBAC, NetworkPolicy and per-tenant node placement.

OpenShell's Kubernetes driver supports three workspace modes:
- `shared`, the default;
- `managed`, where each workspace gets a namespace `openshell-{gateway_id}-{workspace}`, created on
  first use;
- `operator`, where namespaces are pre-provisioned and allowlisted.

## Decision

- Each org gets one namespace, which maps to one OpenShell workspace, using
  `workspaceMode: managed`.
- Orgs can optionally get dedicated NodePools (node pools or node selectors and taints) for
  stronger placement isolation.
- Placement stays abstract in Koinon's model (an org has a placement profile), so the mechanism
  (NodePool, node selector, Karpenter, a separate cluster) can change.
- The namespace scopes quotas, RBAC and secret references. The supervisor and the Kata VM are the
  isolation boundary.

## Alternatives considered

- **Namespace per agent** _(reconstructed)_. Stronger blast-radius limits, but it multiplies
  namespaces and makes org-wide quotas and RBAC awkward. OpenShell workspaces map to orgs, not
  agents.
- **OpenShell `shared` mode with labels** _(reconstructed)_. Less to provision, but quotas and RBAC
  could no longer be scoped per org.
- **OpenShell `operator` mode** _(reconstructed)_. Koinon pre-creates the namespaces. Kept as an
  option if `managed` naming or lifecycle does not fit. See Open questions.
- **Cluster per org** _(reconstructed)_. Maximal isolation, but heavy. Could be offered later
  through the abstract placement profile.

## Consequences

- Positive: per-org quotas and RBAC use standard Kubernetes objects.
- Positive: the mapping follows OpenShell's own tenancy model, which eases alignment with RFC 0011.
- Negative: in `managed` mode the namespace name is chosen by OpenShell
  (`openshell-{gateway_id}-{workspace}`), so Koinon must store the mapping, not derive it
  _(inferred)_.
- Negative: dedicated NodePools add cost and scheduling complexity for orgs that opt in.
- Follow-ups: the ValidatingAdmissionPolicy on sandbox namespaces
  ([0045](0045-openshell-hardening-defaults.md)) selects namespaces by OpenShell-managed labels
  _(inferred)_.

## Open questions

- `managed` or `operator` mode? `operator` lets Koinon own namespace names and labels; `managed` is
  less code. The spike should confirm that `managed` works with Koinon's quota and RBAC rendering.
- Which labels does OpenShell put on managed namespaces, so that the VAP and quotas can select them?

## Upstream verification

- 2026-10-21: the OpenShell Helm key is `server.drivers.kubernetes.workspaceMode`; the modes are
  `shared` (default), `managed` and `operator`; managed namespaces are named
  `openshell-{gateway_id}-{workspace}`. https://github.com/NVIDIA/OpenShell
  (docs/kubernetes/workspaces.mdx)

## Links

- [0001](0001-full-multi-tenancy.md), [0016](0016-harness-and-deployment.md),
  [0045](0045-openshell-hardening-defaults.md)
