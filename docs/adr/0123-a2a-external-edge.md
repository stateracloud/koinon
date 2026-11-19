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
2. **A thread is a context. A turn is a task.** The edge follows A2A 1.0.0: a task that has ended
   (completed, failed, canceled, rejected) never takes a new message, and a follow-up turn is a new
   task in the same `contextId` (sections 3.1.1, 3.4.3). A Koinon direct-message thread is one
   `contextId`: the UUID of the Koinon task. Each turn of the thread is one A2A task. The task ID is
   `<thread UUID>.<turn>`, with the turn counted from 1, and a task ID is never reused. A turn
   starts with the first message of a person, and with each message of a person that reopens a
   completed thread. A message that answers a question is part of the open turn. Every turn before
   the last has ended as completed, because only a completed thread takes a follow-up.
   The new task is linked to the one before it: the first message of the turn has
   `referenceTaskIds` with the earlier task. `SendMessage` has three forms:
   - No task ID and no context ID: a new thread, turn 1.
   - A context ID and no task ID: the next turn in that thread. If the last turn is still open,
     the message is part of it (it is the same thread, and Koinon queues the message), and the
     answer holds the open task. A thread that failed or was canceled takes no message.
   - A task ID: the answer to that task. Only the last turn can take it, and only while it is open.
     A turn that ended gives `UnsupportedOperationError`. A task ID that is not a turn of a thread
     of the agent gives `TaskNotFoundError`. A `contextId` that is not the thread of the task gives
     `InvalidParamsError`.

   The message goes through the same path as a person who writes to the agent: the checks of the
   message, the org queue when durable work is on, and the same agent and trigger rules. The
   A2A message ID is the idempotency key. A retry of a message that was taken returns the task, also
   when the task has ended since. An agent takes A2A messages only if it has a `direct_message` trigger.
   The edge shows no other task of the agent. the same path as a person who writes to the agent: the checks
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

   The state of the last turn is the Koinon state. An earlier turn is `completed`. Messages of people are user messages.
   Messages and questions of the agent are agent messages. When the task needs input, the status
   message holds the question. A completed task has one artifact, `reply`, with the last reply of
   the agent, for the turn of the task. A message to a task that has completed is refused inside the
   lock of the task, so a race with the end of the turn cannot reopen it. Only a message with the
   context ID reopens the thread.
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
- Negative: the turns are read from the timeline: no new column or table. A thread whose timeline is
  over 10,000 events has a wrong turn count in `GetTask`, as above.
- Negative: a thread has one open turn at a time, so a client cannot run two tasks in one context
  at once. It starts a new thread (a new context) for work in parallel.
- Follow-ups: the outbound slice; pin the kagent version used by the e2e ([0101](0101-agent-substrate-and-kagent-not-adopted.md)).

## Open questions

- Does a reply in the thread answer an open `ask_user` question without a separate step? To check
  on kind.

## Upstream verification

a2a-go v2.8.0 serves v1.0 and, through `a2acompat/a2av0`, v0.3. Checked on 2026-11-19 in the module
source. kagent was not checked: the VM has no access to its registry.
