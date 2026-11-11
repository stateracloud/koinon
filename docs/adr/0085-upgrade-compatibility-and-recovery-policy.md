# 0085. Upgrade, compatibility and recovery policy

- Status: Accepted (scope decided by procoduck, 2026-10-31; decisions of 2026-11-01; a v0.1.0 gate; amended 2026-11-06: the schema promise starts at v0.3, see [0099](0099-no-schema-or-upgrade-promise-before-v0-3.md); amended 2026-11-11: storage probe before a rotation, #285)
- Date: 2026-11-01
- Amended by: [0099](0099-no-schema-or-upgrade-promise-before-v0-3.md)
- Relates to: [0027](0027-repo-and-versioning.md) (forward-only migrations, upgrade test), [0030](0030-run-state-machine.md) (leases, epoch, takeover), [0032](0032-agent-home-checkpoints.md) (home checkpoints), [0056](0056-model-providers-pricing-and-budgets-per-org.md) (KEK and envelopes), [0060](0060-migrations-as-a-job.md) (migration Job), [0066](0066-triggers-and-scheduling.md) (DBOS), [0076](0076-database-modes.md) (database modes, backups), [0077](0077-pinned-openshell-and-installer.md) (OpenShell pins), [0045](0045-openshell-hardening-defaults.md) (pinned supervisor image)
- Issue: old issue 336; covers old issue 331 and #64

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

### 4. DBOS workflow version and executor ID (old issue 331)

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

### 5. KEK custody and loss (#64)

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
the draft of this ADR and in old issue 336). Where he chose against the recommendation, the Alternatives
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
7. **The promise starts at v0.1.0** (procoduck, 2026-11-01). _Amended 2026-11-06: the promise starts
   at v0.3.0, see [0099](0099-no-schema-or-upgrade-promise-before-v0-3.md)._ The v0.1.0 schema is
   the first baseline.
   An rc install upgrades to v0.1.0 with a documented stop and cannot roll back to an rc. There is
   no retrofit for 00020 or older installs.
8. **Binaries gate on the schema version** (procoduck, 2026-11-01). Each binary declares the range of
   goose versions it supports and stays not Ready, with a clear log line, outside it. The migration
   Job's own check (refusing to migrate past what running pods support) can follow.
   Implemented (old issue 408): the schema declares who can read it. Migration 00046 adds the one-row table
   `schema_compat(min_reader_version)`, and `internal/schemagate` re-reads it and `goose_db_version`
   every 15 s. A binary is Ready iff the goose version is at least `migrations.MinSupported` (the
   oldest schema its code works against) and `min_reader_version` is at most the newest migration
   it embeds (`migrations.Latest()`). koinon-api and koinon-agentd (new `--health-addr`, default
   `:8082`) and koinon-reconciler answer `/readyz` 503 with the reason otherwise, and log it once
   per change; `/healthz` stays 200, so the pod waits and is not restarted. There is deliberately
   no upper bound compiled into a binary: an old release cannot predict a later schema, so the
   newer schema carries the verdict. An expand migration leaves `min_reader_version` alone and
   every older release in the minor line stays Ready (decision 5: patch releases roll, rollback is
   an image rollback); a breaking migration, allowed only in a new minor line (decision 6a), sets
   it to its own version with `UPDATE schema_compat SET min_reader_version = <its version>`.
   `TestCompatDeclarations` rejects any other write to the column; `TestSupportedRange` keeps
   `MinSupported` satisfiable.

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
   *Amendment (old issue 410).* The image is kept out of `specHash`, so an unreadable ConfigMap never hides a
   flavor, resource, provider or filesystem change and existing hashes stay valid. The sandbox
   carries a separate label, `<domain>/supervisor-image`, holding the first 16 hex characters of
   the sha256 of the image ref (the ref has `/` and `:` and can pass 63 characters, so it is not a
   valid label value); the reconciler compares digests. A recreate takes a slot from
   `maxRecreatesPerPass` (default 4) before the home gate, so at most that many checkpoints and wakes
   start per pass. While the ConfigMap cannot be read, the last image read is kept; before the first
   successful read the image is unknown and only supervisor-driven recreates wait, while creating,
   starting, stopping and other spec changes go on. Once an image is known, a sandbox without the
   label (created before it, or while the image was unreadable) counts as changed and is recreated
   once, so an existing fleet converges; with no `supervisor_image` configured nothing is recreated.
