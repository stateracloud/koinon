# 0012. Cross-org sharing through links with exposure profiles

- Status: Accepted; who pays amended by [0065](0065-work-model-tasks-inbox.md)
- Date: 2026-10-21
- Decision: [design.md#12](../design.md)

## Context

Orgs are independent ([0001](0001-full-multi-tenancy.md)), but they should cooperate. The platform
team's agent can help other teams, like the leagues of Greek city-states the project is named
after ([0028](0028-name-koinon.md)). Letting another org use your agent risks the **confused deputy**
problem: the consumer uses the provider agent's grants to do something the consumer itself is not
allowed to do.

## Decision

- An agent's visibility is one of:
  - `private` (the default);
  - `shared` on request;
  - `public`, which only a platform admin can set.
- A cross-org **link** carries:
  - an exposure profile, meaning reduced grants for linked callers;
  - a rate limit;
  - a budget.
- The consumer org pays.
  _(Changed by [0065](0065-work-model-tasks-inbox.md): the called agent spends its own budget and uses its own grants. The link's budget is a cap on the calls it admits, not who pays.)_
- Calls run in the provider org. Approvals go to the provider org's approvers.

## Alternatives considered

- **Copy the agent into the consumer org** _(reconstructed)_. No confused deputy, but the expertise
  (memory, org knowledge) and the connections stay behind.
- **Run calls in the consumer org with the provider's prompts** _(reconstructed)_. The consumer
  could not use the provider's connections, so this defeats the purpose.
- **Full access for linked orgs** _(reconstructed)_. This is the confused-deputy risk itself.

## Consequences

- Positive: expertise is shared without sharing credentials.
- Positive: cost lands with the consumer, so sharing is not a burden on the provider.
- Negative: provider approvers can receive requests from other orgs, so approval cards must show
  the consumer org and the requester clearly _(inferred)_.
- Negative: exposure profiles add another layer to compile into policy and MCP rules _(inferred)_.
- Follow-ups: cross-org sharing ships in v1.0 ([0024](0024-roadmap.md)); links depend on A2A
  ([0011](0011-agent-to-agent.md)).

## Open questions

- How are exposure profiles enforced at the boundary when one sandbox serves both own-org and
  linked calls? The run's effective grants must be recomputed per run, which matters because
  OpenShell network policies are mutable at runtime only in `network_policies` _(inferred)_.
- The billing model when a linked call spawns further A2A calls (does the consumer pay for the
  whole chain?).

## Upstream verification

- None specific. Relies on [0011](0011-agent-to-agent.md) and
  [0041](0041-koinon-spend-ledger-and-budget-stops.md).

## Links

- [0001](0001-full-multi-tenancy.md), [0011](0011-agent-to-agent.md),
  [0019](0019-personal-agents-and-teams.md), [0041](0041-koinon-spend-ledger-and-budget-stops.md)
