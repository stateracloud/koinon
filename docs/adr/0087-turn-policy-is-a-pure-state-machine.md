# 0087. Turn policy is a pure state machine

- Status: Accepted
- Date: 2026-11-01
- Area: work-model
- Tags: runs, harness, testing
- Relates to: [0081](0081-run-limits.md), [0050](0050-spend-ledger-ingestion-and-net-limits.md)
- Issue: old issue 449

## Context

What happens to a run's turn is the most-fixed code in agentd: the wall-clock cap, the wrap-up
notice, refused tool calls, model retries with backoff and Retry-After, and how a fence or a cancel
ends a turn. It was spread over about 15 functions in five files that passed state to each other
through runner fields and a decremented loop counter, and its ordering rules lived only in comments
(a run past its cap gets no notice; a refusal that lands with a cancel wins; a late cancel waits for
the cap; only the owner that recorded the notice sends it). Every spec for it needed Postgres and the
fake ACP harness, so each fix added a slow end-to-end spec and no edge case was cheap to test.

## Decision

1. **Turn policy is a pure state machine** in `internal/agentd/turnpolicy`. Events go in, together
   with the time they happened at, and the next action comes out: read usage, mark the notice, send a
   prompt, cancel and wait, record an event, sleep, wait, or end with a typed outcome. The package
   has no clock, no database, no goroutines and imports no other agentd code.
2. **I/O and classification stay in agentd.** The driver (`turn.go`) owns the goroutine of the
   prompt, the ACP connection, the ledger reads, the notice row and the run events, and arms one timer
   for the deadline each action carries. Classification reads I/O (`harnessErr`, `budgetStop`,
   `providerRefusal`, `modelGone`), so the driver turns a prompt error into facts (failure class,
   Retry-After, refusals taken, whether the worker was stopped) and the policy decides only what to do
   with them. The outcome is mapped back to the errors `conclude` and `endTurn` read; they are
   unchanged.
3. **The limits keep their ADR 0081 meaning**: three limits, the first one reached ends the run, one
   notice per run, and the ledger alone enforces the request and spend limits; the policy only reads
   usage to time the notice. The race between a budget stop and a cancel (ADR 0050) stays in
   agentd's classification.
4. **The notice row is a conversation.** The policy asks to mark the notice with its cause; the
   driver writes the row, the run event and the metric and answers recorded or lost. Lost means
   another owner holds the run and the turns end as fenced. A suppression (the harness ignored the
   cancel) is policy too: it arms the row and records no event, because the event and the metric
   mean that the agent was told.
5. **Every rule is a spec without a database.** The specs in `turnpolicy` drive the machine with
   events and times. The full-environment specs of `internal/agentd` stay as the regression net of
   this change and are replaced by a few end-to-end smoke specs in a follow-up.

## Consequences

- A change to the policy is a change to one package that is tested in microseconds.
- A new rule (a new limit, a new kind of retry) is a new event or action, which the driver must
  handle, and the compiler does not force it. The full-environment specs are what covers the driver.
- The machine is not a framework: it keeps the behaviour that was, including the oddities (for
  example each notice timer fires once, so a time notice and a usage notice can each cancel a turn).
  Fixing an oddity is its own change.
