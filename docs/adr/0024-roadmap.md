# 0024. Release roadmap from v0.1 to v1.0

- Status: Accepted
- Amended: 2026-10-26 (releases from old issue 146)
- Date: 2026-10-21
- Area: product-scope
- Tags: roadmap, release
- Decision: [design.md#24](../design.md)

## Context

Koinon has many parts: execution, connectors, approvals, A2A, tenancy, MCP, browser, cross-org
sharing. Shipping them together would delay any usable release for a long time; shipping them in
the wrong order would force rewrites, especially tenancy, which is expensive to retrofit (ADR
[0001](0001-full-multi-tenancy.md)). Each release should be installable and demo-able on its own.

Grilling on 2026-10-21 moved some work earlier: the GitHub connector, a minimal agentgateway-based
MCP gateway and the approval flow are needed for the v0.1 demo (ADR
[0036](0036-v01-demo.md), [0042](0042-ask-tier-park-and-execute.md)), and in-sandbox credentials
land in v0.2 (ADR [0044](0044-in-sandbox-credential-exception.md)).

## Decision

| Release | Theme | Includes |
| --- | --- | --- |
| v0.1 | Run an agent | Helm install; agent from template; message it; live run view; one approved GitHub action; agentgateway for models and the GitHub MCP tool; `org_id` everywhere |
| v0.2 | Connectors | connector framework, risk tiers and presets, routines (crons), webhooks, Slack/Teams mirroring, in-sandbox credential exception (ADR 0044) |
| v0.3 | Collaboration | A2A, parking, agent builder |
| v0.4 | Multi-tenancy | full org model, IdP modes, personal orgs at scale |
| v0.5 | Tools | shared MCP servers, browser, self-installed tools |
| v1.0 | Stable | cross-org links, security review, stable APIs (CRDs leave `v1alpha1`) |

- `org_id` is in the data model from v0.1, even though full multi-tenancy lands in v0.4.
- ADRs [0036](0036-v01-demo.md) and [0037](0037-tooling.md) give the v0.1 detail.

## Alternatives considered

- Tenancy first (v0.1 = orgs) _(reconstructed)_. Rejected: delays the first useful demo; `org_id`
  from day one keeps the data model honest without building the whole org UI.
- A2A before connectors _(reconstructed)_. Rejected: agents without external connectors have little
  to collaborate on.
- One big v1.0 _(reconstructed)_. Rejected: no feedback until very late.

## Consequences

- Positive: each release is a demo; risk retires early (execution and approvals in v0.1).
- Negative: v0.1 to v0.3 run with single-org UI paths while the data model is multi-org; tests must
  cover org isolation from v0.1 anyway _(inferred)_.
- Negative: the v0.1 scope grew (GitHub connector, approval flow, agentgateway) compared with the
  original roadmap line _(inferred)_.
- Follow-ups: v0.1 milestone with issues per component (bootstrap deliverable 4,
  old issue 4).

## Open questions

- Which UI parts ship in v0.1 (board, ticket view, agent from template, approval card).
- Whether to move to a `koinon` GitHub org before v0.1 (ADR [0034](0034-project-identity.md)).

## Upstream verification

None needed. Checked 2026-10-21.

## Amendment (2026-10-26): v0.1 to v0.3 replaced by the releases of old issue 146

procoduck set three releases on 2026-10-26 (old issue 146).
They replace the v0.1, v0.2 and v0.3 rows above; v0.4 onwards is unchanged.

| Release | Theme | Includes |
| --- | --- | --- |
| v0.1 | Foundation | the code-host interface: GitHub, GitHub Enterprise Server and Gitea ([0062](0062-code-host-connectors.md), [0063](0063-forge-credential-classes.md)); A2A-shaped tasks, the inbox, `ask_user`, DBOS ([0065](0065-work-model-tasks-inbox.md), [0066](0066-triggers-and-scheduling.md)); schedule and direct-message triggers; home checkpoints ([0032](0032-agent-home-checkpoints.md)). Tested on the home lab with Gitea through the daily FinOps cost check (without the hand-off to the lead-engineer agent), a long-running research task, and the weekly LCM upgrade agent |
| v0.2 | Events and channels | code-host and Slack/Teams events (webhooks, Socket Mode, polling, correlation); the Slack and Teams connectors, with approval buttons and account linking; `ask_agent`. Stories: PR review on a new PR; FinOps asking the lead-engineer agent; the daily stand-up to a channel; @mentions in a channel; the support-channel ops agent |
| v0.3 | (not named) | the concierge and setup checklist ([0067](0067-concierge.md)); tracker connectors (Jira, Linear, Azure Boards, and GitHub, Gitea and GitLab issues) with their triggers ([0064](0064-work-tracker-connectors.md)); the personal agent ([#11](https://github.com/stateracloud/koinon/issues/11)) |

The work moved to v0.2 on 2026-10-23 stays in v0.2: chat
([0055](0055-chat-as-its-own-object.md)), container MCP servers ([0057](0057-mcp-servers.md)), and
the shared gateway driver with REL2 run B ([0058](0058-gateway-modes.md)). Items of the old rows
that old issue 146 does not place, whose release is open: the in-sandbox credential exception
([0044](0044-in-sandbox-credential-exception.md)) and the agent builder
([0020](0020-agent-builder-and-templates.md)). The external A2A gateway is v1.0
([0065](0065-work-model-tasks-inbox.md)).

## Links

- [design.md](../design.md), decisions 24, 36 and 37
- ADR [0036](0036-v01-demo.md), [0040](0040-agentgateway-model-and-mcp-gateway.md),
  [0042](0042-ask-tier-park-and-execute.md), [0044](0044-in-sandbox-credential-exception.md)
