---
title: Upgrading Koinon
description: No schema or upgrade promise before v0.3. From v0.3, patch upgrades roll and minor upgrades are stop, migrate, start. How to check, and how to go back.
---

How you upgrade depends on the version you move between.
[ADR 0085](https://github.com/stateracloud/koinon/blob/main/docs/adr/0085-upgrade-compatibility-and-recovery-policy.md)
has the decisions behind this page.

Before v0.3 there is no schema or upgrade promise
([ADR 0099](https://github.com/stateracloud/koinon/blob/main/docs/adr/0099-no-schema-or-upgrade-promise-before-v0-3.md)).
An upgrade between any two releases before v0.3.0, patch upgrades and release candidates included,
may need a reinstall or the manual steps in the release notes. Read them and take the database
backup first. Backups and restoring from them are supported in every release, and are the way back
when an upgrade goes wrong.

From v0.3.0 the following applies:

| Upgrade | Example | How | Rollback |
| --- | --- | --- | --- |
| Patch, same minor line | v0.3.0 to v0.3.3 | Rolling: one `helm upgrade` | Roll the images back |
| Minor | v0.3.x to v0.4.0 | Stop, migrate, start | Restore the database from your backup |
| Before v0.3.0 to v0.3.0 | v0.2.x to v0.3.0 | Stop, migrate, start, or reinstall (see below) | None (see below) |

Koinon promises schema compatibility within a minor line, starting at v0.3.0. Migrations only move
forward: nothing runs a `Down`. Inside a minor line they only add (new columns and tables, no rename,
no drop, no type change), so the previous patch release keeps working on the new schema. A new
minor line may remove and rename things, so its schema can break the previous minor.

Commands use the namespace `koinon`. Two names depend on your release:

- `<release>` is the Helm release name (`koinon` in the [Quickstart](/quickstart/)).
- `<name>` is the chart's full name, which prefixes every object. It is `<release>-koinon`, except
  that when the release name already contains `koinon` it is just `<release>`. So release `koinon`
  gives `koinon-api`, `koinon-migrate` and `koinon-kek`, and release `prod` gives `prod-koinon-api`,
  `prod-koinon-migrate` and `prod-koinon-kek`. (`fullnameOverride` replaces it.)

The Deployments are `<name>-api`, `<name>-reconciler` and `<name>-agentd`. The Secret with the
encryption keys is `<name>-kek`, or the Secret you set in `secrets.kek.existingSecret`. Replace
`<version>` with the chart version you move to, without the leading `v` (for example `0.3.3`).

## Before you start

1. Read the release notes of the version you move to, under "Upgrading from vX.Y". They name changed
   values, a new OpenShell pin and anything you must do first.
2. Your values file must still render with the new chart:

   ```sh
   helm template <release> oci://ghcr.io/stateracloud/koinon/charts/koinon --version <version> \
     --namespace koinon -f values.yaml > /dev/null
   ```

   If you installed from a clone, use `helm template <release> charts/koinon` after you check out the tag.
3. Back up the encryption keys (the [Quickstart](/quickstart/#back-up-the-encryption-keys) explains
   why they matter):

   ```sh
   kubectl -n koinon get secret <name>-kek -o yaml > koinon-kek-backup.yaml
   ```

4. For a minor upgrade, back up the database too (the details, and how to check the dump, are on
   [Back up and restore](/guides/backup-restore/)), because it is the only way back. Keep the dump with
   the key backup from the step above, and note the chart version you ran (`helm -n koinon list`).
   Without both, a minor upgrade cannot be undone.

   - **cnpg mode** (the default). The rendered CloudNativePG Cluster `<name>-pg` has no backup
     configured, so dump the database from its primary pod. The database is `postgres.cnpg.database`
     (default `koinon`); the dump covers every schema in it, including `dbos`:

     ```sh
     PRIMARY=$(kubectl -n koinon get pod -l cnpg.io/cluster=<name>-pg,cnpg.io/instanceRole=primary -o name)
     kubectl -n koinon exec "$PRIMARY" -c postgres -- pg_dump --format=custom --dbname=koinon > koinon-pre-upgrade.dump
     ```

   - **external mode** (your own Postgres, for example RDS). Take your provider's snapshot, or run
     `pg_dump --format=custom` as the migration role against the same database.

5. Note the state you start from, to compare afterwards:

   ```sh
   helm -n koinon list
   kubectl -n koinon get deploy <name>-api <name>-reconciler <name>-agentd
   kubectl -n koinon get pods
   ```

## Upgrading to the install audit chain (ADR 0100)

The release that adds the install audit chain (migration 00066) needs a `NOLOGIN` group role,
`koinon_install_audit_writer`, and `koinon-api`'s login role must be a member of it. The migrate role
has no `CREATEROLE`, so it cannot create the role itself. The migration does not need the role: the
`INSERT` grant to it is made by `koinon-api migrate` after the migrations, on every run, and only when
the role exists. In cnpg mode a missing role makes the Job log a warning, not fail; on an external
database the chart passes `--require-install-writer` and the Job fails before any migration, naming
the role to create. The membership is checked by `koinon-api` itself, which alone knows the login it
connects as: at startup and every 30 seconds it logs an error with the `GRANT` to run for its own
login and reports `koinon_install_audit_writer_ok 0`, and it keeps serving.

- **`postgres.mode: cnpg`.** Nothing to do. The chart lists the role as a CloudNativePG managed role,
  and `koinon_api` joins it. With Helm or Flux the migrate Job is a `pre-upgrade` hook, so it runs
  before the updated Cluster reaches the API server and finds no role yet. A second Job,
  `<release>-migrate-grants`, is a `post-upgrade` hook (Argo CD: `PostSync`): it runs
  `koinon-api migrate --grants-only`, waits (up to `migrations.wait`) until the operator has created
  the role, and grants it. Until then `koinon-api` serves, logs an error for each install audit event
  it cannot write yet and retries it, and platform price changes fail with a permission error; the
  scheduled price refresh keeps the recorded setting until its change is recorded. If the operator
  does not create the role in time, the hook fails and so does the upgrade: check
  `kubectl get cluster` and the operator's log, then run the same `helm upgrade` again. Helm's
  `--timeout` (default 5m) covers each hook, so it must be longer than `migrations.wait` (default
  4m): otherwise Helm gives up on the hook before the hook gives up on the role, and reports a
  timeout instead of the hook's message.
- **`postgres.mode: external`.** Before you upgrade, as an administrator:

  ```sql
  CREATE ROLE koinon_install_audit_writer NOLOGIN;
  GRANT koinon_install_audit_writer TO <api login role>;
  ```

  If the migration role is not the one that owns the tables, or the role `koinon_app` does not exist,
  also run this after the upgrade's migration, so that only `koinon-api` can write the chain:

  ```sql
  REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON install_audit_events, install_audit_chain_head FROM <reconciler login role>, <agentd login role>;
  GRANT SELECT ON install_audit_events, install_audit_chain_head TO <reconciler login role>, <agentd login role>;
  ```

  Without the role the migrate Job fails (`the role koinon_install_audit_writer does not exist`)
  and changes nothing: create it as above and run the same `helm upgrade` again. Without the
  membership `koinon-api` starts, logs `GRANT koinon_install_audit_writer TO "<its login>"`, and
  picks the grant up within 30 seconds.

  Keep the `api`, `reconciler` and `agentd` logins separate. See "The install audit chain" in
  `docs/ops/secret-keys.md` in the repository.

## Patch upgrade (rolling)

A patch release shares the schema with the one before it. Upgrade in place:

```sh
helm upgrade <release> oci://ghcr.io/stateracloud/koinon/charts/koinon --version <version> \
  --namespace koinon -f values.yaml --wait --timeout 10m
```

Helm runs the preflight, the secrets Job and the migration Job (`<name>-migrate`) first. Then the
Deployments roll. During the roll the old pods run against the new schema, which is why a patch
migration only adds.

agentd starts the new worker before it stops the old one (`maxSurge: 1`, `maxUnavailable: 0`). The
old worker stops claiming runs, finishes the turn in progress for up to `agentd.drainDeadlineSeconds`
(default 300), and then hands its runs to another worker. Its pod gets that time plus 30 seconds
before Kubernetes kills it, so a patch upgrade can take several minutes per agentd pod while agents are
busy. A run handed off by a stopping worker is picked up by another and continues from its session.

Under Argo CD, change the chart version in the Application and sync. The migration Job runs in sync
wave 1 and the Deployments follow in wave 2 (see [GitOps](/quickstart/#gitops-argo-cd-flux)).

## Minor upgrade (stop, migrate, start)

A minor release can change the schema in a way the previous release cannot read. So the old pods must
not be running while the new schema is applied, and Koinon is down for the length of the steps below.
Plan a maintenance window. Sandboxes keep running while Koinon is stopped, but no run makes progress
and no schedule fires.

The order matters because Helm runs the migration Job before it applies the new manifests. If you
upgrade with the old pods still up, they run against the new schema until the roll reaches them.
So stop first.

1. **Stop.** Scale agentd down first, so it drains, then the reconciler and the API:

   ```sh
   kubectl -n koinon scale deployment <name>-agentd --replicas=0
   kubectl -n koinon wait --for=delete pod -l app.kubernetes.io/instance=<release>,app.kubernetes.io/component=agentd --timeout=10m
   kubectl -n koinon scale deployment <name>-reconciler <name>-api --replicas=0
   kubectl -n koinon wait --for=delete pod -l 'app.kubernetes.io/instance=<release>,app.kubernetes.io/component in (api,reconciler)' --timeout=5m
   ```

   If `wait` prints "no matching resources found", the pods were already gone.

   agentd drains for up to `agentd.drainDeadlineSeconds` (default 300) before its pods go. The
   database (CloudNativePG or yours), the gateway and OpenShell stay up.

2. **Migrate.** Upgrade the release with every component at zero replicas. Save this as `stop.yaml`:

   ```yaml chart-values
   # Keeps the three Koinon Deployments at zero while the migration runs.
   api:
     replicas: 0
   reconciler:
     replicas: 0
   agentd:
     replicas: 0
   ```

   ```sh
   helm upgrade <release> oci://ghcr.io/stateracloud/koinon/charts/koinon --version <version> \
     --namespace koinon -f values.yaml -f stop.yaml --wait --timeout 15m
   ```

   Helm runs the preflight, then the migration Job `<name>-migrate` (which runs `koinon-api migrate`),
   then applies the new chart with no pods. The Job is deleted when it succeeds, so a successful
   `helm upgrade` is your proof. If the upgrade fails, the Job is kept:

   ```sh
   kubectl -n koinon logs job/<name>-migrate
   kubectl -n koinon describe job <name>-migrate
   ```

   The Job retries `migrations.backoffLimit` times (default 4) and stops after
   `migrations.activeDeadlineSeconds` (default 900). Fix the cause, then run the same `helm upgrade`
   again: the migration is safe to repeat. Do not start the old version on a half-migrated schema; see
   [Roll back](#roll-back).

3. **Start.** Upgrade again without `stop.yaml`, so the replicas return to your values:

   ```sh
   helm upgrade <release> oci://ghcr.io/stateracloud/koinon/charts/koinon --version <version> \
     --namespace koinon -f values.yaml --wait --timeout 10m
   ```

   The migration Job runs once more and has nothing to do.

Under Argo CD or Flux, do the same through Git: stop the components first as in step 1 (turn off
automated sync or self-heal for the window, or they scale back up), commit the new chart version
together with the values of `stop.yaml` and sync, wait for the migration, then commit your normal
replica counts and sync again.

### The v0.3.0 baseline and earlier releases

v0.3.0 is the first release with the compatibility promise, and its schema is the first baseline.
Releases before it (v0.1.x, v0.2.x and their release candidates) have no promise: their migrations
may rename, drop or retype. The release notes of v0.3.0 say how an earlier install reaches it:
either the stop, migrate, start above, or a reinstall. It cannot go back to an earlier release,
because migrations are forward-only and some of them renamed tables in place (for example
migration 00020). Take the database backup first. An install older than rc.3 is not supported by
any upgrade path: Koinon ships no compatibility views for the tables that migration 00020 renamed,
so re-install instead of upgrading.

## Draining a node that runs the database

This applies to `postgres.mode: cnpg` (the default). Node upgrades (Talos, kubeadm, a cloud node
pool) drain each node in turn. CloudNativePG protects the Cluster with a PodDisruptionBudget, and for
a single-instance Cluster (`postgres.cnpg.instances: 1`, the chart default) that budget allows no
eviction. The drain of the node that runs `<name>-pg-1` then waits forever, and the node upgrade
stalls. `external` mode is not affected by the chart; check your own database's disruption policy.

Pick one:

- **Run two or more instances for production.** Set `postgres.cnpg.instances: 2` (or 3). The budget then
  allows one eviction at a time and CloudNativePG fails over to a replica. This is the recommended
  setup: no maintenance switch, and about a failover's worth of interruption per drain. It costs a
  second copy of the storage and the compute.
- **Use a maintenance window on a single instance.** Set it before the drain and remove it after.
  Pin the chart version you have installed, so that the switch does not also upgrade Koinon (without
  `--version`, helm pulls the newest chart, and a minor upgrade needs the procedure above). Read the
  installed version with `helm -n koinon list` (the `CHART` column, for example `koinon-0.3.1`) or
  `helm -n koinon get metadata <release>` (the `VERSION` field):

  ```sh
  helm upgrade <release> oci://ghcr.io/stateracloud/koinon/charts/koinon --version <installed-version> \
    --reuse-values \
    --set postgres.cnpg.nodeMaintenanceWindow.inProgress=true \
    --set postgres.cnpg.nodeMaintenanceWindow.reusePVC=true
  ```

  The Cluster drops its budget, so the drain evicts the pod, and CloudNativePG recreates it on another
  node with the same PersistentVolumeClaim. Plan for about 1 to 2 minutes without a database per
  drain, and longer if the volume has to detach and re-attach. Keep `reusePVC: true` on a single
  instance: `false` deletes the only copy of the data. Only a storage class that can attach the volume
  on another node (network or replicated storage) can reuse it; node-local storage cannot.
  Afterwards, remove the window with the same pinned version so the budget returns:

  ```sh
  helm upgrade <release> oci://ghcr.io/stateracloud/koinon/charts/koinon --version <installed-version> \
    --reuse-values \
    --set postgres.cnpg.nodeMaintenanceWindow=null
  ```

  If you manage the values in Git, commit the window and its removal like any other change.

During the window the API, the reconciler and agentd cannot reach the database. Their readiness
probes fail and requests return errors; agentd cannot renew run leases, so a run in progress can be
taken over by another worker once the database returns. Nothing is lost that was committed, and the
components reconnect on their own. Do the drain when no run you care about is mid-turn, or scale
agentd down first as in the minor upgrade steps above.

## Check afterwards

```sh
kubectl -n koinon get pods
kubectl -n koinon rollout status deployment/<name>-api
kubectl -n koinon rollout status deployment/<name>-reconciler
kubectl -n koinon rollout status deployment/<name>-agentd
kubectl -n koinon logs deployment/<name>-api --tail=50
```

- All three Deployments are Ready at the replica counts in your values. Each binary reports ready only
  while the database schema is one it supports. A pod that stays not Ready after the migration logs
  the reason once (a schema outside its supported range), and is not restarted. The usual cause is
  that the migration did not run, or that you started a release older than the schema.
- You can sign in, and agents you left active are healthy ([Agent health](/guides/agent-health/)).
- A test run on a throwaway agent reaches a result.
- If you use schedules, check that the next fire happens. Workflows that were pending at the upgrade
  are carried over to the new version at startup.
- With [metrics](/guides/metrics/) on, `koinon_agentd_takeovers_total` rises by one for each run another
  worker picked up after the stop.
- After an OpenShell upgrade (the release notes say whether the pin changed), the reconciler recreates
  each existing sandbox over time, so the fleet picks up the new supervisor. Each recreate takes a
  checkpoint first if [home checkpoints](/guides/home-checkpoints/) are on.

## Roll back

**Within a minor line from v0.3.0 (patch upgrades).** The old and new releases share the schema, so
going back is an image rollback. Use the previous revision or an explicit version:

```sh
helm -n koinon history <release>
helm -n koinon rollback <release> <revision> --wait --timeout 10m
```

or `helm upgrade` with `--version <previous>` and the same values. The migration does not run on a
rollback. A schema-version check keeps an old release from starting on a schema it cannot read, so a
mistake shows as pods that are not Ready, not as errors in queries.

**Across a minor, before v0.3.0 (patch upgrades included), or from a release candidate.** There is
no app-only rollback. A minor release, or any release before v0.3.0, may
contract the schema: it can rename tables and drop columns and types, and a migration cannot be
undone, because Koinon migrations are forward-only and a dropped column cannot return its data. The
previous release's pods would fail on their first query, and the schema-version check keeps them
Not Ready. The only way back is a restore:

1. Scale all three components to zero, as in step 1 above (or use `stop.yaml`).
2. Restore the database from the backup you took before the upgrade.
   - **cnpg mode.** Restore in place. The dump holds the old schema, so first drop the schemas the
     migration changed or added (`public` and `dbos`), then load the dump through the primary pod:

     ```sh
     PRIMARY=$(kubectl -n koinon get pod -l cnpg.io/cluster=<name>-pg,cnpg.io/instanceRole=primary -o name)
     kubectl -n koinon exec "$PRIMARY" -c postgres -- psql --dbname=koinon \
       -c 'DROP SCHEMA IF EXISTS dbos CASCADE' -c 'DROP SCHEMA public CASCADE' -c 'CREATE SCHEMA public AUTHORIZATION pg_database_owner' -c 'GRANT USAGE ON SCHEMA public TO PUBLIC'
     kubectl -n koinon exec -i "$PRIMARY" -c postgres -- pg_restore --dbname=koinon --exit-on-error < koinon-pre-upgrade.dump
     ```

     Pointing `postgres` at a database you restored somewhere else means switching the install to
     `postgres.mode: external` (a cnpg install cannot be pointed elsewhere), which is a change to how
     the chart connects, not a rollback step. Prefer the in-place restore.
   - **external mode.** Restore your provider's snapshot, or `pg_restore` the dump into the database.
3. Make sure the Secret `<name>-kek` (or `secrets.kek.existingSecret`) is the one from your key backup.
   Without the same key, the stored provider keys and every home checkpoint password cannot be read.
4. Install the previous chart version with the values you used then:

   ```sh
   helm upgrade <release> oci://ghcr.io/stateracloud/koinon/charts/koinon --version <previous-version> \
     --namespace koinon -f values.yaml --wait --timeout 10m
   ```

Everything written after the backup is lost: runs, tasks and ledger entries made since. Home
checkpoints in the bucket are not restored with the database, so the restored list of checkpoints is
older than the bucket. Once koinon-agentd is up, rebuild that list from the bucket with
`koinon-api checkpoints resync --all` (see
[After a database restore](/guides/home-checkpoints/#after-a-database-restore)). The whole procedure, with the checks that
prove it worked, is on [Back up and restore](/guides/backup-restore/); a nightly test restores an install with
it.

If the migration failed halfway on your database, restoring the backup is the safe way back too.
