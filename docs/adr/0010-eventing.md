# 0010. Eventing: the ticket board is the queue

- Status: Superseded by [0065](0065-work-model-tasks-inbox.md) and [0066](0066-triggers-and-scheduling.md)
- Note: what still describes the code is the 2026-10-26 wake-up amendment below: row state is the
  truth, `LISTEN/NOTIFY` is only a hint, and the `runs` table is the queue for runs. Work arrives
  as tasks ([0065](0065-work-model-tasks-inbox.md)) through triggers ([0066](0066-triggers-and-scheduling.md)).
- Date: 2026-10-21
- Area: work-model
- Tags: eventing, triggers, tasks
- Decision: [design.md#10](../design.md)

## Context

Work reaches agents from humans, webhooks (GitHub, Slack), crons (routines) and internal events
(another agent's result, a budget alert). An event bus (NATS, Kafka) is the usual answer. But
Postgres is already the source of truth ([0003](0003-postgres-source-of-truth.md)), and writing to
both Postgres and a bus creates the dual-write problem.

## Decision

- In v1 the ticket board is the queue.
- Webhooks, crons and internal events are matched against **org subscriptions**. A match either
  creates a ticket or appends to an open ticket. Appending to an open ticket is the dedupe and
  debounce mechanism.
- Events use the CloudEvents envelope (spec v1.0.2).
- Budgets apply per subscription, so a noisy webhook cannot burn an org's budget.
- An event bus is added only if v1 shows the need.

## Alternatives considered

- **NATS JetStream or Kafka** _(reconstructed)_. Scales and fans out well, but there is one more
  system to run, plus the dual write with Postgres.
- **Postgres LISTEN/NOTIFY as the queue** _(reconstructed)_. No persistence and no replay, so it
  serves only as a wake-up signal next to an outbox table.
- **A Kubernetes-native eventing system such as Knative Eventing** _(reconstructed)_. Heavy, and it
  assumes CRDs as the source of truth.

## Consequences

- Positive: every unit of work is visible on the board. Nothing hides in a queue.
- Positive: there is nothing extra to operate.
- Negative: throughput is bounded by Postgres. That is fine at the expected scale, but high-volume
  webhooks need debouncing at ingestion _(inferred)_.
- Negative: the subscription matching language must be designed carefully. A CEL filter over
  CloudEvents attributes is the obvious candidate _(inferred)_.
- Follow-ups: a transactional outbox for reconciler and UI updates. Clients resume from the last
  outbox ID they saw, and consumers must be idempotent _(inferred)_.

## Open questions

- The subscription filter language (CEL?) and the debounce window semantics.
- Whether webhook ingestion is part of v0.1 (probably not; routines and webhooks are v0.2 per
  [0024](0024-roadmap.md)).

## Upstream verification

- 2026-10-21: CloudEvents v1.0.2 is the latest released spec (CNCF graduated).
  https://github.com/cloudevents/spec
- 2026-10-21: the transactional outbox guarantees a message is sent if and only if the transaction
  commits; the relay can deliver duplicates.
  https://microservices.io/patterns/data/transactional-outbox.html
- 2026-10-21: NOTIFY is delivered only on commit, is limited to 8000 bytes, and is not replayed.
  https://www.postgresql.org/docs/current/sql-notify.html

## Amendment (2026-10-26): Postgres wake-ups instead of an outbox

The follow-up "a transactional outbox for reconciler and UI updates" was not built, and no outbox
table exists. What was built instead:

- Row state is the source of truth, and `NOTIFY` is only a wake-up hint. A listener re-reads the
  rows after its last seen position, so a lost or coalesced notification loses nothing.
- Run updates for the UI: a trigger sends `pg_notify('koinon_run', '<org_id>/<run_id>')` on every
  new `run_events` row and every `runs` change (`internal/db/migrations/00005_run_scheduling.sql:21-42`).
  Each API replica listens (`internal/runwatch/hub.go:28`, `:167`) and clients resume from the last
  run event sequence number they saw, not from an outbox ID.
- Reconciler wake-ups: a trigger sends `pg_notify('koinon_reconcile', '<org_id>')`
  (`internal/db/migrations/00006_agent_sandboxes.sql:79-90`; the reconciler `LISTEN`s in
  `internal/reconciler/loop.go:16`, `:67`) and the reconciler also re-reads everything on a timer.

When this amendment was written the board was still the queue for work and webhooks, crons and
subscriptions were not built. Tasks and triggers replace both (amendment below). [0003](0003-postgres-source-of-truth.md) and
[0022](0022-frontend-architecture.md) also mention the outbox; read them with this amendment.

## Amendment (2026-10-26): tasks and triggers

- **Tasks are the queue for work**, not the board ([0065](0065-work-model-tasks-inbox.md)). The
  board is a secondary view over tasks.
- **Triggers replace org subscriptions.** They live on the agent's versioned config: schedules,
  code-host events, chat events and direct messages; tracker events come later
  ([0066](0066-triggers-and-scheduling.md), [0064](0064-work-tracker-connectors.md)).
- **Ingress** is webhooks with signature checks, Slack Socket Mode and polling. Everything lands in
  DBOS Go, a library on Koinon's Postgres, for dedupe and retries ([0066](0066-triggers-and-scheduling.md)).
- **"Append to an open ticket" becomes correlation by key:** an event whose key matches an open
  task wakes that task; otherwise it creates one. Bursts are batched into one wake-up.
- Still true: there is no event bus, and Postgres is the source of truth. Not decided again: the
  CloudEvents envelope, the filter language, and budgets per trigger (open in
  [0066](0066-triggers-and-scheduling.md)).

## Links

- [0003](0003-postgres-source-of-truth.md), [0011](0011-agent-to-agent.md),
  [0017](0017-board-and-chat-interaction-model.md), [0022](0022-frontend-architecture.md)

## Note (2026-11-12, issue 573): the wake-up channel and the work queue

The reconciler's wake-up stays a one-slot channel that a burst of NOTIFYs fills once. That is correct
only while a pass is short. Since issue 573 a pass does not wait for the sandbox work of any agent
(a per-agent work queue runs it, [0066](0066-triggers-and-scheduling.md)), so a slow backend call for
one agent cannot keep a wake-up for another agent waiting. Postgres is still the source of truth and
the NOTIFY payload is still empty.
