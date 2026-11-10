---
title: Back up and restore
description: Back up the database and the encryption keys by hand, restore them into a stopped install, and check the result.
---

A Koinon install has three parts that must agree after a restore:

| Part | Where it lives | What loses it |
| --- | --- | --- |
| The database | Postgres: runs, tasks, the ledger, the `dbos` schema (pending workflows and schedules), and every stored credential as ciphertext | A lost volume, a bad upgrade, a dropped table |
| The encryption keys | The Secret `koinon-kek` (or `secrets.kek.existingSecret`), or your KMS | A deleted namespace, a new cluster |
| The checkpoint bucket | Your S3 bucket: the agents' homes | Nothing in this procedure touches it |

The database alone is not a backup. Provider keys, connection secrets, MCP credentials, trace
credentials and every agent's home-repository password are stored sealed under the keys, so a dump
restored without them opens nothing. Losing the keys loses every home checkpoint as well. Take both
backups together and keep them apart from each other's storage.

This page is the manual procedure for the `cnpg` and `external` database modes. It uses exactly the
commands an automated backup job would run (`pg_dump --format=custom` and `keys export`), and
Koinon's nightly end-to-end test on kind runs these blocks as written, so they are known to work. The
examples use the release name `koinon` and the namespace `koinon`; replace them with yours.

