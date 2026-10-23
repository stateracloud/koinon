# 0051. Session home generations

- Status: Accepted; amended by [0054](0054-unkillable-sandbox-process.md) and below
- Date: 2026-10-22
- Amends: ADR 0029 (when a ticket's session is loaded) and ADR 0049 (what holds off a sandbox
  recreation)

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

Open: the size limit of the handoff, and who writes the end-of-turn summary.
