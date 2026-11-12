# 0076. Database modes: bring your own, existing CNPG, or standalone Postgres

- Status: Accepted (delivery after v0.1.0, in v0.1.x)
- Date: 2026-10-28
- Area: install-operations
- Tags: database, install, backup-restore, helm-chart
- Amends: [0049](0049-v01-scope-decisions.md) ("Postgres: … No bundled StatefulSet")
- Relates to: [0003](0003-postgres-source-of-truth.md), [0047](0047-repository-scaffold.md), [0060](0060-migrations-as-a-job.md), [0066](0066-triggers-and-scheduling.md), [0075](0075-isolation-tiers.md), [0077](0077-pinned-openshell-and-installer.md)

## Context

The chart has two database modes today (`charts/koinon/values.yaml:190-223`):

- `cnpg` (the default) renders a CloudNativePG `Cluster`, with least-privilege roles and TLS
  (`templates/postgres-cnpg.yaml`);
- `external` reads one libpq URI per role from a Secret.

ADR 0049 decided "No bundled StatefulSet" (`0049:51-52`). The install preflight fails when the CNPG
operator is missing in `cnpg` mode (`preflight.yaml:59-68`).

procoduck, 2026-10-28: small teams don't want a CNPG dependency, and large ones use RDS or CNPG. Koinon
does not bundle the CNPG operator.

What Koinon needs from Postgres:

- **Postgres 18 or newer**, for the built-in `uuidv7()` that every primary key defaults to
  (`internal/db/migrations/00001_initial.sql:32`; ADR 0047 decision 3);
- **no extensions** (no `CREATE EXTENSION` in the migrations);
- `pg_notify` for the ledger's wake-up ([0058](0058-gateway-modes.md));
- the `dbos` schema, created by the migration Job when `api.durable.enabled` is set (ADR 0066;
  `prerequisites.md:61-66`);
- four roles: `migrate` owns the schema, and `api`, `reconciler` and `agentd` need only DML.

The `cnpg` template configures no backups today: there is no `backup` and no `ScheduledBackup`.

How comparable charts ship a simple Postgres:

| Chart | What it does |
| --- | --- |
| GitLab | No PostgreSQL dependency in `Chart.yaml` (master, chart 10.5.3). It requires an external PostgreSQL. |
| Gitea (`helm-gitea`) | PostgreSQL through CloudNativePG, "disabled by default … Set it to `true` once the operator is installed, or use an external database". |
| Authentik | Keeps a Bitnami `postgresql` dependency, off by default, and overrides its image to `docker.io/library/postgres:17.11-trixie` (`charts/authentik/values.yaml:1091-1097`). |
| Backstage | A Bitnami `postgresql` subchart, `enabled: false`. |
| Coder | No database in the chart. It recommends managed Postgres and installs Bitnami's chart by hand for proofs of concept. |

Bitnami changed its catalogue in 2025 (bitnami/containers#83267):

- From 2025-08-28 the existing images moved to `docker.io/bitnamilegacy`, which receives "no further
  updates or support".
- The free images are a small set, "only available on the 'latest' tag".
- The packaged OCI charts "will no longer receive updates", and deploying them "will not work
  out-of-the-box" without overriding the images.

The maintained Apache-2.0 alternative, CloudPirates' `postgres` chart (`appVersion` 18.6.0, chart
0.21.2), is a young third-party project with its own `common` dependency.

## Decision

### Three modes

1. **`postgres.mode` takes three values:**
   - `external`: bring your own Postgres 18 or newer, such as RDS, Aurora PostgreSQL, Cloud SQL,
     Azure Database or any other. As today: one URI per role from a Secret,
     `sslmode=verify-full` recommended. Session-mode connections only (amendment 2026-11-12).
   - `cnpg`: an **existing** CloudNativePG operator. The chart renders the `Cluster` and never
     installs the operator. As today.
   - `standalone`: Koinon's own single-replica Postgres, for small teams (decisions 3 to 7).

