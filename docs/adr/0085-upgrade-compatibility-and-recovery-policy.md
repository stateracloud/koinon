# 0085. Upgrade, compatibility and recovery policy

- Status: Accepted (scope decided by procoduck, 2026-10-31; decisions of 2026-11-01; a v0.1.0 gate)
- Date: 2026-11-01
- Relates to: [0027](0027-repo-and-versioning.md) (forward-only migrations, upgrade test), [0030](0030-run-state-machine.md) (leases, epoch, takeover), [0032](0032-agent-home-checkpoints.md) (home checkpoints), [0056](0056-model-providers-pricing-and-budgets-per-org.md) (KEK and envelopes), [0060](0060-migrations-as-a-job.md) (migration Job), [0066](0066-triggers-and-scheduling.md) (DBOS), [0076](0076-database-modes.md) (database modes, backups), [0077](0077-pinned-openshell-and-installer.md) (OpenShell pins), [0045](0045-openshell-hardening-defaults.md) (pinned supervisor image)
- Issue: #336; covers #331 and #332

## Context

Deep dive G9 (research store, `g9-day2-operations`) looked at what happens to a running Koinon
install when it is upgraded, rolled back or restored. procoduck decided on 2026-10-31 to write one policy
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
- Verified for the decision: OpenShell 0.1.5 reads `[drivers.kubernetes].supervisor_image` live on
  every sandbox create and start and never touches running supervisor pods (decision 9).

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

