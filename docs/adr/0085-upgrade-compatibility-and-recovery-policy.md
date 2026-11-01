# 0085. Upgrade, compatibility and recovery policy

- Status: Proposed (scope decided by Andre, 2026-10-31; the open questions below wait for his decisions; a v0.1.0 gate)
- Date: 2026-11-01
- Relates to: [0027](0027-repo-and-versioning.md) (forward-only migrations, upgrade test), [0030](0030-run-state-machine.md) (leases, epoch, takeover), [0032](0032-agent-home-checkpoints.md) (home checkpoints), [0056](0056-model-providers-pricing-and-budgets-per-org.md) (KEK and envelopes), [0060](0060-migrations-as-a-job.md) (migration Job), [0066](0066-triggers-and-scheduling.md) (DBOS), [0076](0076-database-modes.md) (database modes, backups), [0077](0077-pinned-openshell-and-installer.md) (OpenShell pins), [0045](0045-openshell-hardening-defaults.md) (pinned supervisor image)
- Issue: #336; covers #331 and #332

## Context

Deep dive G9 (research store, `g9-day2-operations`) looked at what happens to a running Koinon
install when it is upgraded, rolled back or restored. Andre decided on 2026-10-31 to write one policy
for it and to treat it as a v0.1.0 gate. The six scope items below are what the code does today, at
the commit this ADR was written on.

### 1. agentd on SIGTERM

- `koinon-agentd` cancels its root context on SIGTERM (`cmd/koinon-agentd/main.go:169`). The pool's
  workers stop at once: `Pool.Run` waits for them and then flushes spans
  (`internal/agentd/agentd.go:300-316`).
- A turn in progress returns `errShutdown` as soon as the context ends
  (`internal/agentd/run.go:840`), so the model call and any tool call in flight are cut off mid-turn.
  The runner then releases the lease (`internal/agentd/run.go:1377-1380`, `ReleaseRunLease` in
  `internal/db/queries/runs.sql:297-305`), which sets `lease_expires_at` in the past.
- Another worker picks the run up with `TakeOverExpiredRun`
  (`internal/db/queries/runs.sql:96-129`). That query increments `runs.takeovers` for every
  takeover, whether the owner died or released the run on purpose, and so does the metric
  `koinon_agentd_takeovers_total` (`internal/agentd/agentd.go:360`, `internal/agentd/metrics.go:50`).
  The harness is told "the connection to you was interrupted" and the session is reloaded
  (`internal/agentd/run.go:107-109`).
- A run that has been taken over more than `MaxTakeovers` times (default 5,
  `internal/agentd/agentd.go:143-145`, `:216`) fails with "the harness connection was lost N times"
  (`internal/agentd/run.go:376-378`). So a run that spans six agentd restarts (rolling upgrades,
  node drains) fails although nothing broke.
- The chart sets no `strategy` and no `terminationGracePeriodSeconds` on the three Deployments
  (`charts/koinon/templates/deployments.yaml:8-40`): Kubernetes' defaults apply (RollingUpdate with
  25% surge and unavailability, 30 s grace). The default run wall clock is 30 minutes (ADR 0081), the
  lease 30 s (`internal/agentd/agentd.go:200`).

### 2. Schema compatibility across versions

- Migrations are forward-only goose files without `Down` sections (no file in
  `internal/db/migrations/` has one; ADR 0027 line 23). They run as a Job **before** the new pods
  start (ADR 0060: `pre-upgrade` hook, Argo Sync wave 1), so during every rolling upgrade the old
  pods run against the new schema, and after an app-only rollback the old pods run against it too.
- `00020_tasks.sql` (landed 2026-10-26, first in v0.1.0-rc.3) renames in place: `tickets` to
  `tasks` (line 26), `ticket_events` to `task_events` (27), the `ticket_id` columns of six tables
  (74-79), `done_at` to `finished_at` (46) and `created_by` to `created_by_user_id` (52), and renames
  constraints, indexes and triggers (101-123); it drops `tickets.status` and the type
  `ticket_status` (42-43). A pod of the previous release fails on its
  first query against any of these, during the rollout and after a rollback alike.
