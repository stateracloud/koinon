# 0005. Agents act as themselves

- Status: Accepted
- Amended by: [0067](0067-concierge.md)
- Date: 2026-10-21
- Area: tenancy-identity
- Tags: identity, audit
- Decision: [design.md#5](../design.md)

## Context

An agent run can start from a human message, a cron, a webhook or another agent. The question is
whose identity the agent uses toward GitHub, Slack, clouds and so on.

Acting on behalf of the requesting user has three problems:
- crons and webhooks have no user;
- delegated user tokens expire, and refresh tokens are sensitive;
- permissions then depend on whoever happened to ask, not on what the agent was designed and
  approved to do.

## Decision

- Agents act as themselves, with their own identity and their own grants
  ([0006](0006-identity-layers.md), [0007](0007-connector-model.md)).
- Every run records `requested_by` (the human, agent, routine or webhook that caused it) and
  `approved_by` (the approvers of any ask-tier actions).
- These are added to pull requests, commits (for example as trailers) and audit records, so humans
  stay accountable and visible.
- On-behalf-of access is available only as a per-connector opt-in (connector auth type
  `delegated-user`, [0006](0006-identity-layers.md)).

## Alternatives considered

- **Always on-behalf-of the requesting user** _(reconstructed)_. Familiar from Copilot-style tools,
  but it fails for crons, makes permissions unpredictable, and needs user refresh tokens stored
  long-term.
- **Shared service account per org** _(reconstructed)_. Simple, but one agent's compromise or
  mistake affects every agent, and audit cannot tell agents apart.

## Consequences

- Positive: permissions are designed per agent and stay stable no matter who asks.
- Positive: crons, webhooks and A2A calls work without a human token.
- Negative: external systems see the agent identity (for example a GitHub App bot), so per-user
  attribution in those systems depends on Koinon writing `requested_by`/`approved_by` into PRs,
  commits and comments _(inferred)_.
- Negative: an agent may hold more access than the requesting user. This is mitigated by grants,
  risk tiers ([0008](0008-approvals-and-risk-tiers.md)) and, for personal agents, by capping access
  at the user's role ([0019](0019-personal-agents-and-teams.md)).
- Follow-ups: a standard trailer format for commits and PR bodies (`Requested-by:`, `Approved-by:`)
  and a matching audit schema _(inferred)_.

## Open questions

- Exact trailer and PR-body format, and whether it includes Koinon ticket and run links.
- Which connectors offer `delegated-user` first? Probably none in v0.1, since the GitHub connector
  uses a GitHub App.

## Upstream verification

- 2026-10-21: GitHub App installation tokens expire after one hour and can be narrowed per request
  by repositories and permissions, which fits per-agent, least-privilege identity.
  https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app

## Amendment (2026-10-26): the concierge acts as the user

[0067](0067-concierge.md) makes one explicit exception. The concierge, the setup and management
assistant built into koinon-api, acts as the logged-in user in the current org, capped at their
role. It uses the same RPCs and audit as the UI, and audit records "via concierge". It runs no code
and has no sandbox. Every other agent still acts as itself.

## Links

- [0006](0006-identity-layers.md), [0007](0007-connector-model.md),
  [0008](0008-approvals-and-risk-tiers.md), [0023](0023-observability-and-audit.md)
