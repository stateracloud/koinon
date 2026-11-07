# 0051. Session home generations

- Status: Accepted; amended by [0054](0054-unkillable-sandbox-process.md), [0065](0065-work-model-tasks-inbox.md) and below (2026-10-23, 2026-10-24, 2026-10-26)
- Date: 2026-10-22
- Amends: ADR 0029 (when a ticket's session is loaded) and ADR 0049 (what holds off a sandbox
  recreation)

> Note (2026-10-26): v0.1 ships OpenCode only ([0059](0059-v01-opencode-only.md)); mentions of Hermes and pi below are history.

## Context

ADR 0029 keeps one harness session per ticket: `session/new` on the first run, `session/load`
afterwards. The harness stores its sessions in the sandbox's home volume. OpenShell 0.1.5 deletes
that volume together with its sandbox and offers no way to detach it, so whenever koinon-reconciler
recreates a sandbox (a spec change: image, providers or filesystem rules), or a sandbox disappears
and the reconciler creates a new one, the stored sessions are gone while tickets still name them.
The next run's `session/load` then fails, and the run with it.

The recreate guard also counted only `starting` and `running` runs, so a sandbox could be recreated
while a run was parked on an approval; once approved, the resumed run loaded its session in an empty
home.

## Decision

1. **Home generation.** `agent_sandboxes.home_generation` identifies the sandbox's current home
   volume: the OpenShell ID of the sandbox, as the reconciler last observed it. Every created
   sandbox gets a new ID; stop and start keep the sandbox, its ID and its volume.
   `tickets.harness_session_generation` records the generation a ticket's session was created in.
   agentd calls `session/load` only when both are known and equal. Otherwise it calls
   `session/new`, stores the new session with the current generation, prompts it with the
   ticket's title and every message (the new session knows nothing of the ticket), and, when the
   ticket had a session, appends the ticket event `run.session_reset` ("The agent's workspace was
   reset, so this run starts a new session."). Sessions recorded before generations existed have
   no generation and start afresh once.
2. **Parked runs hold off recreation.** A spec change waits until no run of the agent is
   starting, running or parked (`GetAgentRunActivity.has_held`). Idle stop is unchanged: parked
   runs still do not keep a sandbox up, since stop keeps the home.

## Consequences

- A recreated sandbox costs the agent its conversation history, not the run; the timeline says so.
- A spec change can wait for as long as a run stays parked (up to its park expiry).
- The generation is only as fresh as the reconciler's last observation. A sandbox replaced by hand
  between a pass and a run's exec is detected on the next pass; until then that run's
  `session/load` can still fail.

## Amendment (2026-10-23): resume by default

Sessions resume by default: `session/load` is the normal path ([0029](0029-run-level-resume.md)).
A new agent version can be marked **apply to open tickets** (opt-in). Open tickets of that agent
then start a **new session plus a handoff** instead of resuming: the agent's earlier replies on the
ticket and an end-of-turn summary.

## Amendment (2026-10-24): fresh session with a handoff

Decided by the product owner; it settles what was open above. Plan item AG1 builds it.

1. **The option.** Activating an agent version takes **"Apply to open tickets now (starts fresh
   sessions with a handoff)"**: `ActivateAgentRequest.apply_to_open_tickets` and a checkbox on the
   activation. Default off. The activation is audited (`agent.activate`) with the flag and the
   number of tickets marked. Only open tickets (not Done) with a stored session are marked
   (`tickets.session_restart_pending`).
2. **The fresh session.** The next run on a marked ticket starts `session/new` instead of loading
   (a new session in the same home), stores it with the home generation and clears the mark, and
   the timeline says "Session restarted: new instructions." (`run.session_restarted`). Tickets
   not marked resume as before. A restart takes precedence over a reset
   ([decision 1](#decision) above) when both apply, because its prompt carries more.
3. **The end-of-turn summary.** At the end of every turn agentd sends one more prompt on the same
   session asking for a short note of state and next steps. It is a normal model call through the
   gateway, so the ledger counts it in the run's and the agent's budget. The note is limited to
   2,000 tokens (instructed, then truncated at 6,000 bytes, bytes / 3 as the estimate), stored on
   the ticket (`tickets.agent_summary`, replaced each turn) and shown, collapsed, as **"Agent's
   notes"** to anyone who can see the ticket.
4. **No visible reply, no side effects.** The summary prompt's message chunks are not recorded as
   run events, which the ticket's reply is built from; only a `run.summary` event records its size.
   Tools are refused in three ways, because ACP has no switch to turn a harness's tools off: the
   prompt says to call none; permission requests during it are rejected; and while it runs the run
   is marked `summarizing` (the mark is cleared when a run is claimed or taken over, so a lost owner cannot leave the tools off), which makes Koinon's tool servers (GitHub and remote MCP servers,
   `GetRunningRunForAgent`) answer as if no run were active, so no approval request can be
   recorded. A harness that calls a tool anyway has its note discarded (`run.summary_refused`). A
   note that fails or times out (2 minutes) never fails the run (`run.summary_failed`). This is
   harness-independent (it uses `session/prompt` only) and is exercised against OpenCode, Hermes
   and pi by the flavor contract suite.
5. **The handoff.** The first prompt of a fresh session is the ticket's title, the latest note, the
   agent's last three replies, every message of the people on the ticket and the approval
   outcomes not yet sent, capped at about 8,000 tokens (bytes / 3): the oldest replies are dropped
   first, then the oldest messages (never the newest), and last the note is cut.

Consequences: every turn costs one more short model call; a ticket whose agent was never asked
(a session from before this) hands off from its messages alone; a handoff is a summary, not the
session, so the agent can miss detail the old session had.

## Amendment (2026-10-26, old issue 146 A6): the generation follows the checkpoint lineage

[0065](0065-work-model-tasks-inbox.md) builds home checkpoints in v0.1
([0032](0032-agent-home-checkpoints.md)). The home generation now follows the home's checkpoint
lineage, not the OpenShell sandbox ID of decision 1:

- A sandbox restored from a checkpoint keeps its generation, and the harness reloads the same
  session (`session/load`).
- Only a sandbox created without a checkpoint gets a new generation, and with it decision 1's
  `session/new`, reset event and handoff.

Prior art (ideas only, no code; its servers are AGPL-3.0): Daytona v0.190.0 keeps a sandbox's
identity when it restores it from a backup on another runner (`sandbox-start.action.ts:475-540`;
research notes `daytona.md`).

## Amendment (2026-10-27, HC1): how the lineage is recorded

`agent_sandboxes.home_generation` is now the home's lineage and `agent_sandboxes.sandbox_id` the
OpenShell ID of the sandbox last observed (RecordAgentSandbox). When the reconciler deletes a
sandbox for a recreation after a complete home checkpoint it sets `restore_checkpoint_id`; the next
sandbox keeps `home_generation`, and agentd restores the checkpoint before the harness starts, so
`session/load` finds the session. A recreation without a checkpoint (checkpoints off, an ephemeral
agent) records the new sandbox's ID as before. The stuck mark of ADR 0054 compares `sandbox_id`.