- Two later migrations already follow expand/contract: `00014_fold_model_rates_into_prices.sql`
  keeps the old `models` rate columns unread "for one release (expand, then contract) so a rolling
  upgrade does not break the old pods" (lines 9-11, 77-79); `00036_notify_targets.sql` keeps
  `questions.asked_to` nullable and deprecated with triggers that translate an old pod's insert
  (lines 69-75). Neither practice is written down as a rule, and nothing checks it.
- ADR 0027 asks for "an upgrade test from the previous release"; none exists (no upgrade test in
  `test/` or `.github/workflows/`). ADR 0060 (last consequence) left a schema-version readiness gate
  out; no binary checks the schema version it runs against (only the migrate command opens goose,
  `cmd/koinon-api/main.go:798`).

### 3. Supervisor updates

- The reconciler recreates a sandbox when its spec hash changes
  (`internal/reconciler/reconciler.go:1131-1145`, after held runs finish and the home gate passes).
  `specHash` covers the image, runtime class, resources, command, providers, filesystem policy and
  labels (`internal/reconciler/render.go:158-178`). It does not cover the OpenShell supervisor
  image: that is set by the OpenShell gateway's `supervisor_image` (read only by the preflight,
  `internal/preflight/sandboximages.go:32`, `:174`) and admitted by `sandboxPolicy.supervisorImages`
  (`charts/koinon/values.yaml:593-602`, ADR 0045).
- So after an OpenShell upgrade (ADR 0077 pins it per Koinon release), an existing sandbox keeps the
  supervisor it was created with until something else recreates it. A supervisor fix (security or
  otherwise) never reaches a long-lived agent, the fleet runs mixed supervisor versions, and features
  Koinon needs from the new supervisor can be missing: ADR 0032's restore fails at once on "a
  supervisor without the subsystem" (ADR 0032, lines 266-267).
- Not verified here: whether the OpenShell driver, when it recreates a supervisor pod (node drain),
  uses the gateway's current image or the one recorded at sandbox creation, and how that interacts
  with a `supervisorImages` list that no longer admits the old image.

### 4. DBOS workflow version and executor ID (#331)

- koinon-api starts DBOS with `ApplicationVersion: project.Version`
  (`cmd/koinon-api/main.go:480-484`) and no `ExecutorID`, which DBOS v1.7.0 defaults to `local`
  (`dbos/dbos.go:173-174` in the module). On `Launch` DBOS recovers only the pending workflows of its
  own executor ID **and** its own application version (`dbos/dbos.go:904`, `dbos/recovery.go:9-11`).
- Four workflows are registered today: the schedule fire (`internal/triggers/schedule/schedule.go:144`),
  notifications (`internal/durable/notify.go:97`), run-now (`internal/services/runnow_durable.go:59`)
  and direct messages (`internal/services/directmessage_durable.go:57`).
- Consequences: every release bump orphans the workflows pending at the upgrade (a scheduled fire, a
  notification, a run-now or a direct message is silently lost). And with a surge pod or
  `api.replicas` > 1 (default 1, `charts/koinon/values.yaml`), two live processes share the executor
  ID `local`, so a new pod may re-run workflows the old pod is still running.
- ADR 0066 (lines 223-229) called fixing both "a prerequisite for TR2/TK4, before any workflow is
  registered". DBOS also reads `DBOS__VMID` and `DBOS__APPVERSION` from the environment
  (`dbos/dbos.go:165-166`), and exposes `RecoverPendingWorkflows(executorIDs)` and `ForkWorkflows`
  with an `ApplicationVersion` override for the carry-over.
- koinon-api stops DBOS with a 30 s timeout (`cmd/koinon-api/main.go:517-521`), the same as the pod's
  default grace period.

### 5. KEK custody and loss (#332)

