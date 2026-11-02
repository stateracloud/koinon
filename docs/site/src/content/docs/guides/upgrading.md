---
title: Upgrading Koinon
description: Patch upgrades roll. Minor upgrades are stop, migrate, start. How to check, and how to go back.
---

How you upgrade depends on the version you move between.
[ADR 0085](https://github.com/stateracloud/koinon/blob/main/docs/adr/0085-upgrade-compatibility-and-recovery-policy.md)
has the decisions behind this page.

| Upgrade | Example | How | Rollback |
| --- | --- | --- | --- |
| Patch, same minor line | v0.1.0 to v0.1.3 | Rolling: one `helm upgrade` | Roll the images back |
| Minor | v0.1.x to v0.2.0 | Stop, migrate, start | Restore the database from your backup |
| A release candidate to v0.1.0 | v0.1.0-rc.9 to v0.1.0 | Stop, migrate, start | None to an rc (see below) |

Koinon promises schema compatibility within a minor line, starting at v0.1.0. Migrations only move
forward: nothing runs a `Down`. Inside a minor line they only add (new columns and tables, no rename,
no drop, no type change), so the previous patch release keeps working on the new schema. A new
minor line may remove and rename things, so its schema can break the previous minor.

The names below assume the release is called `koinon` in the namespace `koinon`, as in the
[Quickstart](/quickstart/). If your release has another name, the Deployments are
`<release>-api`, `<release>-reconciler` and `<release>-agentd` (the chart leaves out the repeated
name when the release name contains `koinon`). Replace `<version>` with the chart version you move
to, without the leading `v` (for example `0.1.3`).

## Before you start

1. Read the release notes of the version you move to, under "Upgrading from vX.Y". They name changed
   values, a new OpenShell pin and anything you must do first.
2. Your values file must still render with the new chart:

   ```sh
   helm template koinon oci://ghcr.io/stateracloud/charts/koinon --version <version> \
     --namespace koinon -f values.yaml > /dev/null
   ```

   If you installed from a clone, use `helm template koinon charts/koinon` after you check out the tag.
3. Back up the encryption keys (the [Quickstart](/quickstart/#back-up-the-encryption-keys) explains
   why they matter):

   ```sh
   kubectl -n koinon get secret koinon-kek -o yaml > koinon-kek-backup.yaml
   ```

4. For a minor upgrade, back up the database too, because it is the only way back. Use your database
   mode's tool: a CloudNativePG backup, your provider's snapshot, or `pg_dump --format=custom` on the
   database (include the `dbos` schema). Keep it with the key backup from the step above, and note
   the chart version you ran (`helm -n koinon list`). Without both, a minor upgrade cannot be undone.
5. Note the state you start from, to compare afterwards:

   ```sh
   helm -n koinon list
   kubectl -n koinon get deploy koinon-api koinon-reconciler koinon-agentd
   kubectl -n koinon get pods
   ```

## Patch upgrade (rolling)

A patch release shares the schema with the one before it. Upgrade in place:

```sh
helm upgrade koinon oci://ghcr.io/stateracloud/charts/koinon --version <version> \
  --namespace koinon -f values.yaml --wait --timeout 10m
```

Helm runs the preflight, the secrets Job and the migration Job (`koinon-migrate`) first. Then the
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
   kubectl -n koinon scale deployment koinon-agentd --replicas=0
   kubectl -n koinon wait --for=delete pod -l app.kubernetes.io/instance=koinon,app.kubernetes.io/component=agentd --timeout=10m
   kubectl -n koinon scale deployment koinon-reconciler koinon-api --replicas=0
   kubectl -n koinon wait --for=delete pod -l app.kubernetes.io/instance=koinon,app.kubernetes.io/component=reconciler --timeout=5m
   kubectl -n koinon wait --for=delete pod -l app.kubernetes.io/instance=koinon,app.kubernetes.io/component=api --timeout=5m
   ```

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
   helm upgrade koinon oci://ghcr.io/stateracloud/charts/koinon --version <version> \
     --namespace koinon -f values.yaml -f stop.yaml --wait --timeout 15m
   ```

   Helm runs the preflight, then the migration Job `koinon-migrate` (which runs `koinon-api migrate`),
   then applies the new chart with no pods. The Job is deleted when it succeeds, so a successful
   `helm upgrade` is your proof. If the upgrade fails, the Job is kept:

   ```sh
   kubectl -n koinon logs job/koinon-migrate
   kubectl -n koinon describe job koinon-migrate
   ```

   The Job retries `migrations.backoffLimit` times (default 4) and stops after
   `migrations.activeDeadlineSeconds` (default 900). Fix the cause, then run the same `helm upgrade`
   again: the migration is safe to repeat. Do not start the old version on a half-migrated schema; see
   [Roll back](#roll-back).

3. **Start.** Upgrade again without `stop.yaml`, so the replicas return to your values:

   ```sh
   helm upgrade koinon oci://ghcr.io/stateracloud/charts/koinon --version <version> \
     --namespace koinon -f values.yaml --wait --timeout 10m
   ```

   The migration Job runs once more and has nothing to do.

Under Argo CD or Flux, do the same through Git: stop the components first as in step 1 (turn off
automated sync or self-heal for the window, or they scale back up), commit the new chart version
together with the values of `stop.yaml` and sync, wait for the migration, then commit your normal
replica counts and sync again.

### The v0.1.0 baseline and release candidates

v0.1.0 is the first release with the compatibility promise, and its schema is the first baseline.
An install made from a release candidate upgrades to v0.1.0 with the stop, migrate, start above and
cannot go back to a release candidate: the rcs have no promise, and migration 00020 (rc.3) renamed
tables in place. Take the database backup first. There is no retrofit for installs that are older than
rc.3.

## Check afterwards

```sh
kubectl -n koinon get pods
kubectl -n koinon rollout status deployment/koinon-api deployment/koinon-reconciler deployment/koinon-agentd
kubectl -n koinon logs deployment/koinon-api --tail=50
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

**Within a minor line (patch upgrades).** The old and new releases share the schema, so going back is
an image rollback. Use the previous revision or an explicit version:

```sh
helm -n koinon history koinon
helm -n koinon rollback koinon <revision> --wait --timeout 10m
```

or `helm upgrade` with `--version <previous>` and the same values. The migration does not run on a
rollback. A schema-version check keeps an old release from starting on a schema it cannot read, so a
mistake shows as pods that are not Ready, not as errors in queries.

**Across a minor, or from a release candidate.** There is no app-only rollback. A minor release may
contract the schema: it can rename tables and drop columns and types, and a migration cannot be
undone, because Koinon migrations are forward-only and a dropped column cannot return its data. The
previous release's pods would fail on their first query, and the schema-version check keeps them
Not Ready. The only way back is a restore:

1. Scale all three components to zero, as in step 1 above (or use `stop.yaml`).
2. Restore the database from the backup you took before the upgrade, with its `dbos` schema, into the
   same database (or point `postgres` at the restored one).
3. Make sure `koinon-kek` is the one from your key backup. Without the same key, the stored provider
   keys and every home checkpoint password cannot be read.
4. Install the previous chart version with the values you used then:

   ```sh
   helm upgrade koinon oci://ghcr.io/stateracloud/charts/koinon --version <previous-version> \
     --namespace koinon -f values.yaml --wait --timeout 10m
   ```

Everything written after the backup is lost: runs, tasks and ledger entries made since. Home
checkpoints in the bucket are not restored with the database, so a restored install can list snapshots
that were newer than the backup. A full, tested restore procedure is not part of this page yet (it
is tracked in ADR 0085, implementation items 19 and 20).

If the migration failed halfway on your database, restoring the backup is the safe way back too.
