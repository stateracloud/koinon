# 0029. Run-level resume through the harness session

- Status: Accepted; amended by [0051](0051-session-home-generations.md)
- Date: 2026-10-21
- Area: runtime-sandbox
- Tags: harness, runs, flavors, agent-home
- Decision: [design.md#29](../design.md)

> Note (2026-10-26): v0.1 ships OpenCode only ([0059](0059-v01-opencode-only.md)); mentions of Hermes and pi below are history.

## Context

Runs get interrupted in several ways:

- they park for approvals ([0042](0042-ask-tier-park-and-execute.md)), budgets
  ([0041](0041-koinon-spend-ledger-and-budget-stops.md)) or A2A replies ([0011](0011-agent-to-agent.md))
- the sandbox is suspended between runs ([0009](0009-agent-runtime.md))
- a node fails or the pod is evicted

The harness (OpenCode, Hermes, pi) owns the agent loop. That loop can't be checkpointed from
outside: Koinon can't snapshot the model's working state in the middle of a step.

What the harnesses do offer is a saved session. Each one persists turns to disk under `$HOME`, and
ACP lets a client reload a session:

- `session/load` is an optional capability: the agent must return `loadSession: true` from
  `initialize`, and the client must not call it otherwise.
- `session/resume` is a newer optional method that reattaches without replaying history.

Agent Sandbox suspend keeps the volume but not memory: snapshot and memory checkpointing are out of
scope for its beta. The home volume is therefore the only state that survives
([0032](0032-agent-home-checkpoints.md)).

## Decision

- **Resume from the last saved session turn.** When a run continues after a park, a suspend or a
  crash, `agentd` reconnects to the harness and continues from the last turn the harness saved:
  - ACP `session/load` (or `session/resume` where advertised)
  - or the harness's own `--resume` / `--continue`
- **Repeating turns must be safe.** A resumed run may redo work done after the last saved turn.
  Every external write passes through either the MCP gateway
  ([0040](0040-agentgateway-model-and-mcp-gateway.md)) or the OpenShell supervisor, and both record
  that write per run with an idempotency key. A repeated write is therefore deduplicated instead of
  executed twice.
- **The resume contract is part of each flavor.** Each flavor declares which resume method it
  supports. Flavor contract tests ([0025](0025-testing-strategy.md)) check load and resume behaviour.
- **Workflows later.** Resuming at the level of individual steps (durable workflows) is deferred to
  when workflows exist ([0030](0030-run-state-machine.md)).

## Alternatives considered

- **Checkpoint the whole sandbox** (CRIU or a VM snapshot) _(reconstructed)_: would capture memory
  state, but Kata and Agent Sandbox do not support it in beta, it is fragile across nodes, and it is
  heavy.
- **Restart every interrupted run from scratch** _(reconstructed)_: simple, but it wastes tokens and
  repeats side effects. Idempotency would still be needed.
- **Koinon-owned step workflow engine** (Restate, Temporal) _(reconstructed)_: would checkpoint
  every step, but the harness loop is opaque, so steps can't be extracted without forking each
  harness. Revisit with workflows ([0030](0030-run-state-machine.md)).

## Consequences

- Positive:
  - Near-zero cost to keep an agent between runs.
  - Parks of any length are survivable.
  - Works with any harness that persists sessions.
- Negative:
  - Resume quality depends on each harness ("Upstream verification" below lists the caveats):
    - Session IDs and working directories must be persisted by Koinon _(inferred)_.
    - Work after the last saved turn is repeated, costing tokens.
- Follow-ups:
  - `agentd` keeps a run record holding the ACP session ID, original `cwd`, flavor resume mode and
    last turn index, stored with the run in Postgres and mirrored on the home volume _(inferred)_.
  - The MCP gateway and the supervisor path record idempotency keys per run: `run_id` plus a call
    hash, or a key from the tool.
  - Flavor contract test: kill the harness mid-run, resume, assert that no duplicate external write
    happens and that the session continues.

## Open questions

- Should `agentd` prefer `session/resume` (no history replay) over `session/load` where both exist?
- How are idempotency keys derived for supervisor-path HTTP writes with no tool-level key? Request
  hash, or a header the harness can't set?
- What does `agentd` do when Hermes silently starts a new session on an unknown ID? Detect it and
  fail the run, or accept the fresh session?

## Upstream verification

Checked 2026-10-21:

- **ACP session setup:** `session/load` is optional behind `loadSession`. `session/resume`,
  `session/list` and `session/close` are optional capabilities. Protocol version 1, schema
  v1.26.0. https://agentclientprotocol.com/protocol/session-setup
- **OpenCode** (anomalyco/opencode, MIT):
  - Advertises `loadSession: true` plus resume, list and fork.
  - `session/load` replays history; resume and fork do not.
  - **The session's original `cwd` must be passed or the load is rejected.**
  - `/undo` and `/redo` don't work over ACP.
  - https://opencode.ai/docs/acp/
- **Hermes** (NousResearch/hermes-agent, MIT, v0.22.2):
  - Advertises load, resume and list.
  - **`resume_session` silently starts a new session when the ID is unknown**, while
    `load_session` returns nothing.
  - The CLI also has `hermes --resume`.
- **pi via pi-acp** (svkozak/pi-acp, MIT, v0.0.34, "MVP-style"):
  - Advertises `loadSession` and `list`, **but not `resume`**.
  - Keeps its own map in `~/.pi/pi-acp/session-map.json`, which must survive on the home volume.
- **Agent Sandbox:** suspend keeps volumes but not memory; no snapshot in beta. KEP-0694.
  https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/keps/694-kep-for-suspend-and-resume-for-beta/README.md

## Links

- [0009 Agent runtime](0009-agent-runtime.md), [0016 Execution backend and agentd](0016-harness-and-deployment.md)
- [0030 Run state machine](0030-run-state-machine.md), [0032 Agent home](0032-agent-home-checkpoints.md)
- [0035 v0.1 flavors](0035-v01-flavors.md), [0042 Ask tier](0042-ask-tier-park-and-execute.md)
