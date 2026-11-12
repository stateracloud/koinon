# 0001. Full multi-tenancy from the start

- Status: Accepted
- Date: 2026-10-21
- Area: tenancy-identity
- Tags: multi-tenancy
- Decision: [design.md#1](../design.md)

## Context

Koinon is an org-and-work layer for AI agents. Different teams and individual users want their own
agents, connector cards and secrets, and they want to decide for themselves who may use their
agents. Adding tenancy to a single-tenant system after the fact touches every table, every API,
every authorization check and every rendered Kubernetes object. The design calls this "the
expensive mistake".

The execution layer we reuse, NVIDIA OpenShell, already has a tenancy concept: workspaces (RFC
0011). Its roles are Platform Admin, Workspace Admin and User, and in `managed` mode each workspace
gets its own namespace (see [0002](0002-namespace-per-org-isolation.md)).

## Decision

- Koinon is fully multi-tenant. The tenant is an **org**.
- A team or a single user gets an org and sets up its own agents, connector cards and secrets inside
  it. Every user also gets an automatic personal org ([0018](0018-v1-screens.md)).
- Using an agent from another org is opt-in for the org that owns the agent. The default is private
  ([0012](0012-cross-org-sharing.md)).
- Every tenant-owned record carries an `org_id` from v0.1, even though the multi-tenancy UI ships in
  v0.4 ([0024](0024-roadmap.md)).

## Alternatives considered

- **Single tenant per install, one install per team** _(reconstructed)_. Simple, but it multiplies
  the operational cost (one control plane, Postgres and OpenShell gateway per team) and makes
  cross-team agent use a federation problem.
- **Teams as labels inside one tenant** _(reconstructed)_. Cheap at first, but secrets, budgets and
  approvals still need hard boundaries, so this leads to retrofitting tenancy anyway.
- **Kubernetes namespaces as the only tenancy model** _(reconstructed)_. Rejected in
  [0003](0003-postgres-source-of-truth.md): self-service authorization and tickets do not fit
  Kubernetes RBAC and etcd.

## Consequences

- Positive: personal agents, team agents and cross-org links all use one code path.
- Positive: the data model is ready for tenancy before the v0.4 UI exists.
- Negative: every query, API handler and reconciler path must be org-scoped and tested for leaks
  between orgs _(inferred)_.
- Negative: v0.1 carries some tenancy overhead (org IDs, org-scoped keys) before it needs them.
- Follow-ups: an org-isolation test suite (an org A user can never read org B data) in the API test
  layer _(inferred)_.

## Open questions

- Should Postgres row-level security back up the org scoping in the application, or is
  application-level enforcement plus tests enough for v0.x? _(inferred)_
- How are orgs created in v0.1: platform admin only, or self-service?

## Upstream verification

- 2026-10-21: OpenShell workspaces (RFC 0011) are status `draft` but already implemented and
  documented, with the roles Platform Admin, Workspace Admin and User.
  https://github.com/NVIDIA/OpenShell (docs/kubernetes/workspaces.mdx)

## Links

- [0002](0002-namespace-per-org-isolation.md), [0004](0004-human-identity.md),
  [0012](0012-cross-org-sharing.md), [0019](0019-personal-agents-and-teams.md),
  [0024](0024-roadmap.md)