procoduck decided all of it in a grilling round on 2026-11-01 (the questions are numbered Q1 to Q18 in
the draft of this ADR and in #336). Where he chose against the recommendation, the Alternatives
section says what the recommended option would have bought.

1. **Gate.** v0.1.0 is not tagged until the implementation issues of this ADR are done and the
   restore drill (decision 15) has passed on the home lab.

### agentd

2. **Drain on SIGTERM** (procoduck, 2026-11-01). A stopping agentd stops claiming new runs at once,
   keeps heartbeating, and finishes the turn in progress up to a **drain deadline** (a chart value,
   default 5 minutes). Then it hands the run off. The pod's `terminationGracePeriodSeconds` is the
   deadline plus 30 s.
3. **Planned handoffs are not takeovers** (procoduck, 2026-11-01). `ReleaseRunLease` takes a reason. A
   planned release counts in a new `runs.handoffs` column, not in `runs.takeovers`, so it does not
   count against `MaxTakeovers`. The run timeline gets a `run.handed_off` event, and the metric
   `koinon_agentd_handoffs_total{reason}` counts them with a fixed label set (ADR 0023).
   `koinon_agentd_takeovers_total` keeps meaning "the owner was lost".
4. **Rollout** (procoduck, 2026-11-01). The agentd Deployment uses `RollingUpdate` with `maxSurge: 1`
   and `maxUnavailable: 0`, plus a PodDisruptionBudget when `replicas` > 1, so a drain never takes
   the last worker away.

### Schema

5. **Compatibility is promised within a minor line only** (procoduck, 2026-11-01; he chose against the
   recommendation of a two-release window). Patch releases of one minor line (v0.1.x) roll: the old
   and the new binaries share the schema, so a rolling upgrade works and a rollback is an image
   rollback. Every **minor** upgrade (v0.1 to v0.2) is a documented **stop, migrate, start**: scale
   to zero, run the migration Job, scale up. There is no app-only rollback across a minor; going
   back means a restore (decision 15). Expand/contract stays good practice (00014 and 00036 are the
   pattern), but a contract may land in the next minor. Migrations stay forward-only (ADR 0027):
   nothing runs a `Down`.
6. **Enforcement** (procoduck, 2026-11-01). (a) A CI lint rejects `RENAME`, `DROP COLUMN`, `DROP TABLE`,
   `DROP TYPE` and column type changes in migrations that land within a minor line (patch releases
   and release branches). A new minor line may contain them. (b) A CI test installs the previous
   patch release's images, runs the new migration Job and runs the previous patch's smoke test on
   the new schema. This is also ADR 0027's missing upgrade test, for patches. Minor upgrades are
   covered by the documented stop, migrate, start and by the restore test (decision 18), not by an
   N-1 test.
7. **The promise starts at v0.1.0** (procoduck, 2026-11-01). The v0.1.0 schema is the first baseline.
   An rc install upgrades to v0.1.0 with a documented stop and cannot roll back to an rc. There is
   no retrofit for 00020 or older installs.
8. **Binaries gate on the schema version** (procoduck, 2026-11-01). Each binary declares the range of
   goose versions it supports and stays not Ready, with a clear log line, outside it. The migration
   Job's own check (refusing to migrate past what running pods support) can follow.

### Supervisor and OpenShell

9. **A new supervisor reaches existing sandboxes through the spec hash** (procoduck, 2026-11-01). The
   reconciler reads the gateway's `supervisor_image` (as the preflight does) and adds it to
   `specHash`. A change then recreates each sandbox through the existing path: held runs finish,
   the home gate passes, a checkpoint is taken first. A cap on recreates per reconcile pass keeps an
   OpenShell upgrade from recreating the whole fleet at once.
   Verified: OpenShell 0.1.5 reads `[drivers.kubernetes].supervisor_image` live on every sandbox
   create and start (`crates/openshell-driver-kubernetes/src/driver.rs:2551` and `:3368` at v0.1.5)
   and never touches running supervisor pods. So a recreated sandbox always gets the gateway's
   current image, and only a recreate moves a sandbox to a new supervisor.
10. **OpenShell N and N-1** (procoduck, 2026-11-01). Each Koinon release supports its pinned OpenShell
    (ADR 0077) and the previous pin, tested in CI against both. Either can be upgraded first.

### DBOS (#331)

11. **Workflow version and executor identity** (procoduck, 2026-11-01).
    - A fixed `durable.WorkflowVersion`, bumped only when a workflow's steps change incompatibly,
      replaces the release version as DBOS's `ApplicationVersion`. A startup sweep re-runs pending
      workflows of older versions from step 0 (`ForkWorkflows` with the current
      `ApplicationVersion`). A test fails when a registered workflow's steps change without a bump
      (a recorded step list per workflow). Idempotence of the four registered workflows from step 0
      (their dedupe keys) is verified during implementation, and is a rule for new ones.
    - Executor ID = pod name (`DBOS__VMID` from the downward API or `Config.ExecutorID`). Each
      executor heartbeats into a table, and a leader-elected job recovers the pending workflows of
      executor IDs whose heartbeat is stale (a pod name is never reused, so without it a crashed
      pod's workflows are never recovered). koinon-api stays a Deployment, so it can run HA.
    - A metric of pending workflows by workflow version, with a fixed label set (ADR 0023).

### KEK and recovery (#332)

12. **KMS adapter now, for v0.1.0** (procoduck, 2026-11-01; he chose against the recommendation of an
    export command first). Koinon gets a key-wrapping interface with two adapters: Vault/OpenBao
    transit and AWS KMS. GCP and Azure follow behind the same interface. The envelope already
    carries `kek_id`. The local Secret-held KEK stays for kind and development (its backup is the
    documented key export). The fingerprints of the wrapping keys are recorded in the database, and
    a metric alerts when the active key is unreachable. The runbook (`docs/ops/secret-keys.md`)
    lists every envelope the `keys` tool covers, including `home_repositories.password_envelope`
    and the trace-destination credentials, says that losing the key loses every home checkpoint,
    and covers both the KMS and the Secret-held KEK.
13. **A lost key: `koinon keys forget <id>`** (procoduck, 2026-11-01). It marks every credential wrapped
    by that key as lost: provider, connection, MCP and trace credentials show "credential lost,
    enter it again", and people re-enter provider keys. Each affected agent gets a new kopia
    repository and a new home generation (ADR 0051). The command is audited and needs no SQL.
14. **A compromise: new kopia repositories** (procoduck, 2026-11-01). Each agent gets a new kopia
    repository (a new master key) built from its latest snapshot, and the S3 credentials are
    rotated. History older than that snapshot is dropped.
    Verified: kopia v0.23.1 `repository change-password` only re-wraps `kopia.repository` and
    `kopia.blobcfg` (`repo/format/format_change_password.go:13-43`), so it does not revoke: a copy
    of the repository taken earlier still opens with the old password. `keys reencrypt` likewise
    reseals the password but does not change it.
15. **The restore drill** (procoduck, 2026-11-01) is a full restore into a fresh namespace from a CNPG
    dump (the `dbos` schema included) plus the KEK or KMS key, the checkpoint bucket left as it is.
    Checks: sign in; a provider key decrypts; a schedule fire; recovery of a workflow that was
    pending before the dump; an archived agent's home and session restore. It records the time
    taken (RTO) and the data lost (RPO); there are no targets yet.
16. **The drill starts from a manual backup** (procoduck, 2026-11-01) that uses exactly the commands ADR
    0076's backup job will automate: `pg_dump --format=custom` plus the key export. The job later
    automates the same steps.
17. **`checkpoints resync`** (procoduck, 2026-11-01). A command rebuilds `home_checkpoints` from each
    agent's kopia snapshot list (the bucket is the truth for what exists). It is part of the
    restore runbook and of the drill.
18. **Cadence** (procoduck, 2026-11-01). An automated backup-and-restore step runs in the nightly kind
    e2e, and the full home-lab drill runs before each final minor release.

## Open questions

Implementation-level unknowns, to settle in the sub-issues:

- Whether all four registered workflows are idempotent from step 0 (decision 11); a workflow that
  is not gets a dedupe key or a version bump rule before v0.1.0.
- The exact shape of the key-wrapping interface (wrap, unwrap, key identity and reachability
  probe) and how an adapter's credentials reach koinon-api without entering a sandbox.
- Resolved (detection of the decision 6 rule): the lint reads a marker file, `hack/migrate-patch-line`
  (`none` or the shipped minor line, for example `v0.1`), at the merge base. Patch tags are cut from
  `main` (the release gate requires an ancestor of `origin/main`), so the marker, not a branch name,
  makes the rule bite: it is set when a minor line's `.0` ships and reset to `none` in the change that
  opens the next minor. A base ref named `release/*` and `MIGRATE_LINT_PATCH_LINE=1` also enable it.

## Alternatives considered

- **Down migrations for rollback.** Rejected: ADR 0027 chose forward-only, and a `Down` that drops
  data cannot be undone.
- **Handoff: at once (ACP cancel) or without a deadline.** At once pays the model call twice and may
  repeat a tool call on every upgrade; without a deadline a rollout can take half an hour per pod.
- **Handoffs counted as takeovers with a higher `MaxTakeovers`.** A crash loop and a busy upgrade
  week would look the same, and the cap would stop protecting against a harness that keeps dropping.
- **`Recreate` for agentd.** Stops all work during every upgrade.
- **Schema window of two releases (every N runs on N+1's schema), across minors.** The recommended
  option: a one-step rollback would always be an image rollback. Rejected by procoduck for the cost of
  writing expand/contract pairs on every minor; the price is that each minor upgrade is an outage
  and has no rollback short of a restore.
- **No schema promise before 1.0.** Costs the same as the minor-line rule on every release, and the
  habit is harder to start later.
- **Enforcement by lint only, or by review only.** A lint alone misses a changed constraint or a
  column an old pod writes that the new schema rejects; review does not scale.
- **Retrofit for installs older than rc.3** (views and triggers named after the old tables). Work
  for installs only the maintainers have.
- **No schema-version gate.** A rollback past what the schema supports fails at its first query
  instead of at start.
- **A separate supervisor-generation label and roll; or natural recreation only.** A second recreate
  path to keep correct; and a supervisor security fix would never reach a long-lived agent.
- **A fixed OpenShell upgrade order.** An operator who upgrades in the wrong order gets a failed
  preflight mid-maintenance.
- **Release version as the DBOS version, or DBOS's binary hash.** Every upgrade would replay every
  pending workflow, or orphan them on every build.
- **koinon-api as a StatefulSet, or one replica with `Recreate`.** A StatefulSet has no surge and
  still orphans a scaled-down ordinal's workflows; one replica costs an API outage per upgrade and
  blocks HA.
- **`keys export` plus a backup-match metric instead of a KMS adapter** (the recommendation).
  Cheaper for v0.1.0 and does not block a KMS later; procoduck preferred the end state for cloud
  installs now.
- **A manual SQL runbook, or a reinstall, for a lost key.** SQL by hand is error-prone and leaves
  agents with unreadable repositories failing every checkpoint; a reinstall loses data the key never
  protected.
- **kopia password change, or S3 credential rotation only, after a compromise.** The first does not
  revoke (verified above); the second protects only while the attacker has no copy of the bucket.
- **A drill of database and KEK only, or a tabletop review.** They leave checkpoints and workflows,
  the parts most likely to fail, unproven.
- **Bringing ADR 0076's backup jobs forward, or leaving backup to the operator.** More scope at the
  gate; or no tested restore path of Koinon's own.
- **Relying on agentd's newest-first fallback, or backing up the bucket with the database.** The
  first can restore an older home than the bucket holds and wastes an attempt per missing snapshot;
  the second doubles storage and still drifts by the dump's duration.
- **A drill only before v0.1.0.** Restore would rot unnoticed.

## Consequences

- Positive: a patch upgrade no longer loses scheduled fires and notifications, does not fail long
  runs with "connection lost", and can be undone by deploying the previous images.
- Positive: an operator knows what to back up, can prove a restore works, and knows what a key loss
  costs before it happens. A cloud install keeps its KEK in a KMS from the first release.
- Negative: every minor upgrade is an outage (stop, migrate, start) with no app-only rollback.
- Negative: CI gains a lint and a previous-patch test, and the KMS interface with two adapters is
  v0.1.0 scope _(inferred)_.
- Negative: agentd pods take up to the drain deadline to stop, so rollouts and node drains are
  slower _(inferred)_.
- Negative: an OpenShell upgrade recreates every sandbox over time, each with a checkpoint and a
  restore _(inferred)_.
- Negative: a compromise drops checkpoint history older than the latest snapshot.
- Metrics follow ADR 0023: handoffs, pending workflows and key reachability carry fixed labels
  (reason, workflow version, key backend), never org, agent, run or ticket IDs.

## Implementation sub-issues

Titles only here; each is opened after this ADR is accepted.

1. agentd: drain on SIGTERM (stop claiming, finish the turn within a deadline, planned handoff)
2. runs: record planned handoffs apart from takeovers (`handoffs`, `run.handed_off`, metric)
3. chart: agentd rollout strategy, drain deadline, termination grace and PodDisruptionBudget
4. migrations: patch-line rule in AGENTS.md and a lint banning RENAME, DROP and type changes within a minor line
5. ci: previous patch release's smoke test on the new schema
6. koinon-api, agentd, reconciler: schema-version readiness gate
7. docs and release notes: minor upgrade as stop, migrate, start; rc to v0.1.0 stop; v0.1.0 as the baseline
8. reconciler: supervisor image in the sandbox spec hash, with a recreate cap
9. preflight and ci: support OpenShell pins N and N-1, tested against both
10. durable: fixed workflow version, step-list test, startup carry-over sweep, idempotence check (#331)
11. durable: executor ID per pod, heartbeats and leader-elected dead-executor recovery (#331)
12. durable: `koinon_durable_workflows_pending{version}` metric and alert (#331)
13. security: key-wrapping interface, the local Secret adapter and its key export, fingerprints in the database and an unreachable-key metric (#332)
14. security: Vault/OpenBao transit adapter
15. security: AWS KMS adapter
16. keys: `keys forget` for a lost key (credentials marked lost, new kopia repositories, audited)
17. keys: new kopia repository per agent after a compromise, with S3 credential rotation (#332)
18. docs: secret-keys runbook covers home repositories, trace credentials, KMS, loss and compromise (#332)
19. docs: manual backup and restore procedure (`pg_dump` plus key export) for the cnpg and external modes
20. checkpoints: `checkpoints resync` after a database restore
21. e2e: automated backup-and-restore step in the nightly kind run
22. home lab: run the v0.1.0 restore drill and record RTO and RPO

## Links

- Issues #336, #331, #332; procoduck's scope decision of 2026-10-31 (in #336)
- Research store: deep dive G9, `g9-day2-operations`
- DBOS Transact Go v1.7.0 (the module in `go.mod`): `dbos/dbos.go`, `dbos/recovery.go`,
  `dbos/workflow.go` (`ForkWorkflowsInput`)
- ADRs [0027](0027-repo-and-versioning.md), [0030](0030-run-state-machine.md), [0032](0032-agent-home-checkpoints.md), [0051](0051-session-home-generations.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0060](0060-migrations-as-a-job.md), [0066](0066-triggers-and-scheduling.md), [0076](0076-database-modes.md), [0077](0077-pinned-openshell-and-installer.md)
- Runbook: [docs/ops/secret-keys.md](../ops/secret-keys.md)