2. **There is no default mode.** The choice is the team's.
   - The chart fails to render when `postgres.mode` is unset, with: "set postgres.mode to
     external (your own Postgres 18+), cnpg (an existing CloudNativePG operator) or standalone (a
     single-replica Postgres for small teams)".
   - The installer's `eval` profile sets `standalone`
     ([0077](0077-pinned-openshell-and-installer.md)).
   - The docs present `external` or `cnpg` for production.
   - The install notes and the platform overview say which mode is in use, and say that
     `standalone` is one replica.

### Standalone

3. **What the chart renders:**
   - a `StatefulSet` with one replica, on the official `docker.io/library/postgres` image at an
     18.x tag, **pinned by digest** in `values.yaml` and rewritten by `global.imageRegistry` and
     `registryMirrors` like every image (ADR 0061);
   - a PVC (`postgres.standalone.storage.size`, default 10Gi, and `storageClass`);
   - a headless Service plus a ClusterIP Service;
   - a NetworkPolicy that admits only Koinon's components and the backup Job.

   It renders no third-party chart.

4. **The same security as `cnpg` mode:**
   - TLS on every TCP connection, with a server certificate the ensure-secrets hook generates from a
     chart-local CA. Clients verify it, and `pg_hba` rejects non-SSL connections.
   - The same roles, `koinon_migrate` (owner), `koinon_app` (NOLOGIN, DML) and the three login roles.
   - No superuser login over the network: the `postgres` superuser authenticates only over the local
     socket, for the init scripts and the backup.
   - Passwords come from the ensure-secrets hook Job and are never rendered, so renders stay
     deterministic (AGENTS.md).
   - The role setup runs in the migration Job before the migrations
     ([0060](0060-migrations-as-a-job.md)).
   - Pod and container security contexts are restricted, as for Koinon's own pods.

5. **Backups.** A `CronJob` runs `pg_dump --format=custom` of the Koinon database, the `dbos`
   schema included, as a dedicated backup role.
   - Default schedule `0 2 * * *` (UTC), keeping 7 dumps.
   - **Each run also copies the KEK Secret** (`secrets.kek`, default `koinon-kek`) next to the dump.
     Without the KEK, stored provider keys and connection secrets cannot be read
     (`quickstart.md:241-245`).
   - The target is a PVC (`postgres.standalone.backup.pvc`, the default) or an S3-compatible bucket
     (`postgres.standalone.backup.s3`, the same Secret keys as `checkpoints.s3`).
   - Each backup set is written atomically and named by time. A failed run is a failed Job, and the
     metric `koinon_db_backup_last_success_timestamp_seconds` goes stale.
   - Because the set includes the KEK, it decrypts everything. The docs say to keep the target as
     private as the database; the bucket's NetworkPolicy and IAM admit only the backup Job.
   - Optional `postgres.standalone.backup.ageRecipient` encrypts each set to an age public key.

6. **Restore** is a documented procedure, tested by a chart test:
   1. Scale Koinon's components to zero.
   2. Restore the KEK Secret.
   3. Run `pg_restore` into an empty database.
   4. Run the migration Job.
   5. Scale back up.

7. **Stated limits:**
   - one replica, no HA and no failover;
   - no point-in-time recovery, so a restore loses up to a day by default;
   - the node or zone of the PVC is a single point of failure;
   - Postgres major upgrades are manual (dump and restore).

   **Migration path:** take a final dump, then restore it into RDS or another Postgres 18 and switch
   to `external`, or bootstrap a CNPG `Cluster` with `initdb.import` (`microservice`) from the
   standalone Service and switch to `cnpg`. The guide covers both.

### Everything else stays

8. **No CNPG operator in Koinon's chart or installer** ([0077](0077-pinned-openshell-and-installer.md)).
9. **`cnpg` mode gains backups.** It renders an optional `ScheduledBackup` and a backup section when
   the user names an object store. The KEK is still backed up separately, and the docs say how.
