# 0042. Ask tier: park the run, Koinon executes the approved call

- Status: Accepted; amended by [0048](0048-openshell-spike-results.md) and [0065](0065-work-model-tasks-inbox.md)
- Date: 2026-10-21
- Amends: ADR 0008 (approvals)
- Verification: Pending: agentgateway spike (routing ask-tier tools) and OpenShell spike ([#3](https://github.com/stateracloud/koinon/issues/3))

## Context

Decision 8 compiles each grant to OpenShell network policy plus MCP gateway tool rules, and requires
approvals to be bound to the exact request with the run parked while waiting. Research on
2026-10-21 found:

- **OpenShell has no ask tier.** A denied request stays denied. Approval in OpenShell applies to a
  *proposed rule* (policy advisor, `rule approve`, RFC 0002 agent-driven proposals), scoped to
  binary, host, port and, for HTTP, method and path. It cannot hold one request while a human decides
  or bind approval to a request body.
- OpenShell L7 endpoints default to `enforcement: audit`, which logs and allows (ADR 0045 fixes the
  default).
- agentgateway (ADR 0040) can authorize MCP calls with CEL on tool name and arguments, and its
  ExtMCP gRPC callout can allow, modify or deny a call. Holding a call open for human approval is not
  documented; gRPC ext_authz defaults to a 2 s timeout.

The v0.1 demo (decision 36) ends with "approve one GitHub action", so this path is in v0.1.

## Decision

1. Scopes in the **ask tier** (external or irreversible) are exposed to agents **only as MCP tools**
   through agentgateway. OpenShell network policy denies the corresponding direct write APIs from the
   sandbox (for example GitHub `POST /repos/*/pulls`).
2. When an agent calls an ask-tier tool, the call is routed to Koinon's **approval service**, which
   records the exact tool name, arguments and target, creates an approval card on the ticket, and
   **returns immediately** with a structured "approval requested" result.
3. The run **parks** (decision 30). No connection is held open.
4. After approval (per-grant approver policy, optional two-person rule), **Koinon executes exactly
   the approved call** itself, outside the sandbox, with an idempotency key (decision 29), using the
   connector's credentials.
5. Koinon then **resumes the harness session** (ACP `session/load` or `session/resume`) with the
   call's result injected as the tool outcome. On rejection, it resumes with a rejection result.
6. The approval is therefore bound to the exact request by construction: what runs is what was
   approved.

```mermaid
sequenceDiagram
  participant A as Agent (harness)
  participant G as agentgateway
  participant K as Koinon approval service
  participant H as Human approver
  participant X as External system (GitHub)
  A->>G: tools/call create_pull_request(args)
  G->>K: route ask-tier tool (CEL / ExtMCP / Koinon MCP server)
  K->>K: store exact call + idempotency key, approval card
  K-->>A: result: approval requested (ticket waiting on you)
  K->>K: park run
  H->>K: approve exact call
  K->>X: execute approved call (idempotency key)
  X-->>K: result
  K->>A: resume session with tool result
```

## Alternatives considered

- **Hold the call in ExtMCP until a human decides**: simplest for the harness, but approvals take
  minutes to days; the call would need long timeouts and would break on gateway restarts or sandbox
  suspension.
- **Deny, then a one-shot OpenShell rule**: the supervisor denies, Koinon turns the denial into an
  approval card, approval adds a temporary narrow rule and the agent retries. No new component, but
  approval binds only to method and path, not the request body, and the retry may differ from what
  was approved.
- **Contribute a hold-for-approval feature to OpenShell**: worth raising upstream (RFC 0002 defers
  its inbox), but not something to block v0.1 on. _(reconstructed)_

## Consequences

- Positive: approvals bind to the exact request; no long-lived connections; works across sandbox
  suspension.
- Positive: real connector credentials for ask-tier actions never enter the sandbox at all.
- Negative: v0.1 needs agentgateway, a GitHub MCP tool set, the approval service and session resume
  with an injected tool result. This grows v0.1 scope.
- Negative: each ask-tier action needs a Koinon-side executor per connector (for v0.1: GitHub).
- Negative: harnesses must cope with a tool result that arrives in a later turn; flavor contract
  tests must cover it. _(inferred)_
- Follow-ups: approval service; GitHub ask-tier executors; tool-result injection on resume in
  `agentd`; deny rules for direct write APIs in rendered policies.

## Open questions

- Routing mechanism in agentgateway: CEL rule to a Koinon MCP server, ExtMCP callout, or both.
- How each harness represents "approval requested" and the later result (ACP `session/load` replays
  history; pi-acp has no `session/resume`).
- Approval expiry and what happens to a parked run when an approval expires.

## Upstream verification

Checked 2026-10-21: OpenShell policy docs (`docs/how-it-works/policies/schema.mdx`, default
`enforcement: audit`), RFC 0002 (accepted; agent proposals behind
`agent_policy_proposals_enabled`, default false); agentgateway MCP authorization and ExtMCP docs.

## Amendment (2026-10-26, #146 A4): safe defaults and scoped approvals

[0065](0065-work-model-tasks-inbox.md) changes two things:

- **Every question names a safe default** (`ask_user`, and approvals), used when it is dismissed
  or expires; the agent is told which default applied.
- **Approvals can be scoped:** once (this ADR's exact-request approval), this task, or this agent
  and this kind of action until a date. A scope wider than once needs an admin, shows in the
  agent's grants, and is audited.

## Links

- https://github.com/NVIDIA/OpenShell/blob/main/docs/how-it-works/policies/schema.mdx
- https://agentgateway.dev/docs/standalone/main/configuration/security/mcp-authz/
- https://agentgateway.dev/docs/standalone/main/mcp/guardrails/about/
- https://agentclientprotocol.com/protocol/session-setup
- ADR 0008, ADR 0029, ADR 0030, ADR 0036, ADR 0040, ADR 0045
- Design adapted from [hydro-project/infinity](https://github.com/hydro-project/infinity) (Apache-2.0, commit 341f6c2): the safe default choice and approval scopes (`rap-protocol/src/lib.rs:152-163`; `sandbox-core/src/server.rs:864-945`). Research notes: `infinity-code-ideas.md`.