A restore into the same install is below. For a different cluster, install the same chart version
first (see [Restore into a new install](#restore-into-a-new-install)).

## Back up

### 1. The encryption keys

Export the key set from the running API. The export is a Secret manifest without a namespace:

```sh e1=backup-keys
kubectl -n koinon exec deploy/koinon-api -- koinon-api keys export > koinon-kek-backup.yaml
```

Check that it holds the active key. This reads the active key's fingerprint from `keys status` and
looks for it in the export. Do it after each export, because a `keys add` and `keys activate` after
the export makes a backup that cannot open what was sealed since:

```sh e1=check-keys
fp=$(kubectl -n koinon exec deploy/koinon-api -- koinon-api keys status | awk '$2 == "active" { print $NF }')
test -n "$fp"
grep -q "fingerprint $fp" koinon-kek-backup.yaml || { echo "the backup does not hold the active key $fp" >&2; false; }
```

If you use a KMS, back up the KMS key as your KMS documents; the Secret then holds only wrapped
keys. The [key runbook](https://github.com/stateracloud/koinon/blob/main/docs/ops/secret-keys.md)
covers rotation, retired keys (keep every key that a database backup still needs) and what a lost key
costs.

### 2. The database

**cnpg mode.** The rendered CloudNativePG Cluster `<release>-pg` has no backup configured. Dump the
database from its primary pod. The database is `postgres.cnpg.database` (default `koinon`); the dump
covers every schema, including `dbos`:

```sh e1=backup-db
PRIMARY=$(kubectl -n koinon get pod -l cnpg.io/cluster=koinon-pg,cnpg.io/instanceRole=primary -o name)
kubectl -n koinon exec "$PRIMARY" -c postgres -- pg_dump --format=custom --dbname=koinon > koinon.dump
```

**external mode.** Take your provider's snapshot, or run `pg_dump --format=custom` as the migration
role against the same database. Use a `pg_dump` of the same major version as the server or newer.

The dump runs while Koinon is up and is consistent as of one instant. Runs, tasks and ledger entries
written after it are not in it.

Check that the dump can be read. `pg_restore --list` reads the whole archive's table of contents and
fails on a truncated file:

```sh e1=verify-dump
PRIMARY=$(kubectl -n koinon get pod -l cnpg.io/cluster=koinon-pg,cnpg.io/instanceRole=primary -o name)
kubectl -n koinon exec -i "$PRIMARY" -c postgres -- pg_restore --list < koinon.dump > /dev/null
```

Note the chart version (`helm -n koinon list`). Restore with that version. To restore onto a newer
chart, run `helm upgrade` after the restore so the migration Job brings the schema forward; a dump
never goes back into an older release's pods.

## Restore

Restore into a stopped install. The steps below replace the database with the dump and put the keys
back. Everything written after the backup is lost.

### 1. Stop

Scale agentd down first, so it drains, then the reconciler and the API. The database, the gateway and
OpenShell stay up. Sandboxes keep running while Koinon is stopped, but no run makes progress and no
schedule fires.

```sh e1=stop
kubectl -n koinon scale deployment/koinon-agentd --replicas=0
kubectl -n koinon wait --for=delete pod -l app.kubernetes.io/instance=koinon,app.kubernetes.io/component=agentd --timeout=10m
kubectl -n koinon scale deployment/koinon-reconciler deployment/koinon-api --replicas=0
kubectl -n koinon wait --for=delete pod -l 'app.kubernetes.io/instance=koinon,app.kubernetes.io/component in (api,reconciler)' --timeout=5m
```

If `wait` prints "no matching resources found", the pods were already gone.

### 2. Restore the database

Drop the schemas the dump will bring back, then load it. `--exit-on-error` stops at the first
problem instead of leaving a half-restored database.

**cnpg mode:**

```sh e1=restore-db
PRIMARY=$(kubectl -n koinon get pod -l cnpg.io/cluster=koinon-pg,cnpg.io/instanceRole=primary -o name)
kubectl -n koinon exec "$PRIMARY" -c postgres -- psql --dbname=koinon -v ON_ERROR_STOP=1 \
  -c 'DROP SCHEMA IF EXISTS dbos CASCADE' -c 'DROP SCHEMA public CASCADE' -c 'CREATE SCHEMA public AUTHORIZATION pg_database_owner' -c 'GRANT USAGE ON SCHEMA public TO PUBLIC'
kubectl -n koinon exec -i "$PRIMARY" -c postgres -- pg_restore --dbname=koinon --exit-on-error < koinon.dump
```

**external mode:** restore your provider's snapshot, or run the same two steps with `psql` and
`pg_restore` against your database as a role that owns the schemas.

Restore schema and data together into an empty schema, as above. A data-only restore onto a migrated
database stops on the install ID (see the runbook's "The install binding"). The install ID travels
with the dump, so a restored database and a restored key Secret match in either order.

### 3. Restore the keys

If the Secret is still there and is the one from the backup, there is nothing to do. If it is gone
or newer than the backup, restore the backup:

```sh e1=restore-keys
kubectl -n koinon apply -f koinon-kek-backup.yaml
```

koinon-api refuses to start without the Secret and never makes a new key when the database already
holds sealed values (it says "KEK Secret missing; restore it from your backup"). A Secret with the
wrong keys shows in the start-up log and in `keys status`.

### 4. Start

Scale the components back to the replica counts in your values (`helm -n koinon get values koinon`).
The default is one each. Start the API and reconciler first, then agentd:

```sh e1=start
kubectl -n koinon scale deployment/koinon-api deployment/koinon-reconciler --replicas=1
kubectl -n koinon rollout status deployment/koinon-api deployment/koinon-reconciler --timeout=5m
kubectl -n koinon scale deployment/koinon-agentd --replicas=1
kubectl -n koinon rollout status deployment/koinon-agentd --timeout=5m
```

Starting the API again is also what makes it read the restored install ID.

### 5. Rebuild the checkpoint list

The bucket was not restored with the database, so the restored list of home checkpoints is older than
the bucket: it lacks snapshots written since the dump, and it can name snapshots that retention has
since deleted. Rebuild it from the bucket once agentd is up (it is a no-op for an install without
checkpoints):

```sh e1=resync
kubectl -n koinon exec deploy/koinon-api -- koinon-api checkpoints resync --all
```

[After a database restore](/guides/home-checkpoints/#after-a-database-restore) says what it changes.
Run it before `keys forget` or any key compromise procedure.

## Check the result

1. `kubectl -n koinon get pods`: the three Deployments and the gateway are Ready.
2. `kubectl -n koinon exec deploy/koinon-api -- koinon-api keys status` lists the keys, marks the
   active one and prints no warning about a missing key or a fingerprint that differs.
3. Sign in, open an agent and a provider: a stored provider key is decrypted when a run starts, so
   start a run to prove it.
4. Pending schedules fire again on their next due time; workflows that were pending in the dump are
   recovered when koinon-api starts.

## Restore into a new install

To restore into another cluster or namespace, create the namespace and the OpenShell client
certificate and install the same chart version with your values, as in the
[Quickstart](/quickstart/#2-install). Then follow the Restore steps above from step 1. Apply the key
backup before the first start, if you can: a new install makes its own key Secret on first start,
and step 3 replaces it with yours. Use the same `postgres.mode`; a `cnpg` install cannot be pointed at
a database restored somewhere else.

### Two installs on one OpenShell gateway

A restored copy that runs next to the live install on the same gateway computes the same sandbox,
provider and workspace names, because the org and agent IDs are the same. Every OpenShell object Koinon
makes carries an install mark: a hash of a random token in the Secret `koinon-install-mark`, which the
chart's hook Job creates once. The token is not in Postgres, so a restored database does not bring it:
the copy gets its own, whatever its namespace is called and on whatever cluster it runs. A Koinon
install does not adopt, stop, change, exec in or delete an object that has another install's mark. The
reconcile of that agent fails with "owned by another install", the agent's row goes to `error` (so
`koinon-agentd` fails its runs and does not run them in the other install's sandbox), and the live
install is not touched. The copy cannot run those agents on that gateway: give it its own gateway for a
drill that needs running sandboxes.

The Secret outlives `helm uninstall` and upgrades. If it is lost (the namespace is deleted), the install
no longer owns its objects. To take them back, create `koinon-install-mark` again with the old `token`
(`installMark.existingSecret` names a Secret you manage), or delete the install's sandboxes, providers
and workspaces on the gateway so that the reconciler makes them again with the new mark.

Objects made before this mark existed have none and count as the install's own, so a restored copy next
to a live install that has never recreated them can still take them over. A sandbox gets the mark when
it is recreated (an image or spec change does that). Upgrading to this release adds the mark Secret
and the mark on objects the reconciler makes from then on; existing sandboxes keep working.

## What to automate, and what is not covered

An automated job runs the same commands on a schedule: a `pg_dump --format=custom` and a `keys export`
taken together, stored away from the cluster, with the fingerprint check after each. A CNPG
`ScheduledBackup` or your provider's snapshots are the equivalent for the database alone; they still
need the key backup beside them.

Not covered here: the checkpoint bucket's own durability (use your storage provider's versioning or
replication), and a standalone database mode, which has its own backup set
([ADR 0076](https://github.com/stateracloud/koinon/blob/main/docs/adr/0076-database-modes.md)).
Koinon records the time a restore took and the data it lost in the full restore drill it runs before
each release
([ADR 0085](https://github.com/stateracloud/koinon/blob/main/docs/adr/0085-upgrade-compatibility-and-recovery-policy.md));
there are no targets yet.