10. **OpenShell N and N-1** (procoduck, 2026-11-01). Each Koinon release supports its pinned OpenShell
    (ADR 0077) and the previous pin, tested in CI against both. Either can be upgraded first.

### DBOS (old issue 331)

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

### KEK and recovery (#64)

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

- Resolved (old issue 412, decision 11): all four registered workflows are idempotent from step 0. Each is a
  single step whose effect carries its own key, so a carried-over run finds the first run's record:
  - `koinon.trigger-fire` (`create-task`): `CreateTriggeredTask` under dedupe source `trigger`, key
    `TriggerFireKey(trigger, due)`.
  - `koinon.trigger-run-now` (`run-trigger-now`): the manual-trigger source and the second fixed in
    the input (`Now`), so a replay is the same key.
  - `koinon.direct-message` (`deliver-direct-message`): dedupe source `direct_message`, key the
    client's message ID.
  - `koinon.notify` (`deliver:<kind>`): the wake handler is a state update plus a reconciler notify;
    the question handlers insert notifications under a per-user dedupe key and re-check the
    question's state first.
  The rule for a new workflow is on `durable.WorkflowVersion`. `durable.CarryOver` runs at launch
  and every minute (an older pod that still runs during a rolling upgrade can enqueue after the
  first sweep); the fork ID is `<original>~v<version>`, so concurrent sweeps make one fork. The
  step lists are recorded in `internal/durable/testdata/workflow_steps.json`, and
  `TestWorkflowSteps` fails on a change without a bump. Done under decision 11 (old issue 413): the executor
  ID is the pod name (`-executor-id`, `KOINON_EXECUTOR_ID` from the downward API when
  `api.durable.enabled`); each executor heartbeats into `dbos.koinon_executors` (created by the
  migrate step); the holder of a Postgres advisory lock recovers the pending workflows of executors
  of its own version whose heartbeat is older than 90 s (`durable.Runtime.RecoverDeadExecutors`),
  forgets stale executors of older versions (the carry-over forks their workflows) and leaves those
  of newer versions to a process of that version. Still to do: the pending-by-version metric.
