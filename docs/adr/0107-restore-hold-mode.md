# 0107. Restore-hold mode: a restored install starts held

- Status: Accepted
- Date: 2026-11-10
- Relates to: [0085](0085-upgrade-compatibility-and-recovery-policy.md) (the patch-line rule; recovery policy),
  [0100](0100-install-audit-chain.md) (`restore-hold` is a recorded setting), [0023](0023-observability-and-audit.md) (fixed metric label sets)
- Issues: closes #462; refs #275 (the restore drill)

## Context

The restore drill (#275) showed a gap. A dump holds a backlog: schedules that are due, runs that
held a lease when the dump was taken, sandboxes the reconciler would converge, and a code-host scope
the API would read. The moment a restored install starts, it acts on that backlog: schedules fire, runs
are claimed or taken over, sandboxes are created, and outward calls (model providers, code hosts)
leave the cluster. The operator needs to check three things first: the sealed keys open (the KEK came
back with the dump), the org run limits fit the backlog, and the audit chain is whole. Today they
can check only after the install already acted.

A hold must work for a dump taken before any hold existed, and it must work when the database is
not yet trusted.

## Decision

1. **A process-start flag in all three binaries.** `--restore-hold` (env `KOINON_RESTORE_HOLD`) is read at start by
   `koinon-api`, `koinon-agentd` and `koinon-reconciler` (package `internal/restorehold`). The chart value `restoreHold`
   (boolean, default false; a non-boolean fails the render) sets it on all three Deployments.
2. **What a held process does.** It starts, serves its health and metrics listeners, logs `restore-hold` and
   exports `koinon_restore_hold 1`. It does no automatic work and starts no outward call:
   - `koinon-api` runs no DBOS runtime (so no schedule fires, no pending workflow is recovered, no queue drains), no
     sweepers, no price refresh and no code-host scope refresh. It still serves the API and reads, and the CLI
     commands (`keys verify`, `org-limits show|set`) the operator uses to check and tighten. A call that would reach outward is
     refused with `FailedPrecondition` on the Connect API and `409` on the admin REST API ("install is held for
     restore; release the hold first"). The guard is at the service layer: one shared `restorehold.Guard`
     sits in each service that calls out (the provider echo check, the MCP server preflight, run-trace
     destinations, approvals, the code-host scope read), which returns `restorehold.ErrHeld`, so the hold holds
     for every caller and both API surfaces, and a new caller of a guarded service is held without any check of its
     own. What is refused: the provider echo check; adding an MCP server or changing its address (the add-time
     preflight); saving or testing a run-trace destination; approving a request (**Approve**; the approved call
     would run at once, and Deny stays open), an approved call's execution (including trusted-preset calls) and an
     approval's diff; creating, re-keying, testing and listing a code-host connection and the scope read they
     queue; the code-host check of an agent's repository bindings; and **Run now** of a schedule trigger.
     No new work is queued while held: **Run now**, `CreateTask`, a task or ticket follow-up message (it
     stores the message and queues a run in one transaction, so the whole call is refused), `CreateTicket` and direct
     messages are refused with the same guard, at the service layer; `CreateTriggeredTask` is the one choke point
     of every trigger fire. The declarative importer skips an
     `mcp-server` Secret whose apply would dial out (a new server or a changed URL): it logs
     `held: MCP server <name> not applied until release` once per pass, records no Invalid Event, and carries on
     with the other Secrets and the removal pass; the Secret is imported after the release. The
     connection and binding handlers call the code host themselves, with no service in between, so they keep a
     check of their own (`Server.refuseWhileHeld`).
   - `koinon-agentd` claims and takes over no run and touches no sandbox. It still serves `checkpoints resync`, which
     the runbook runs before the release.
   - `koinon-reconciler` converges no sandbox, applies no provider key and runs no spend settler or cost
     reconciliation.
3. **Release is a rollout.** Set `restoreHold=false` and upgrade; the pods restart without the flag. There is no live
   toggle. `koinon_restore_hold` is 0 on all three after it.
4. **Alert.** The chart's `KoinonRestoreHoldOn` rule fires when `max(koinon_restore_hold) == 1` for 30 minutes
   (severity warning, no extra labels). A held install does no work, so a forgotten hold is an outage.
5. **Settings record.** `restore-hold` is in `recordedSettings`, so the audit chain shows when an install
   started held and when it was released (ADR 0100).
6. **Runbook.** The backup and restore guide starts the restored install held, runs the checks (`keys verify`,
   `org-limits`, `checkpoints resync`), then releases. `test/e2e/backup-restore.sh` runs that order.

## Alternatives considered

- **A database-backed hold row** (a setting an admin flips live). Releasable without a rollout. But a dump taken
  before the row existed has no row, so the restore of such a dump would run unheld; and the components must read the
  database before they know whether to hold, so a component that cannot reach the database, or reads an
  untrusted one, cannot hold. The hold has to exist outside the data it protects.
- **A gateway-level block** (agentgateway or a NetworkPolicy refuses the outward calls). It does not stop the
  internal effects: schedules still fire, runs are still claimed and lose their lease, sandboxes are still
  converged. It also leaves the install in a state the operator cannot tell from a working one.

## Prior art

Postgres `recovery_target_action = pause`: the restored server reaches the target and stops before it opens for
writes, so the operator checks the data and then promotes. Our hold is the same shape: the install is up and
readable, nothing acts, and an explicit step ends it. We differ in that the hold is a start flag, not
a state in the data, for the reasons above.

## Consequences

- Positive: a restore can be checked before it acts; the hold covers dumps taken before this ADR; it needs no database read.
- Negative: release is a rollout (a minute), not a toggle.
- The hold is not complete silence. The purger, the audit anchor and the importer still write while held (they keep
  the retention rules and the audit chain current); the operator should expect those writes.
- The hold does not stop sandboxes that are already running. An in-place restore with live sandboxes leaves them
  running; their model calls go through agentgateway, not koinon-api. Stop them first. (The branch check in the
  GitHub tool server is reachable only from such a sandbox.)
- A forgotten hold is an outage; the alert is the guard.
- ADR 0085: the flag changes no schema and adds no migration, so it is allowed within a shipped patch line.
