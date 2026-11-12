# 0004. Human identity: generic OIDC with roles in Postgres

- Status: Accepted; amended 2026-10-23, 2026-10-26 and 2026-10-30 (below); platform admin audit moved by [0100](0100-install-audit-chain.md) (note of 2026-11-07 below)
- Date: 2026-10-21
- Decision: [design.md#4](../design.md)

## Context

Organizations use their identity provider (IdP) to very different degrees:
- some manage every team as an IdP group;
- others only use the IdP for login and want to manage membership in the product.

Koinon must work with any OIDC provider (Entra ID, Okta, Keycloak, Dex, Google) and must not be
locked out when IdP groups are misconfigured.

## Decision

- Login is generic OIDC. Memberships and roles live in Postgres.
- Each org chooses a membership mode:
  - `platform`: roles are managed in Koinon only.
  - `idp`: roles come from IdP group mappings.
  - `hybrid`: both apply, and the highest role wins.
- Every org has at least one manually assigned owner, so an IdP change cannot orphan it.
- IdP groups are synced at login. SCIM provisioning comes later.
- `platform-admin` is a role above all orgs.

## Alternatives considered

- **IdP groups only** _(reconstructed)_. Simplest, but forces every team to manage membership in
  the IdP, and lockouts become IdP tickets.
- **Koinon-managed users only (no IdP groups)** _(reconstructed)_. Ignores the investment large
  organizations have in IdP groups.
- **SCIM from day one** _(reconstructed)_. Accurate offboarding, but more to build and not every IdP
  offers it. Deferred.

## Consequences

- Positive: works for small teams and for large IdP-driven organizations.
- Positive: the manual-owner rule prevents orphaned orgs.
- Negative: sync at login means a user removed from an IdP group keeps their role until their next
  login or until the session expires. Session lifetime must be bounded _(inferred)_.
- Negative: in `hybrid` mode, "highest role wins" means an IdP misconfiguration can only raise
  access, never lower it. Admins must understand this _(inferred)_.
- Follow-ups: the backend-for-frontend login flow ([0022](0022-frontend-architecture.md)) and the mapping of
  `platform-admin` to OpenShell's Platform Admin role for the gateway ([0016](0016-harness-and-deployment.md))
  _(inferred)_.

## Amendment (2026-10-23): org roles

An org has three roles: **owner**, **admin** and **member** _(Changed: there is a fourth role, viewer; see the amendment of 2026-10-30 below)_. Members use agents and create tickets,
and see approvals **read-only**; admins and owners decide them, as in
[0049](0049-v01-scope-decisions.md). A viewer role is not part of this decision.

## Amendment (2026-10-26): platform admins from IdP groups

`oidc.platformAdminGroups` lists IdP groups whose members are platform admins. It is reconciled at
every login like the default-org group mappings: a user who gains a listed group is granted, one who
loses all of them is revoked. `platform_admins.source` records where a grant came from (`bootstrap` or
`oidc_group`), and the login revokes only `oidc_group` rows, so the bootstrap admin and any grant from
another source stay. Grants and revocations are audited as `platform_admin.grant` and
`platform_admin.revoke` (actor `system oidc`), in the user's personal org because audit rows need an
org. An empty setting changes nothing, so removing the setting does not revoke earlier grants.
Operators set it and log in once before they disable the bootstrap admin.

**Note (2026-11-07).** [ADR 0100](0100-install-audit-chain.md) (Accepted 2026-11-07) moves `platform_admin.grant`
and `platform_admin.revoke` out of the user's personal org into the install-level audit chain, renames
the actor `system koinon-api:oidc-sync`, and adds events for the bootstrap admin's creation and for each
bootstrap re-grant that inserts a row. The text above is left as accepted.

## Open questions

- Session lifetime and re-validation interval, so that group removal takes effect in bounded time.
- Does v0.1 need OIDC at all, or a single bootstrap admin with OIDC added later in v0.1? This
  affects the 15-minute quickstart ([0036](0036-v01-demo.md)).

## Upstream verification

- 2026-10-21: the OpenShell gateway supports OIDC with a roles claim (admin and user roles) and mTLS
  (the default, with the certificate subject mapped to a user). Koinon's control plane talks to the
  gateway as a service identity, not as end users. https://github.com/NVIDIA/OpenShell
  (docs/how-it-works/gateways/authentication.mdx)

## Links

- [0001](0001-full-multi-tenancy.md), [0019](0019-personal-agents-and-teams.md),
  [0022](0022-frontend-architecture.md)

## Amendment (2026-10-30): the viewer role

Orgs have four roles, viewer < member < admin < owner: the viewer role was in the data model from the
scaffold ([0047](0047-repository-scaffold.md) decision 4, migration `00001_initial.sql`, commit eded3ae7),
which the 2026-10-23 amendment above did not record. A viewer reads the org's work but cannot answer
questions or decide approvals. "Org admins" means everyone with the admin or owner role.