- The KEK Secret is created by koinon-api on first start and never by the chart; a missing Secret
  with encrypted values present stops koinon-api (`docs/ops/secret-keys.md:21-32`). Backing it up is
  a manual `kubectl get secret` (`docs/ops/secret-keys.md:46-62`; quickstart "Back up the encryption
  keys").
- The `keys` tool covers every envelope column (`internal/keyscli/keyscli.go:59-70`), including
  `home_repositories.password_envelope` and both trace-destination tables. The runbook's "What is
  where" table (`docs/ops/secret-keys.md:12-17`) and its loss text (lines 57-59) list only provider
  keys, connections, MCP credentials and saved work.
- Each agent's kopia repository password exists only in `home_repositories.password_envelope`
  (`internal/db/migrations/00025_home_checkpoints.sql:9-16`; ADR 0032 lines 134-137). **Losing the
  KEK therefore loses every home checkpoint**, not only the provider keys. Trace-destination
  credentials are lost the same way (`00042_trace_destinations.sql:14-29`).
- `keys reencrypt` after a compromise (`internal/keyscli/keyscli.go:292-305`) reseals the kopia
  passwords under new DEKs but does not change them, so a leaked password still opens the
  repository for anyone who also reaches the bucket.
- Nothing checks that the active KEK is in a backup. ADR 0076 (decision 5) copies the KEK into each
  standalone backup set, but that mode and CNPG backups (decision 9) are delivered after v0.1.0; the
  `cnpg` template configures no backups today (ADR 0076, line 32).

### 6. Restore

- There is no documented restore procedure for the modes v0.1.0 ships (`cnpg`, `external`). ADR
  0076's procedure (decision 6: scale to zero, restore the KEK, `pg_restore`, migration Job, scale
  up) belongs to standalone mode, delivered later.
- A restore has three parts that must agree: the database (including the `dbos` schema), the KEK
  Secret, and the checkpoint bucket. The bucket is not restored with the database, so after a
  restore `home_checkpoints` can miss snapshots newer than the dump, and can name snapshots that
  kopia retention (24 latest, 14 daily, 8 weekly; ADR 0032 line 137) has since deleted. agentd
  restores newest first and falls back to older checkpoints (`internal/agentd/checkpoint.go:292`).
- No restore has been run end to end on any install.

## Decision

Only what the scope decision of 2026-10-31, the issues and the accepted ADRs already settle. The
rest is under Open questions.

1. **Gate.** v0.1.0 is not tagged until the implementation issues of this ADR are done and the
   restore drill (decision 7) has passed on the home lab.
2. **agentd drains on SIGTERM.** A stopping agentd claims no new run, and a run it gives up because
   it is stopping is a **planned handoff**: it does not count as a takeover, neither in
   `runs.takeovers` (and so not against `MaxTakeovers`) nor in `koinon_agentd_takeovers_total`.
   Takeovers keep meaning "the owner was lost". How long agentd waits before handing off is open
   (Q1); how a handoff is recorded is Q2.
3. **N-1 schema compatibility through expand/contract.** The release before must keep working on
   the new schema: a rename or a drop is split into an expand migration (add the new shape, keep the
   old one working, bridge writes where needed) and a contract migration in a later release.
   00014 and 00036 are the pattern. Migrations stay forward-only (ADR 0027): a rollback deploys the
   previous release's images on the current schema; it never runs a `Down`. The size of the window
   and how it is enforced are Q4 to Q7.
4. **Supervisor updates reach existing sandboxes.** The supervisor a sandbox runs is part of what
   Koinon tracks about it; an OpenShell upgrade must not leave sandboxes on the old supervisor until
   something unrelated recreates them. The mechanism is Q8, the upgrade order Q9.
