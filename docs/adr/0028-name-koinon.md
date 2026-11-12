# 0028. Name the project Koinon

- Status: Accepted
- Date: 2026-10-21
- Area: product-scope
- Tags: api, registry, helm-chart
- Decision: [design.md#28](../design.md)

## Context

The project needs a name before the repository, Go module, API groups and domain are created
([0034](0034-project-identity.md)). The name ends up in many places that are expensive to change:

- the module path
- CRD API groups
- Helm chart names
- container image names
- the docs site

The product idea is a federation of independent organisations (orgs) that keep their own agents,
connectors and secrets, and cooperate through explicit links
([0001](0001-full-multi-tenancy.md), [0012](0012-cross-org-sharing.md)). A *koinon* was a league of
independent Greek city-states that kept their autonomy while acting together, which matches that
model.

## Decision

- The project is called **Koinon**, pronounced **KOY-non**.
- The README carries a pronunciation line.
- Written forms:
  - "Koinon" in prose.
  - `koinon` in identifiers: module path, binaries (`koinon-api`, `koinon-reconciler`), Helm chart
    `koinon` and image names.
  - `koinon.dev` as the domain and the root of CRD API groups.

## Alternatives considered

- A descriptive name such as "agent-platform" or "agentorg" _(reconstructed)_: easier to guess, but
  generic, hard to search for, and likely to clash with existing projects.
- A name that reuses the OpenShell or Agent Sandbox brands _(reconstructed)_: would suggest an
  official affiliation that does not exist.

## Consequences

- Positive:
  - A short, distinctive name that searches well.
  - The metaphor (autonomous members, opt-in cooperation) explains the tenancy model in one
    sentence.
- Negative:
  - Pronunciation isn't obvious _(inferred)_; the README's pronunciation line addresses this.
  - The name has not yet been checked for trademark conflicts or for availability of the `koinon`
    GitHub org.
- Follow-ups:
  - Check availability of the `koinon` GitHub org, the `koinon.dev` domain and trademarks (open item
    in design.md; tracked as [#7](https://github.com/stateracloud/koinon/issues/7)).

## Open questions

- Trademark search result, and whether it forces a rename before v0.1 (tracked in
  [#7](https://github.com/stateracloud/koinon/issues/7)).

## Upstream verification

None needed; this is a naming decision. The availability checks are tracked separately.

## Links

- [0034 Project identity](0034-project-identity.md)
- [#7 Check availability: koinon GitHub org, koinon.dev domain, trademarks](https://github.com/stateracloud/koinon/issues/7)