10. **The preflight checks per mode:**
    - `cnpg`: the operator, as today;
    - `external`: connects with the migrate role, checks `server_version_num >= 180000` and the
      `CREATE` privilege when durable is on;
    - `standalone`: a storage class or an explicit PV.

## Amendment (2026-11-12): `external` needs session-mode connections

Every `external` URI must reach Postgres through a session-mode connection: direct, or through a
pooler in session mode. The reconciler's `LISTEN` and its session advisory lock (the leader lock,
which a follower now waits on with a blocking `pg_advisory_lock`, #573) need one Postgres session for
as long as the process leads. Transaction and statement pooling (PgBouncer's `pool_mode =
transaction` or `statement`) do not work.

## Consequences

- **A plain cluster is enough** with `standalone`: no operator and no cloud database.
- **Every install states its database choice.** Existing values files without `postgres.mode`
  fail on upgrade until they set it, and the release notes say so _(inferred)_.
- **Koinon owns a Postgres template:** image pins, CVE bumps, TLS, roles, backups and the restore
  test. Patch releases of Postgres become Koinon chart releases.
- **Small installs may run on `standalone` longer than they should.** The UI's mode notice and the
  backup metric make this visible, but they do not prevent it _(inferred)_.
- **Backups become explicit in every mode,** including CNPG, which had none.
- **The chart gets larger:** `hack/chart-check.sh` renders the new mode and asserts the TLS, role,
  NetworkPolicy and backup behaviour (AGENTS.md).
- **Follow-ups:**
  - the standalone template;
  - the backup CronJob and its restore test;
  - the external-mode version check;
  - CNPG backups;
  - the migration guide;
  - the quickstart and the `eval` profile setting `standalone`;
  - the prerequisites page.

## Alternatives

- **Bitnami's `postgresql` subchart.** It is the most common choice, but since 2025 its images
  freeze in `bitnamilegacy` or need a subscription, and the packaged charts get no updates.
  Rejected.
- **CloudPirates' `postgres` chart.** Maintained and Apache-2.0, but a third-party subchart from a
  young project, with the same kind of vendor risk. Rejected in favour of a small template Koinon
  owns.
- **Bundle the CNPG operator.** It is cluster-scoped, and it collides with an operator that is
  already installed. procoduck ruled it out.
- **Embedded Postgres inside koinon-api,** as Coder does when no URL is set. This does not fit the
  four roles or the migration Job, and it ties the database's lifecycle to the API pod. Rejected.
- **SQLite for small installs.** The queries are Postgres-specific, and the ledger and DBOS need
  Postgres. A second dialect contradicts ADR 0003. Rejected.
- **A default mode** (`cnpg` as today, or `standalone`). A default hides a choice with large
  consequences: `cnpg` fails without the operator, and `standalone` puts a production install on one
  replica unnoticed. Rejected in favour of a required choice (decision 2).

## Upstream verification

- 2026-10-28:
  - bitnami/containers#83267 (catalogue change, legacy repository, chart updates stopped);
  - GitLab chart `Chart.yaml` on master (no PostgreSQL dependency);
  - Gitea `helm-gitea` README;
  - goauthentik/helm `charts/authentik/Chart.yaml` and `values.yaml:1091-1097`;
  - backstage/charts README;
  - coder.com docs (Kubernetes install; embedded Postgres in `coder server`);
  - CloudPirates-io/helm-charts `charts/postgres/Chart.yaml`;
  - CloudNativePG v1.30.1 `docs/src/database_import.md`: "relies on `pg_dump` via a network
    connection to the origin host"; `microservice` and `monolith` types.

## Links

- [0003](0003-postgres-source-of-truth.md), [0049](0049-v01-scope-decisions.md),
  [0060](0060-migrations-as-a-job.md), [0066](0066-triggers-and-scheduling.md)
- https://github.com/bitnami/containers/issues/83267
- https://hub.docker.com/_/postgres
- https://cloudnative-pg.io/docs/current/database_import
