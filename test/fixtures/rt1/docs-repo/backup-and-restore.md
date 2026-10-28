# Backup and restore

Owner: Platform. A backup that has never been restored does not count.

## What is backed up

| Data | Method | Frequency | Retention | Copy location |
|---|---|---|---|---|
| Production databases | continuous WAL archive plus a full base backup | WAL: continuous; base: daily | 30 days | second region, separate account |
| Object storage (uploads) | versioning plus cross-region replication | continuous | 90 days of versions | second region |
| Git repositories | mirror to the backup host | hourly | 1 year | offsite disk, rotated monthly |
| Cluster configuration | Git is the source of truth; no separate backup | n/a | n/a | n/a |
| Lab volumes (persistent) | volume snapshots | nightly | 14 days | same site, other node |

## Rules

1. Backups must be encrypted with a key that is not stored with the backup.
2. Backup credentials must be write-only for the systems being backed up. A compromised
   database host must not be able to delete its own backups.
3. Retention is enforced by the backup system, not by people deleting files.

## Restore tests

- Production databases: a full restore into an isolated environment every month. The test passes
  when row counts match and the application's smoke tests succeed.
- Git mirrors: a clone from the mirror every quarter.
- Lab volumes: restore one volume per quarter to a scratch namespace.
- Results go in the restore log (a table in the Platform wiki: date, system, duration, outcome).

## Recovery targets

| Tier | Recovery point objective | Recovery time objective |
|---|---|---|
| Tier 1 (customer-facing databases) | 5 minutes | 2 hours |
| Tier 2 (internal tools) | 24 hours | 1 working day |
| Tier 3 (lab and experiments) | 7 days | best effort |
