# 0033. File-based, scoped memory stores

- Status: Accepted
- Date: 2026-10-21
- Area: runtime-sandbox
- Tags: memory, agent-home
- Decision: [design.md#33](../design.md)

## Context

Agents need memory across runs. Agent memory as such is a solved problem: harnesses already read
instruction files (`AGENTS.md`, `CLAUDE.md`), and several projects add memory, either through MCP
or as git-backed files (Hindsight, Mem0, Letta MemFS).

What Koinon adds is **scoping and audit**:

- what an agent may remember for itself
- what an org knows and shares
- what belongs to a user's personal agents

Plus who changed what, and when. That has to work across all v0.1 harnesses
([0035](0035-v01-flavors.md)).

## Decision

- **Three kinds of store**, mounted into the sandbox:
  - **Agent store:** read-write, private to the agent.
  - **Org knowledge:** read-only to agents. Agents submit *proposals* (a change request), and an org
    admin approves them.
  - **User store:** for personal agents ([0019](0019-personal-agents-and-teams.md)), scoped to the
    user.
- **Each store is a git repository**, giving history, diffs and audit. Agent commits are attributed
  to the agent and run; human approvals record `approved_by` ([0005](0005-agent-identity.md)).
- **Harness wiring:** harnesses find the stores through `AGENTS.md` and `CLAUDE.md` in the
  workspace, which Koinon generates per flavor and which name the store paths and how to use them.
- **Optional memory servers:** Hindsight (MIT) or Mem0 (Apache-2.0) can be added as MCP memory
  servers outside the sandbox, through the MCP gateway
  ([0040](0040-agentgateway-model-and-mcp-gateway.md)).
- **Checkpoints:** stores on the agent home are included in home checkpoints
  ([0032](0032-agent-home-checkpoints.md)).

## Alternatives considered

- **Vector-database memory as the default** (Mem0, Hindsight) _(reconstructed)_: better recall at
  scale, but opaque to humans and harder to audit and review. Kept as optional.
- **Letta / MemFS as the memory engine** _(reconstructed)_: git-backed markdown is the same idea,
  but it is tied to Letta Code and the Letta API, and not available on a Docker server.
- **Memory in Postgres via a Koinon API** _(reconstructed)_: auditable, but every harness would
  need a custom tool, which loses the "harnesses already read files" advantage.

## Consequences

- Positive:
  - Works with every harness that reads instruction files.
  - Humans can read and diff memory.
  - Org knowledge changes are reviewed like code.
- Negative:
  - Generated instruction files differ per harness (see "Upstream verification") _(inferred)_.
  - Git repos on the home volume need garbage collection and size limits _(inferred)_.
  - Proposal review adds admin work.
- Follow-ups:
  - A per-flavor generator for `AGENTS.md` / `CLAUDE.md` / `.hermes.md` that respects each
    harness's lookup order.
  - Where org knowledge is hosted (a read-only mount from a Koinon-managed repo) and its sync.
  - Proposal flow as a ticket type in the "waiting on you" column ([0017](0017-board-and-chat-interaction-model.md)).

## Open questions

- Is memory in v0.1 scope, or is the agent store alone (no org knowledge) enough for the demo?
- How are org knowledge repos delivered into sandboxes: mount, clone at start, or a git server
  behind the supervisor?
- Size quotas per store.

## Upstream verification

Checked 2026-10-21:

- **OpenCode:** reads `AGENTS.md`, falling back to `CLAUDE.md` and `~/.claude/CLAUDE.md`. AGENTS.md
  rules apply over ACP. Rules docs in https://github.com/anomalyco/opencode
- **Hermes:** loads only the first project file it finds, in the order `.hermes.md`, `AGENTS.md`,
  `CLAUDE.md`, `.cursorrules`. `skip_context_files` turns this off. Whether ACP mode loads them is
  unverified (likely). https://hermes-agent.nousresearch.com/docs/user-guide/features/context-files
- **pi:** loads `AGENTS.md` and `CLAUDE.md` (plus `AGENTS.override.md`) from the agent directory,
  the working directory and parent directories. `packages/coding-agent/docs/configuration.md` in
  https://github.com/earendil-works/pi
- **Hindsight:** MIT, per-bank MCP endpoint `/mcp/{bank_id}/`. https://github.com/vectorize-io/hindsight
- **Mem0:** Apache-2.0. https://github.com/mem0ai/mem0
- **Letta MemFS:** git-backed markdown in Letta Code; not on Docker servers. https://docs.letta.com/letta-code/memfs

- **Amendment (2026-10-27, EP1):** an ephemeral agent ([0065](0065-work-model-tasks-inbox.md) decision 13c) has no home; its memory is the task's notes and these stores, which are mounted into each task's fresh sandbox.

## Links

- [0019 Personal agents and teams](0019-personal-agents-and-teams.md), [0032 Agent home](0032-agent-home-checkpoints.md)
- [0035 v0.1 flavors](0035-v01-flavors.md), [0040 agentgateway](0040-agentgateway-model-and-mcp-gateway.md)
