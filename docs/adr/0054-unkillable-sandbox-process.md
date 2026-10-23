# 0054. A sandbox with an unkillable process is recreated

- Status: Accepted
- Date: 2026-10-23
- Amends: [0051](0051-session-home-generations.md) (adds a reason for recreating a sandbox)

## Context

A process in uninterruptible sleep (state D, usually stuck on storage or a network filesystem)
cannot be killed, even by `SIGKILL`. agentd's process sweep ([0051](0051-session-home-generations.md),
plan item AG1) then cannot end it, and the sweep fails the run. Left alone, every later run in that
sandbox fails the same way.

## Decision

1. When the sweep finds a process it cannot end because it is in D state, and the state lasts
   60 seconds, the sandbox is marked unhealthy.
2. koinon-reconciler recreates an unhealthy sandbox. The recreation is audited, with the process
   and the sandbox named.
3. The agent's home is kept through checkpoints when they exist ([0032](0032-agent-home-checkpoints.md)).
   Without checkpoints (v0.1), the home goes with the sandbox, as for any recreation, and the next
   run starts a new session ([0051](0051-session-home-generations.md)).

## Alternatives considered

- **Wait indefinitely** _(reconstructed)_. A D-state process can stay for hours; the agent is
  unusable meanwhile.
- **Reboot the node** _(reconstructed)_. Hits every tenant on the node for one agent's problem.
- **Fail the run and keep the sandbox** _(reconstructed)_. The next run hits the same process.

## Consequences

- Positive: a stuck sandbox heals without an operator.
- Negative: without checkpoints the agent loses its home, including conversation history, though
  tickets keep their messages.
- Negative: the stuck process may leave the underlying storage problem in place _(inferred)_.
- Follow-ups: the sweep reports D-state processes; the reconciler gets an unhealthy flag on
  `agent_sandboxes`; the audit event type.

## Open questions

- Whether a recreation held off by a parked run ([0051](0051-session-home-generations.md),
  decision 2) is overridden when the sandbox is unhealthy.
- How D state is detected in the sandbox (the source of the process state).

## Upstream verification

None needed.

## Links

- ADRs [0032](0032-agent-home-checkpoints.md), [0051](0051-session-home-generations.md)