- Resolved (old issue 415, decision 12; first slice 415a). A KMS is an option, not a mandate (procoduck,
  2026-11-02): the local keyring, with the KEKs in a Kubernetes Secret, stays the default and is
  fully supported, with no migration warning and nothing deprecated. A KMS (OpenBao or AWS) is opt-in
  through configuration. What is decided:
  - Interface: `secretstore.Wrapper` (`Backend`, `KeyID`, `Wrap`, `Unwrap`, `Probe`), with `Local` as
    the default adapter. Every adapter call ends by its context's deadline and the adapter applies
    its own per-call timeout; `secretstore.Wrappers` also bounds each call (10 s), because the
    reload loops pass a context that lives as long as the process. One constructor
    (`secretstore/wrapping.New`) builds a process's wrappers from its configuration.
  - The KMS wraps the KEKs, not each DEK (procoduck accepted, 2026-11-02). The KEK Secret holds each KEK
    as its wrapper stored it (a local KEK as the base64 of its bytes, unchanged; a KMS-wrapped one as
    JSON naming the backend and key). Processes unwrap the key set when they load or reload it, so
    `Seal` and `Open` stay local and synchronous and a short KMS outage does not stop reads. A
    wrapped KEK is bound to its ID. `Wrappers` reads each stored KEK with its own adapter (`Local` is
    always readable), so an install that opts in moves by add (wrapped by the KMS), `activate`,
    `rewrap`, `retire`.
  - Remote adapters share `secretstore/remotekms` (old issue 416): the per-call bound, the error rules, and
    a frame (version, SHA-256 of the AAD, KEK) that the KMS encrypts, so the binding of a KEK to its
    ID does not depend on the KMS honouring associated data. OpenBao's key-admin policy grants
    `update` on `transit/encrypt/<key>` and `transit/decrypt/<key>`, never `create`: with `create`,
    encrypt makes a missing key on the spot, so a mistyped key name would wrap under a new key.
  - Identity: adapters sign in with the pod's identity (OpenBao Kubernetes auth, the AWS default
    chain with IRSA; procoduck, 2026-11-02), so no credential is in Helm values, env or a sandbox. The
    AWS adapter (old issue 417, `secretstore/awskms`) narrows the default chain to pod identity: IRSA's web
    identity token or EKS Pod Identity, read again on each refresh. It never reads static keys
    (`AWS_ACCESS_KEY_ID` and friends are ignored with a warning), the shared config files or the
    instance metadata, so a node's role cannot stand in for the pod's.
  - Only once a KMS primary is configured: koinon-api (with its `keys` commands) and the reconciler
    are decrypt-only (agentd holds no keyring), and a separate key-admin Job with its own service account is the only
    identity with encrypt rights. It wraps new KEKs: on first install and on demand for `keys add`
    and rotation (procoduck, 2026-11-02). The services refuse to wrap with a KMS
    (`secretstore.ErrKeyAdminWraps`, "new KEKs are wrapped by the key-admin Job") before they call
    it. A local install has no key-admin Job: koinon-api creates and adds KEKs exactly as before.
    415a has the refusal; the Job, its service account and its chart wiring are part of old issue 416, with
    the first adapter (OpenBao transit, `secretstore/openbao`).
  - The key-admin Job (old issue 416) is `koinon-api key-admin`, rendered only with `secrets.kek.kms`. It is
    an ordinary release resource, not a hook: a post-install hook would deadlock `helm install
    --wait` (koinon-api is not ready before the KEK Secret exists) and in cnpg mode a pre-install
    hook has no database. Its name carries a hash of its pod template, so any change of its inputs
    (`keyAdmin.addKey`, the KMS settings, the image) runs it once more and an unchanged upgrade or
    Argo CD sync leaves the finished Job alone; no immutable Job spec is ever changed. It creates
    the KEK Secret when missing (never with `existingSecret`, and never while the database holds
    values) and adds the KEK `addKey` names; it never activates, rewraps or retires. With a KMS,
    koinon-api runs with `--kek-create=false` and restarts until the Job has made the Secret.
  - Lost KMS key that wrapped the active KEK (#255): the one exception to "it never activates". With
    `--activate-if-no-active` (chart `keyAdmin.activateIfNoActive`, with `addKey`) the Job adds the
    KEK and makes it active in the same write, only when the Secret's `active` entry is missing or
    names a KEK the Secret no longer holds. It never replaces a working active KEK. The key set must
    not load without an active KEK (every process still refuses it), and the new KEK has to be
    wrapped by the identity that may encrypt, so the Job does it. The operator removes the dead
    KEKs' entries first; `keys forget` then runs as for any lost KEK.
  - Fingerprint: decision 12 said the wrapping keys' fingerprints; the code fingerprints the KEKs
    (`secretstore.Fingerprint`, HMAC-SHA256 keyed by the KEK, 16 bytes). A KEK's fingerprint is the
    same however it is wrapped: a KEK's fingerprint does not depend on its wrapping, so an export
    matches the KEKs it holds whichever backend wraps them; a wrapping key's identity is already in the stored form (backend and key). `keys status`
    and `keys export` print it.
  - Built (415b, old issue 591): koinon-api records each KEK's fingerprint in `kek_fingerprints` on load, on
    reload and on `keys activate`; `keys status` warns when a loaded KEK differs from the recorded
    one, says whether the active KEK's wrapping key answers, and warns about a local active KEK only
    when a KMS is configured; `keys activate` refuses a local KEK while a KMS is the configured primary (procoduck, 2026-11-03; rollback: set the primary to local first). koinon-api and the reconciler probe the active KEK's wrapper on every
    reload and export `koinon_kek_active_reachable{backend}`; the reconciler records nothing. Rows of
    retired KEKs are kept: IDs are never reused and `keys forget` needs the history. Binding an
    install ID into the KEK AAD is old issue 615 (split off old issue 416, procoduck 2026-11-03). If the KMS is down at start the pod exits (fail
    closed) and the gauge never shows 0.
    agentd holds no keyring (it asks koinon-api for repository passwords), so it needs no adapter.
  - N-1 and rollback: once a KMS-wrapped KEK is in the Secret, a binary from before 415a cannot parse
    the key set (it wants base64): koinon-api will not start and the reconciler keeps waiting for
    keys. Before rolling back past 415a, an install that opted in sets the wrapping back to local (the
    KMS still configured for reading), adds and activates a local KEK, rewraps and
    retires every KMS-wrapped KEK, so the Secret holds local KEKs only. 415a itself cannot configure
    a KMS, so no install reaches that state before an adapter ships; a local install's Secret is
    byte-for-byte what N-1 reads.
  - Amended (old issue 615, 2026-11-03): the install ID is bound into the KEK AAD. The install ID is a
    random UUID the first migration writes into `install_identity` (one row; triggers refuse
    update, delete and truncate as a guard against mistakes; the table owner, `pg_restore
    --disable-triggers` and `session_replication_role = replica` bypass them, which can only close
    bound KEKs, never open one). A database clone is the same install, so a staging copy that must
    not open production KEKs needs its own KMS key; a data-only restore onto a migrated database
    stops on the singleton key. `koinon_kek_unbound` (no labels) counts the KMS KEKs not bound yet,
    set on every load, with a warning while above 0. The database is the source because it is born with the data and
    travels with it: an upgrade, a database restore, a KEK Secret restore, a reinstall that keeps
    the database and a namespace or cluster move all keep it, restore order does not matter, and an
    empty database is a new install whose left-over KEK Secret does not open. A chart value (lost on
    a reinstall, not born with the data), a generated Secret (restored with the KEK Secret, which
    defeats the binding) and a namespace or cluster UID (changes on a move) were rejected. The
    version lives in the stored KEK, not in the KMS frame (which is only readable after the KMS
    call, while the AAD must be chosen before it): `"fmt":1` keeps the old issue 416 AAD and opens as
    before; `"fmt":2` records the install ID and uses a separate AAD with the process's own install
    ID, never the recorded one (that only names both IDs in the error). Relabelling a KEK between
    the formats, or opening it in another install, is `ErrOpen`, never `ErrUnreachable`; a process
    without the install ID refuses a bound KEK with `ErrNoInstallID`. Every new KMS wrap is bound
    (the key-admin Job now always reads the database and waits for the first migration). Binding
    the KEKs wrapped before is opt-in, in the key-admin Job only (`keyAdmin.rewrapUnbound`,
    idempotent, primary wrapping key only), because N-1: a binary before old issue 615 refuses `"fmt":2`
    ("unsupported format 2") and fails closed, so rolling back below old issue 615 needs the 415a steps
    above. Local KEKs are not wrapped and stay unbound. `keys status` prints the install ID and
    marks each KMS KEK bound or unbound.
  - Amended (old issue 615, 2026-11-03, procoduck): a strict switch after binding. Re-binding keeps the KEK
    material, so an old `"fmt":1` backup of the KEK Secret would open forever. A "bound only" marker,
    one row in its own table `kek_bound_only` (expand-only migration, triggers refuse UPDATE, DELETE
    and TRUNCATE; they guard against mistakes, not against the table owner), is set at first install
    (before the first KEK Secret is created) and by the key-admin Job's `rewrapUnbound` once no KMS
    KEK is unbound, never while one is. Once set, every process refuses a KMS `"fmt":1` KEK
    (`ErrBoundOnly`, an `ErrOpen`), naming when and how the marker was set; an unreadable marker
    refuses too. Koinon only sets it. A process reads it on every key-set load while it is unset
    (start and the reload about once a minute) and caches it once set, so one that loaded before the
    switch refuses from its next load. Setting it ends rollback below old issue 615 for that key set, and the
    Job says so. The marker protects only databases that hold it; rotating the KEKs is what kills an
    old backup everywhere, so the docs tell operators to rotate as well if one may have leaked.
    `keys status` prints the marker; `koinon_kek_unbound` stays.
- Built (#88, #90): the manual procedure is `docs/site/src/content/docs/guides/backup-restore.md` (cnpg and
  external modes: `keys export`, `pg_dump --format=custom`, stop, restore, keys, start, `checkpoints resync`).
  The nightly kind e2e runs its blocks through `test/e2e/backup-restore.sh` (`BACKUP_RESTORE=1`), changes the
  install after the backup, restores, and requires the install ID, task and run counts and the keys to equal
  the backed-up ones. Not covered by it: a provider key decrypting in a run, a schedule fire, a recovered
  DBOS workflow, a restored agent home (the home-lab drill, decision 15).
- Resolved (detection of the decision 6 rule): the lint reads a marker file, `hack/migrate-patch-line`
  (`none` or the shipped minor line, for example `v0.1`), at the merge base. Patch tags are cut from
  `main` (the release gate requires an ancestor of `origin/main`), so the marker, not a branch name,
  makes the rule bite: it is set when a minor line's `.0` ships and reset to `none` in the change that
  opens the next minor. A base ref named `release/*` and `MIGRATE_LINT_PATCH_LINE=1` also enable it.
  _Amended 2026-11-06 ([0099](0099-no-schema-or-upgrade-promise-before-v0-3.md)): a `release/*` base
  no longer enables it; only the marker and `MIGRATE_LINT_PATCH_LINE=1` do._

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

## Amendment (2026-11-06): the schema promise starts at v0.3

Maintainer decision, recorded in [0099](0099-no-schema-or-upgrade-promise-before-v0-3.md): no schema
or upgrade promise before v0.3. Decisions 5 to 8 (the minor-line promise, the patch-line lint and
the N-1 patch test, the baseline, the schema-version gate as a promise) bind from v0.3.0, and the
v0.3.0 schema is the baseline of decision 7. Until then migrations may rename and drop, and an
upgrade between pre-v0.3 releases may need a reinstall or documented manual steps. The gate of
decision 1 on v0.1.0 no longer includes the schema decisions; it still covers backups, the restore
runbook and the restore drill (decisions 13 to 16), and the drain and rollout decisions 2 to 4.
The marker `hack/migrate-patch-line` stays `none` until v0.3.0 ships, and a `release/*` base is not
a patch line by its name.

## Amendment (2026-11-11): keys commands ask agentd to probe the storage before they pick an epoch (#285)

Maintainer decision of 2026-11-07, built for v0.2. `keys forget` and `keys new-repositories` start the
agent's next repository epoch at `max(current + 1, home_repositories.next_epoch_at_least)`. The floor
(#262, `next_epoch_at_least`) is recorded only after agentd has met a taken prefix, so a rotation
before any resync or backup could still land on a prefix that holds a repository the database has no
password for, and the next backup then fails with `ErrPrefixTaken`. The keys command holds the KEKs
and never the bucket's credentials; agentd holds the credentials and no KEK (ADR 0032). No process
may hold both, so the check goes through the database:

- The command writes a probe request (`home_repository_probes`, keyed `(org_id, id)`: agent and
  candidate epoch) and waits, by default two minutes plus 250 ms per agent (`Tool.ProbeTimeout`,
  `--probe-timeout`). agentd claims pending probes with a lease and answers up to 16 at once, as
  each is an independent listing, so the wait does not grow with the number of agents (the checkpoint loop and the restore-hold loop, `probeTick`), lists the agent's
  epoch folders with the listing a resync uses (`HomeStore.HeldEpochs`: any object under `r<N>/` is a
  taken prefix, whatever its password) and answers in the row: `free`, `taken` or `error`, with a
  message that carries no endpoint text and no secret.
- **Fail closed.** `taken`, `error` and no answer in time each refuse the rotation before anything is
  written: no epoch, no seed request, no `lost` mark, no audit event. There is no override flag; the
  operator fixes agentd or the bucket and runs the command again, or lets agentd record the floor
  (a resync or a backup into the prefix) and runs it again.
- The epoch probed must be the epoch the transaction starts. The command reads the candidate
  (`LatestHomeRepositoryNextEpoch`) before it probes and compares it with the epoch
  `Passwords.NewEpochIn` picked under the row lock; a difference (another rotation, or agentd
  raising the floor meanwhile) rolls the transaction back and asks for a re-run.
- The floor of #262 stays as the second guard: the candidate already includes it, and agentd still
  records it when a backup meets a taken prefix.
- `keys new-repositories` probes every agent before it starts any epoch, so one refused agent refuses
  the command. `keys forget` collects the agents that need an epoch inside its org transaction and
  probes them together on other connections of the pool (hence `forgetMinConns` is 3), so a refusal
  rolls back the lost marks of that org too.
- The probe table is ephemeral: the command deletes the rows it read, agentd deletes rows an hour
  after they expire, and agentd never claims an expired probe.

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
10. durable: fixed workflow version, step-list test, startup carry-over sweep, idempotence check (old issue 331)
11. durable: executor ID per pod, heartbeats and leader-elected dead-executor recovery (old issue 331)
12. durable: `koinon_durable_workflows_pending{version}` metric and alert (old issue 331)
13. security: key-wrapping interface, the local Secret adapter and its key export, fingerprints in the database and an unreachable-key metric (#64)
14. security: Vault/OpenBao transit adapter
15. security: AWS KMS adapter (old issue 417)
16. keys: `keys forget` for a lost key (credentials marked lost, new kopia repositories, audited)
17. keys: new kopia repository per agent after a compromise, with S3 credential rotation (#64)
18. docs: secret-keys runbook covers home repositories, trace credentials, KMS, loss and compromise (#64)
19. docs: manual backup and restore procedure (`pg_dump` plus key export) for the cnpg and external modes
20. checkpoints: `checkpoints resync` after a database restore (#89)
21. e2e: automated backup-and-restore step in the nightly kind run
22. home lab: run the v0.1.0 restore drill and record RTO and RPO

## Links

- Old issues 336, 331, #64; procoduck's scope decision of 2026-10-31 (in old issue 336)
- Research store: deep dive G9, `g9-day2-operations`
- DBOS Transact Go v1.7.0 (the module in `go.mod`): `dbos/dbos.go`, `dbos/recovery.go`,
  `dbos/workflow.go` (`ForkWorkflowsInput`)
- ADRs [0027](0027-repo-and-versioning.md), [0030](0030-run-state-machine.md), [0032](0032-agent-home-checkpoints.md), [0051](0051-session-home-generations.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0060](0060-migrations-as-a-job.md), [0066](0066-triggers-and-scheduling.md), [0076](0076-database-modes.md), [0077](0077-pinned-openshell-and-installer.md)
- Runbook: [docs/ops/secret-keys.md](../ops/secret-keys.md)