5. **DBOS: no orphaned and no doubled workflows** (#331, ADR 0066's prerequisite). Workflows pending
   at an upgrade are recovered by the new release, and no two live processes share an executor ID.
   Both hold before v0.1.0. A metric of pending workflows by workflow version is added, with a fixed
   label set (ADR 0023). The version scheme is Q10, the executor identity Q11.
6. **KEK custody and loss** (#332). The runbook lists every envelope the `keys` tool covers,
   including `home_repositories.password_envelope` and the trace-destination credentials, and says
   that losing the KEK loses every home checkpoint. After a compromise the kopia passwords are
   changed, not only resealed. The KEK is backed up in every database mode, and a check reports when
   the active KEK is not in the latest backup (`koinon_kek_backup_fingerprint_match`). The custody
   model and the loss procedure are Q12 to Q14.
7. **A documented restore drill** runs on the home lab before v0.1.0: from a backup of the database
   and the KEK to a working install, with its steps, timings and findings recorded. Its scope and
   the backup it starts from are Q15 to Q18.

## Open questions for Andre

Each question lists the options, the sustainable one first, and a recommendation. What the cheaper
option costs later is stated with it.

### Drain (scope item 1)

- **Q1. What does a stopping agentd do with a turn in progress?**
  - (a) Finish the turn, up to a drain deadline (a chart value, default 5 minutes; the pod's
    `terminationGracePeriodSeconds` is set to the deadline plus 30 s), then hand off. It stops
    claiming at once and keeps heartbeating while it drains.
  - (b) Hand off at once: send an ACP cancel, release the lease as planned, and let the new owner
    reload the session. Upgrades finish in seconds, but every upgrade interrupts every running turn:
    the model call is paid twice and a tool call in flight may be repeated (the resume note only asks
    the model not to).
  - (c) Drain without a deadline (grace = the run wall clock, 30 minutes or more). Never interrupts,
    but a rollout or node drain can take half an hour per pod.
  - Recommendation: (a). A turn that outlives the deadline gets (b)'s handoff.
- **Q2. How is a planned handoff recorded, so it is not a takeover?**
  - (a) `ReleaseRunLease` takes a reason; a planned release stamps the run (for example
    `handoff_at`), `TakeOverExpiredRun` increments `takeovers` only for an unstamped expired lease
    and counts a stamped one in a new `runs.handoffs` column and a metric
    `koinon_agentd_handoffs_total{reason}`. The run timeline gets a `run.handed_off` event.
  - (b) Keep one counter and raise `MaxTakeovers`. Cheap, but a crash loop and a busy upgrade week
    look the same, and the cap stops protecting against a harness that keeps dropping.
  - Recommendation: (a).
- **Q3. Rollout settings for agentd.** (a) Explicit `RollingUpdate` with `maxSurge: 1`,
  `maxUnavailable: 0`, plus a PodDisruptionBudget when `replicas` > 1, so a drain never takes the
  last worker away; (b) `Recreate`, which stops all work during each upgrade. Recommendation: (a).

### Schema compatibility (scope item 2)

- **Q4. How wide is the window?**
  - (a) Every release N runs on the schema of N+1: an expand lands in N+1, its contract no earlier
    than N+2, across minors and patches alike. A one-step rollback is always an image rollback.
  - (b) The promise holds within a minor line only; a minor may break it, with documented downtime
    (scale to zero, migrate, scale up). Cheaper to write migrations, but every minor upgrade is an
    outage and has no rollback short of a restore.
  - (c) No promise before 1.0. Costs the same as (b) on every release, and the habit is harder to
    start later.
  - Recommendation: (a).
- **Q5. How is it enforced?**
  - (a) A CI job, which is also ADR 0027's missing upgrade test: install the previous release,
    run the new migration Job, then run the previous release's smoke test and scenarios against the
    new schema (previous images by tag), and finally the new release's. Plus a migration lint that
    rejects `RENAME`, `DROP COLUMN`, `DROP TABLE` and `DROP TYPE` outside a migration marked as a
    contract, naming the release whose expand it completes.
  - (b) The lint only: catches the obvious statements, misses a changed constraint or a column an
    old pod writes that the new schema rejects.
  - (c) Review only.
  - Recommendation: (a). The kind e2e need not run it per PR; the smoke and scenario layers suffice.
- **Q6. What about 00020 and the rc's already published?**
  - (a) The promise starts at v0.1.0: the v0.1.0 schema is the first baseline, and the release notes
    say an rc install upgrades to v0.1.0 with a stop (scale to zero, migrate, scale up) and cannot
    roll back to an rc.
  - (b) Retrofit compatibility for installs older than rc.3 (views named after the old tables and columns, plus
    triggers). Work for installs that only the maintainers have.
  - Recommendation: (a).
- **Q7. Do binaries check the schema version they run on?** (ADR 0060 left this out.)
  - (a) Each binary declares the range of goose versions it supports (its own and the next release's
    expands) and stays not Ready outside it, with a clear log line; the migration Job refuses to
    migrate past a contract when pods older than the contract are still running.
  - (b) No gate: a rollback two releases back fails at its first query instead of at start.
  - Recommendation: (a), the readiness part before v0.1.0; the Job's check can follow.

### Supervisor (scope item 3)

- **Q8. How does a new supervisor reach existing sandboxes?**
  - (a) The reconciler reads the gateway's `supervisor_image` (as the preflight does) and adds it to
    `specHash`. A change then recreates each sandbox through the existing path: after held runs
    finish, behind the home gate, with a checkpoint first. Add a cap on how many sandboxes are
    recreated at once, so an OpenShell upgrade does not recreate the whole fleet in one pass.
  - (b) A separate supervisor-generation label and a dedicated roll with its own pacing and status.
    More control, more code, and a second recreate path to keep correct.
  - (c) Leave it to natural recreation and document it. Costs: a supervisor security fix never
    reaches a long-lived agent; mixed fleets; restore features missing on old sandboxes.
  - Recommendation: (a) with the cap. First verify the open point in Context item 3 (which image the
    driver uses when it recreates a supervisor pod).
- **Q9. Upgrade order between OpenShell and Koinon.**
  - (a) Koinon release N supports the OpenShell pins of N and N-1 (ADR 0077 already moves one line at
    a time), so either order works; the upgrade test of Q5 covers both orders.
  - (b) A fixed order (OpenShell first), documented and checked by the preflight. Simpler to test,
    but an operator who upgrades in the wrong order gets a failed preflight mid-maintenance.
  - Recommendation: (a).

### DBOS (scope item 4, #331)

- **Q10. What is the workflow version?**
  - (a) A fixed compatibility version in code (`durable.WorkflowVersion`, for example `wf1`),
    bumped only when a workflow's step sequence changes incompatibly. On a bump, a startup sweep in
    koinon-api finds pending workflows of older versions and forks each from step 0 under the current
    version (`ForkWorkflows` with `ApplicationVersion`), which needs every workflow to be idempotent
    from the start: verify that for the four registered workflows (their dedupe keys), and make it a
    rule for new ones.
  - (b) Keep the release version and run the same sweep on every upgrade. Every upgrade replays
    every pending workflow from scratch, and one that changed shape fails with DBOS's unexpected-step
    error.
  - (c) DBOS's default (a hash of the binary). Orphans workflows on every build, as `durable.go`
    already warns.
  - Recommendation: (a), with a test that fails when a registered workflow's steps change without a
    version bump (a recorded step list per workflow).
- **Q11. Executor identity, and who recovers a dead executor's workflows?**
  - (a) Executor ID = pod name (`DBOS__VMID` from the downward API, or `Config.ExecutorID`), each
    replica heartbeats its ID into a small table, and one leader-elected replica calls
    `RecoverPendingWorkflows` for executor IDs whose heartbeat is stale. Needed because a pod name
    is never reused, so without the sweep a crashed pod's workflows are never recovered.
  - (b) koinon-api as a StatefulSet: stable IDs (`koinon-api-0`, ...), and each pod recovers its own
    on restart. No sweep, but no surge (a StatefulSet replaces a pod by delete then create), a
    scaled-down ordinal's workflows are still orphaned, and the API changes workload kind.
  - (c) The durable runtime stays at one replica with `Recreate`. Cheapest; costs a short API outage
    on every upgrade and blocks API HA.
  - Recommendation: (a).

### KEK (scope item 5, #332)

- **Q12. Custody model: how does the KEK reach a backup, and how does Koinon know?**
  - (a) A `koinon-api keys export` command that writes the KEK set to an operator-chosen target
    (a file encrypted to an age recipient, or a Secret in another namespace or cluster) and records
    the exported key fingerprints and time in the database. The metric
    `koinon_kek_backup_fingerprint_match` compares the active KEK with the last export (and, in
    standalone mode, with the latest backup set of ADR 0076). `keys add` and `keys activate` print a
    reminder until the next export.
  - (b) Keep the manual `kubectl get secret` and only add the metric from what the backup Job sees.
    The metric then cannot work in `cnpg` and `external` modes, which are the v0.1.0 modes.
  - (c) A KMS adapter now (the envelope already carries `kek_id`). The sustainable end state for
    clouds, but more than v0.1.0 needs; (a) does not block it.
  - Recommendation: (a) for v0.1.0, (c) as a later option.
- **Q13. What happens when a KEK is lost for good?**
  - (a) A `keys forget <kek-id>` command, after a confirmation, marks every value wrapped by it as
    lost: providers, connections, MCP and trace credentials show "credential lost, enter it again";
    each affected agent gets a new kopia repository (new password and prefix) and a new home
    generation (ADR 0051), and its old repository is deleted. `keys status` lists what was lost.
  - (b) A runbook with SQL to clear the columns by hand. Error-prone, and agents with an unreadable
    repository fail every checkpoint until someone notices.
  - (c) Reinstall. Loses everything, including the data the KEK never protected.
  - Recommendation: (a).
- **Q14. How are kopia passwords changed after a compromise?**
  - (a) A new repository per agent: the writer copies the latest snapshot into a fresh repository
    with a new password, then deletes the old one, and the S3 credentials are rotated. Changes what
    an attacker would need. History older than the latest snapshot is lost.
  - (b) kopia's password change on the existing repository. To verify before relying on it: if it
    only rewraps the repository's key, a copy of the repository taken before the change still opens
    with the old password.
  - (c) Rotate the S3 credentials only. Cheap; protects only as long as the attacker has no copy of
    the bucket.
  - Recommendation: (a), offered as `keys reencrypt --rotate-checkpoints`; (b) only if the
    verification shows it really rekeys.

### Restore drill (scope item 6)

- **Q15. What does the v0.1.0 drill cover?**
  - (a) A full restore into a fresh namespace on the home lab: a dump of the CNPG database (the
    `dbos` schema included) and the KEK export, the checkpoint bucket left as it is. Steps: restore
    the KEK, `pg_restore` into an empty database, run the migration Job, scale up. Checks: sign in,
    a provider key opens (a run against the fake model or the echo check, ADR 0072), a schedule
    trigger fires and a pending workflow from before the dump is recovered, an archived agent
    restores its home and reloads its session. Record the time taken and the data lost (RTO, RPO);
    no targets yet.
  - (b) Database and KEK only, without checkpoints and workflows. Leaves the two parts most likely
    to fail (Context item 6) unproven.
  - (c) A tabletop review of the document. Proves nothing runs.
  - Recommendation: (a).
- **Q16. Which backup does the drill start from, given ADR 0076's backups land after v0.1.0?**
  - (a) A documented manual procedure that runs the same commands ADR 0076's CronJob will run
    (`pg_dump --format=custom`, plus `keys export` from Q12), for every mode. The CronJob later
    automates the same steps, so nothing is rewritten.
  - (b) Bring ADR 0076's backup CronJob and CNPG `ScheduledBackup` forward into v0.1.0. More scope
    at the gate.
  - (c) Leave backup to the operator and document only what to back up. Costs: no tested restore
    path of Koinon's own; the drill proves only the maintainers' setup.
  - Recommendation: (a).
- **Q17. How are the database and the checkpoint bucket made to agree after a restore?**
  - (a) A `koinon-api checkpoints resync` step in the restore procedure: it lists each agent's kopia
    snapshots (the bucket is the truth for what exists) and adds or marks missing rows in
    `home_checkpoints`, so the newest real snapshot is restored and deleted ones are never tried.
  - (b) Rely on agentd's newest-first fallback. Works, but a restore can bring back an older home
    than the bucket holds, and every missing snapshot costs a failed attempt.
  - (c) Back up the bucket with the database at the same moment. Doubles storage and still drifts by
    the dump's duration.
  - Recommendation: (a).
- **Q18. Cadence after v0.1.0.** (a) The restore runs as an automated test in the kind e2e (nightly),
  and the home-lab drill is repeated before each final minor release; (b) only before v0.1.0.
  Recommendation: (a).

## Alternatives considered

- **Down migrations for rollback.** Rejected for the policy: ADR 0027 chose forward-only, a `Down`
  that drops data cannot be undone, and an image rollback on an expanded schema needs none.
- **Stop-the-world upgrades** (scale to zero, migrate, scale up) as the only supported path. Simpler
  migrations, but every upgrade is an outage and a rollback needs a restore. Kept only for the rc to
  v0.1.0 step (Q6 (a)) _(inferred)_.
- **Counting planned handoffs as takeovers and raising the cap.** Covered in Q2 (b).

## Consequences

- Positive: an upgrade no longer loses scheduled fires and notifications, does not fail long runs
  with "connection lost", and can be undone by deploying the previous images.
- Positive: an operator knows what to back up, can prove a restore works, and knows what a KEK loss
  costs before it happens.
- Negative: every rename or drop takes two releases and a bridge (triggers or dual writes), and a
  CI job runs the previous release against each new schema _(inferred)_.
- Negative: agentd pods take up to the drain deadline to stop, so rollouts and node drains are
  slower (Q1) _(inferred)_.
- Negative: an OpenShell upgrade recreates every sandbox over time, each with a checkpoint and a
  restore (Q8) _(inferred)_.
- Metrics follow ADR 0023: handoffs, pending workflows and the KEK backup match carry fixed labels
  (reason, workflow version), never org, agent, run or ticket IDs.

## Implementation sub-issues

Titles only; each is opened after this ADR is accepted, scoped by the answers to the open questions.

1. agentd: drain on SIGTERM (stop claiming, finish the turn within a deadline, planned handoff)
2. runs: record planned handoffs apart from takeovers (`handoffs`, `run.handed_off`, metric)
3. chart: agentd rollout strategy, termination grace and PodDisruptionBudget
4. migrations: expand/contract rule in AGENTS.md and a contract-marker lint in `hack/migrate-lint.sh`
5. ci: N-1 upgrade test (previous release's smoke and scenarios on the new schema)
6. koinon-api, agentd, reconciler: schema-version readiness gate
7. release notes: rc to v0.1.0 upgrade with a stop; v0.1.0 as the compatibility baseline
8. reconciler: supervisor image in the sandbox spec hash, with a recreate cap
9. preflight and upgrade test: support OpenShell pins N and N-1 in either upgrade order
10. durable: fixed workflow version, step-list test and the startup carry-over sweep (#331)
11. durable: executor ID per pod, executor heartbeats and dead-executor recovery (#331)
12. durable: `koinon_durable_workflows_pending{version}` metric and alert (#331)
13. keys: `keys export`, export records and `koinon_kek_backup_fingerprint_match` (#332)
14. keys: `keys forget` for a lost KEK (credentials marked lost, new kopia repositories)
15. keys: rotate checkpoint repositories after a compromise (#332)
16. docs: secret-keys runbook covers home repositories and trace credentials, loss and compromise (#332)
17. docs: backup and restore procedure for the cnpg and external modes
18. checkpoints: `checkpoints resync` after a database restore
19. home lab: run the v0.1.0 restore drill and record the results
20. e2e: automated restore test in the nightly kind run

## Links

- Issues #336, #331, #332; Andre's scope decision of 2026-10-31 (in #336)
- Research store: deep dive G9, `g9-day2-operations`
- DBOS Transact Go v1.7.0 (the module in `go.mod`): `dbos/dbos.go`, `dbos/recovery.go`,
  `dbos/workflow.go` (`ForkWorkflowsInput`)
- ADRs [0027](0027-repo-and-versioning.md), [0030](0030-run-state-machine.md), [0032](0032-agent-home-checkpoints.md), [0051](0051-session-home-generations.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0060](0060-migrations-as-a-job.md), [0066](0066-triggers-and-scheduling.md), [0076](0076-database-modes.md), [0077](0077-pinned-openshell-and-installer.md)
- Runbook: [docs/ops/secret-keys.md](../ops/secret-keys.md)
