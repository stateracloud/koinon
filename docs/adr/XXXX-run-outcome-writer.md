# XXXX. One writer ends a run

- Status: Accepted
- Date: 2026-11-01
- Relates to: [0065](0065-work-model-tasks-inbox.md), [0074](0074-run-traces-trace-router.md), [0082](0082-work-claims-and-ready-queue.md), [0087](0087-turn-policy-is-a-pure-state-machine.md)
- Issue: #453

## Context

Six places in agentd knew how to end a run, and each took the source-seq lock by hand and had to know
the order of the rows it wrote: the finish and task-end transactions and the reply in `run.go`, the
160-line turn-end transaction in `park.go`, the requeue and the sandbox-unavailable failure in
`sandbox.go`, and the trace's turn row in `runtrace.go`. The order rules lived in comments: the turn row
ends before the run state changes; the reply comes before the park note and before `run.parked`; the
task ends after the reply; the source seq only grows under the write mutex; `drive` had to end the trace
before it concluded. At least five fixes were a broken order rule (the turn row written after the
success commit; the task left working after a park; and three more).

## Decision

1. **`internal/runoutcome` owns how a run ends on its timeline.** Every writer that ends or cancels a
   run goes through it. Nothing else writes `FinishRun`, the closing run event or the task's end.
   This covers agentd now; the ledger and the scheduler sweep move onto it in their own change.
2. **A per-run `Owner` handle**, opened when a worker claims a run. It holds the org, run, epoch, the
   source seq, the write mutex and the fence state, so none of them is on agentd's runner. Its verbs:
   `Record` (a run event, idempotent through `AppendRunEventOnce`, abandons the run when the event
   cannot be written), `EndTurn`, `Succeed`, `Fail(class)`, `Cancel(note)`, `Requeue`, `Release` and
   `SandboxUnavailable`.
3. **`EndTurn` decides between park and success.** In one transaction under `LockRun` it marks the
   delivered approvals and questions, then parks or succeeds. The race with the approval service that
   resolves the park stays inside the module, ordered by the run's row lock.
4. **The module ends the trace's turn row.** Each ending verb takes the open turn row (`Turn`) and ends
   it in the same transaction, before the run row changes, so whoever sees the run finished sees the
   turn ended. The trace keeps its spans and attributes. agentd no longer has to end the trace before
   it concludes; it ends the spans first only so they are exported promptly.
5. **Stale writers write nothing.** A verb whose epoch no longer owns the run matches no row, rolls
   its transaction back and returns `ErrFenced` after telling a cancel from a lost epoch (ADR 0082).
6. **The turn policy's typed outcome is mapped onto the verbs** by agentd's `conclude` (ADR 0087); it
   holds no write of its own.
7. **The module is tested against real Postgres**, one group of specs per verb: the order of the
   timeline, the fence of a stale epoch, and the race between park and approval. No fake harness and no
   agentd pool. The agentd specs stay as the regression net; a follow-up replaces the ones the module
   specs cover with a few smoke specs.

## Consequences

- An order rule is a property of one package and has one spec, not a comment in five files.
- A new way for a run to end is a new verb, which has to say where the turn row and the task end go.
- The module has no knowledge of how a run is driven: it takes reasons, classes and notes as values, so
  the ledger and the scheduler can use the same verbs for their cancels.
- agentd's metrics and the pool's shutdown reason reach the module through two small hooks
  (`Metrics`, `Stop`), not an import of agentd.
