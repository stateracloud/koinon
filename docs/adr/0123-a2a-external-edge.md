---
status: proposed
date: 2026-11-19
area: work-model
tags: [api, auth, tasks]
relates: ["0011", "0065", "0066", "0101", "0120"]
---
# 0123. External A2A edge

- Status: Proposed
- Date: 2026-11-19
- Area: work-model
- Tags: api, auth, tasks
- Relates to: [0011](0011-agent-to-agent.md), [0065](0065-work-model-tasks-inbox.md), [0066](0066-triggers-and-scheduling.md), [0101](0101-agent-substrate-and-kagent-not-adopted.md), [0120](0120-execution-hardening-defaults.md)
- Issue: [#1129](https://github.com/stateracloud/koinon/issues/1129)
- Verification: Pending: a kind test with a kagent agent in each direction (issue 1129). The inbound
  half is covered by specs with a fake backend.

## Context

[0011](0011-agent-to-agent.md) says A2A is the protocol at the edge. Nothing implements it yet.
kagent agents speak A2A. A Koinon agent must be able to take a task from such an agent, and (second
slice) send a task to one.

Koinon already has a work model ([0065](0065-work-model-tasks-inbox.md)): a task has one owning
agent, a state, and a timeline of events. A message to an agent starts a direct-message thread
([0066](0066-triggers-and-scheduling.md)). The task states are close to the A2A states.

## Decision

1. **Inbound edge.** Koinon serves A2A on `/a2a/v1/agents/{agent}/`. It serves the v1.0 JSON-RPC
   binding at `jsonrpc` and the v0.3 binding at `v0.3/jsonrpc`. It serves one agent card per agent at
   `.well-known/agent-card.json`. The edge implements `a2asrv.RequestHandler` of a2a-go directly over
   Koinon tasks. It has no second task store and no executor.
2. **A message is a direct message.** `SendMessage` without a task ID starts a thread. With a task ID
   it continues that thread. It uses the same path as a person who writes to the agent: the checks
   of the message, the org queue when durable work is on, and the same agent and trigger rules. The
   A2A message ID is the idempotency key. A retry of a message that was taken returns the task, also
   when the task has ended since. An agent takes A2A messages only if it has a `direct_message` trigger.
   The edge shows no other task of the agent.
3. **State mapping.**

   | Koinon | A2A |
   | --- | --- |
   | submitted | `TASK_STATE_SUBMITTED` |
   | working | `TASK_STATE_WORKING` |
   | input_required | `TASK_STATE_INPUT_REQUIRED` |
   | completed | `TASK_STATE_COMPLETED` |
   | canceled | `TASK_STATE_CANCELED` |
   | failed | `TASK_STATE_FAILED` |

   The task ID and the context ID are the Koinon task ID. Messages of people are user messages.
   Messages and questions of the agent are agent messages. When the task needs input, the status
   message holds the question. A completed task has one artifact, `reply`, with the last reply of
   the agent. A task that has ended takes no message, and the delivery refuses it inside the lock of the task (a
   follow-up from a person reopens a completed task, an A2A message does not): the client starts a
   new task. A client that sends a `contextId` without a task ID starts a new thread: Koinon does not
   link A2A tasks into one context yet (see open questions).
4. **Blocking send.** `SendMessage` waits until the task is `input-required` or has ended, up to 30
   seconds, then returns the task as it stands. `returnImmediately` returns at once. The client
   then reads the task with `GetTask`. The wait reads only the events after the message. A run that
   parks writes `run.parked` and no status event, so the wait takes `run.parked` or
   `question.asked` after the message as the proof of `input-required`, not the state alone (the
   state can be left over from before a reply). At most 64 waits run at one time; more return at
   once. An error that is not for the client returns as an A2A internal error and is logged.
5. **Auth.** The edge uses org API tokens behind the same gate as the admin API (token check,
   failure rate limit, audit event per call). The token needs the member role. Messages are written
   as the user that created the token. The agent card names an HTTP bearer scheme. The edge adds no
   new key, credential or table.
6. **Not served in this slice.** Streaming, task list, push notifications and the extended card
   return the A2A errors for an unsupported operation. The card says so in its capabilities.
7. **Outbound edge (next slice).** A Koinon agent calls an external A2A agent through the gateway.
   The route follows [0120](0120-execution-hardening-defaults.md): default-deny egress, enforced L7
   rules, and the credential held by the gateway. The sandbox never holds it. This ADR is amended
   when that slice is designed.

## Alternatives considered

- **The SDK executor and task store.** It keeps a second copy of every task and needs a bridge to
  keep both copies in step. Rejected.
- **A new token type for A2A.** More key infrastructure for no gain over org tokens. Rejected.
- **Streaming first.** The blocking send covers a first interop. Streaming needs a way to follow
  the timeline. Left for later.

## Consequences

- Positive: a kagent agent can send a task to a Koinon agent with a plain A2A client. The edge is
  small, because the work model already has the states.
- Negative: no streaming, so a long task needs polling with `GetTask`.
- Negative: a thread longer than 10,000 events shows its first 10,000 only in `GetTask`. _(inferred)_
- Negative: the audit event of a call has the HTTP status, so a JSON-RPC error shows as 200.
  An approval that parks a run shows `input-required` with a note and no question: the client
  cannot decide it. _(inferred)_
- Negative: a reply to an `input-required` task is a new message in the thread. It is not checked
  that the message answers the open question of the agent. _(inferred)_
- Follow-ups: the outbound slice; pin the kagent version used by the e2e ([0101](0101-agent-substrate-and-kagent-not-adopted.md)).

## Open questions

- Multi-turn conversations. A direct-message thread completes after each turn, and a person's
  next message reopens it. The edge refuses a message to a completed task, as A2A says an ended
  task is final. A client that keeps one `contextId` over many turns gets a new thread each time,
  without the earlier turns. Options: map `contextId` to the thread and reopen it (the task ID is
  then reused, which A2A does not allow), or link the new task to the earlier one in the same
  context. This is a decision about the public API of the edge.
- Does a reply in the thread answer an open `ask_user` question without a separate step? To check
  on kind.

## Upstream verification

a2a-go v2.8.0 serves v1.0 and, through `a2acompat/a2av0`, v0.3. Checked on 2026-11-19 in the module
source. kagent was not checked: the VM has no access to its registry.
