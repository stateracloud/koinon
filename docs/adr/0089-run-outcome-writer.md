# 0089. One writer ends a run

- Status: Accepted
- Date: 2026-11-01
- Relates to: [0030](0030-run-state-machine.md), [0065](0065-work-model-tasks-inbox.md), [0074](0074-run-traces-trace-router.md), [0087](0087-turn-policy-is-a-pure-state-machine.md)
- Issue: old issue 453

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
   its transaction back and returns `ErrFenced` after telling a cancel from a lost epoch (ADR 0030).
6. **agentd's `conclude` maps how a run ended onto the verbs** and holds no write of its own. It
   switches on the turn controller's typed outcome (`turnpolicy.Outcome`, ADR 0087) for a run whose
   turns ended, and on the error `execute` returned for everything before the turn: the setup-phase
   failures, a dropped connection, a requeue and the fence and shutdown sentinels. The trace reads the
   same two inputs. A verb whose transaction fails ends the turn row on its own, best effort, unless
   another owner took the run over, so a run canceled meanwhile does not keep a turn that looks
   running. `EndTurn` is the exception: it does not know the turn's end reason (a cancel fence ends it
   as "canceled", which the caller learns from the error), so the caller ends the row with `CloseTurn`
   once it knows.
7. **The module is tested against real Postgres**, one group of specs per verb: the order of the
   timeline, the fence of a stale epoch, the race between park and approval and the source seq under
   concurrent writers. No fake harness and no agentd pool. The agentd specs stay as the regression
   net; replacing the ones the module specs cover with a few smoke specs is tracked in #104 (exit
   condition: no agentd spec repeats a module spec).

## Consequences

- An order rule is a property of one package and has one spec, not a comment in five files.
- A new way for a run to end is a new verb, which has to say where the turn row and the task end go.
- The module has no knowledge of how a run is driven: it takes reasons, classes and notes as values, so
  the ledger and the scheduler can use the same verbs for their cancels.
- agentd's metrics and the pool's shutdown reason reach the module through two small hooks
  (`Metrics`, `Stop`), not an import of agentd.

## Amendment 2026-11-02 (old issue 454): the control plane's cancels

- **Every cancel by someone other than the run's owner is in the module too**, as package functions
  beside the `Owner`: `CancelUnowned` (a person, an expired approval, the ledger's budget stop of an
  agent's or org's runs), `CancelAtLimit` (the run's own request or spend limit, which carries what the
  agent last said), `CancelOverTimeLimit` (the wall-clock sweep), `CancelQueuedForTask` and
  `CancelForAgent`. With the `Owner`'s verbs they are the only writers of `CancelRun`, `FinishRun`, the
  closing run event and the cancel's task event. The cancel reasons, `Actor` and `KindRunCanceled`
  moved here from `internal/scheduler`, which keeps the limit check and the sweep's loop and calls the
  module for the write.
- **They hold no handle and no epoch; the run's state fences them.** Only a run that is still live
  (queued, starting, running or parked) is canceled; a finished one returns `pgx.ErrNoRows` and gets no
  event. The `Owner` keeps its rule that it always holds an epoch: there is no unfenced `Owner`. The two
  are ordered by the run's row: whichever commits first is the run's outcome, and the other writes
  nothing (the owner's verb is fenced as `FenceCanceled`, ADR 0050, ADR 0030). One spec per order, for a single cancel and for the sweep, each against
  the owner's success and its own cancel (old issue 522).
- **The time-limit note is built once, in the module.** `Owner.CancelAtTimeLimit` (agentd at the cap)
  and `CancelOverTimeLimit` (the sweep) both write `runreply.TimeLimitNote`; the reads the control
  plane makes inside its transaction run in a savepoint, so a failed read only leaves the summary out.
- **The park's resolution moved to `internal/runpark`** (`Resolve`, formerly `approvals.ResolveRun`):
  the module imports the question service, which resumed parks through the approval service, which
  cancels expired runs through the module. `runpark` depends on the store alone and breaks that cycle.

## Amendment 2026-11-11 (#21): `Supersede`

`Owner.Supersede` ends a run queued behind a parked run without a turn, because the earlier run already
read every message this run was queued for. It takes starting -> running -> succeeded in one transaction
under `LockRun`, ends the turn row, and completes the task.
