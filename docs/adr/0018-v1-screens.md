# 0018. v1 screens and automatic personal orgs

- Status: Accepted
- Amended by: [0055](0055-chat-as-its-own-object.md), [0065](0065-work-model-tasks-inbox.md), [0066](0066-triggers-and-scheduling.md)
- Date: 2026-10-21
- Area: frontend-ux
- Tags: ui, multi-tenancy, tasks, triggers
- Decision: [design.md#18](../design.md)

## Context

Koinon is multi-tenant from the start (ADR [0001](0001-full-multi-tenancy.md)), and personal
agents are part of the product (ADR [0019](0019-personal-agents-and-teams.md)). If personal agents
were a special case, every feature would need a second code path: grants, budgets, audit, memory.
The UI must also stay small enough to build and keep consistent across releases.

## Decision

The v1.0 UI has nine screens:

| Screen | Purpose |
| --- | --- |
| Board | Home; tickets by column, including `waiting on you` (ADR [0017](0017-board-and-chat-interaction-model.md)) |
| Ticket | Conversation, runs, live view, approvals, artifacts, cost |
| Agents | Org's agents, status, current run, budget use |
| Agent editor | Flavor, template, grants, memory, budget, schedule; draft vs active (ADR [0020](0020-agent-builder-and-templates.md)) |
| Connections | Connector cards per org, setup guides, validation (ADR [0021](0021-connector-setup-guides.md)) |
| MCP servers | Shared MCP servers deployed for the org, and their grants |
| Org settings | Members, roles, IdP mode, links, budgets |
| Platform admin | Orgs, connector types, allowed auth types, global policy |
| Audit | Hash-chained audit log, filterable (ADR [0023](0023-observability-and-audit.md)) |

Every user gets an automatic personal org at first login. A personal org is an ordinary org with one
owner; the same screens, grants and budgets apply.

## Alternatives considered

- Personal agents as a user-level object outside orgs _(reconstructed)_. Rejected: duplicates the
  grant, budget and audit model.
- A separate admin console application _(reconstructed)_. Rejected: one SPA with role-gated screens
  is less to build and ship (ADR [0022](0022-frontend-architecture.md)).
- Fewer screens with configuration inside the Agents screen _(reconstructed)_. Rejected: Connections
  and MCP servers are shared across agents and have their own lifecycle.

## Consequences

- Positive: personal agents need no special code path; features land once.
- Positive: a fixed screen list bounds the v1 UI scope.
- Negative: every user creates an org row and namespace mapping on first login; namespace creation
  should be lazy until the first agent exists, to avoid empty namespaces _(inferred)_.
- Negative: the screen list grows the API surface in step; buf breaking checks keep it stable
  (ADR [0027](0027-repo-and-versioning.md)).
- Follow-ups: v0.1 ships a subset (board, ticket view, agent from template, approval card), to be
  fixed in the v0.1 milestone plan (ADR [0024](0024-roadmap.md), [0036](0036-v01-demo.md)).

## Open questions

- Which screens, and which parts of them, ship in v0.1 (listed as still open in BOOTSTRAP.md).
- Whether personal org namespaces are created eagerly at login or lazily at first agent.

## Upstream verification

None needed. Checked 2026-10-21.

## Amendment (2026-10-26): the inbox is home; triggers on the agent editor

- **Home.** The home screen is the inbox ("Waiting on you"), with chat next to it; the board is a
  secondary view over tasks, and the Ticket screen shows a task ([0065](0065-work-model-tasks-inbox.md)).
- **Agent editor.** "Schedule" becomes triggers: schedules, code-host events, chat events and
  direct messages, each with a filter and an instruction, in the versioned agent config
  ([0066](0066-triggers-and-scheduling.md)).

## Links

- [design.md](../design.md), decisions 18 and 24
- ADR [0001](0001-full-multi-tenancy.md), [0017](0017-board-and-chat-interaction-model.md),
  [0019](0019-personal-agents-and-teams.md), [0022](0022-frontend-architecture.md)
