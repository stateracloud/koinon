-- SPDX-License-Identifier: Apache-2.0
--
-- Per-org latency tunables (plan item LT1). A NULL column is the platform default.

-- name: GetOrgSettings :one
SELECT * FROM org_settings
WHERE org_id = @org_id;

-- name: UpsertOrgSettings :one
INSERT INTO org_settings (org_id, idle_timeout_seconds, park_warm_seconds, queue_wait_seconds,
                          max_concurrent_runs, trigger_concurrency, trigger_rate_per_minute,
                          sandbox_wait_cap_seconds, sandbox_creations_per_minute)
VALUES (@org_id, sqlc.narg(idle_timeout_seconds), sqlc.narg(park_warm_seconds), sqlc.narg(queue_wait_seconds),
        sqlc.narg(max_concurrent_runs), sqlc.narg(trigger_concurrency), sqlc.narg(trigger_rate_per_minute),
        sqlc.narg(sandbox_wait_cap_seconds), sqlc.narg(sandbox_creations_per_minute))
ON CONFLICT (org_id) DO UPDATE
SET idle_timeout_seconds = EXCLUDED.idle_timeout_seconds,
    park_warm_seconds = EXCLUDED.park_warm_seconds,
    queue_wait_seconds = EXCLUDED.queue_wait_seconds,
    max_concurrent_runs = EXCLUDED.max_concurrent_runs,
    trigger_concurrency = EXCLUDED.trigger_concurrency,
    trigger_rate_per_minute = EXCLUDED.trigger_rate_per_minute,
    sandbox_wait_cap_seconds = EXCLUDED.sandbox_wait_cap_seconds,
    sandbox_creations_per_minute = EXCLUDED.sandbox_creations_per_minute
RETURNING *;

-- name: SetOrgRunLimits :exec
-- The org's run concurrency, trigger queue and sandbox creation limits (DB2, ADR 0066, issues #462
-- and #27); NULL is the platform default. It touches only these four columns, so the latency overrides and the other
-- settings stay.
INSERT INTO org_settings (org_id, max_concurrent_runs, trigger_concurrency, trigger_rate_per_minute,
                          sandbox_creations_per_minute)
VALUES (@org_id, sqlc.narg(max_concurrent_runs), sqlc.narg(trigger_concurrency), sqlc.narg(trigger_rate_per_minute),
        sqlc.narg(sandbox_creations_per_minute))
ON CONFLICT (org_id) DO UPDATE
SET max_concurrent_runs = EXCLUDED.max_concurrent_runs,
    trigger_concurrency = EXCLUDED.trigger_concurrency,
    trigger_rate_per_minute = EXCLUDED.trigger_rate_per_minute,
    sandbox_creations_per_minute = EXCLUDED.sandbox_creations_per_minute;

-- name: SetOrgLeakMatchThreshold :exec
-- The leak guard's match threshold in characters (ADR 0084 decision 3); NULL is the platform
-- default. It touches only this column, so the latency and limit overrides stay.
INSERT INTO org_settings (org_id, leak_match_threshold)
VALUES (@org_id, sqlc.narg(leak_match_threshold))
ON CONFLICT (org_id) DO UPDATE SET leak_match_threshold = EXCLUDED.leak_match_threshold;

-- name: ListOrgFetchTrustedHosts :many
-- The org's fetch-trusted host list (ADR 0084 decision 5). Empty for a new org.
SELECT * FROM org_fetch_trusted_hosts
WHERE org_id = @org_id
ORDER BY host;

-- name: AddOrgFetchTrustedHost :execrows
-- Adding a host that is on the list changes nothing (0 rows).
INSERT INTO org_fetch_trusted_hosts (org_id, host)
VALUES (@org_id, @host)
ON CONFLICT (org_id, host) DO NOTHING;

-- name: DeleteOrgFetchTrustedHost :execrows
DELETE FROM org_fetch_trusted_hosts
WHERE org_id = @org_id AND host = @host;

-- name: SetOrgRunContentRetention :exec
-- How many days a finished run's stored content is kept (issue #69); NULL is the platform default.
-- It touches only this column, so the latency and limit overrides stay.
INSERT INTO org_settings (org_id, run_content_retention_days)
VALUES (@org_id, sqlc.narg(run_content_retention_days))
ON CONFLICT (org_id) DO UPDATE SET run_content_retention_days = EXCLUDED.run_content_retention_days;

-- name: SetOrgHomeBackupRetention :exec
-- How long an old home backup repository is kept after a key change (#219); NULL is the platform
-- default. It touches only this column.
INSERT INTO org_settings (org_id, home_backup_retention_days)
VALUES (@org_id, sqlc.narg(home_backup_retention_days))
ON CONFLICT (org_id) DO UPDATE SET home_backup_retention_days = EXCLUDED.home_backup_retention_days;

-- name: EnsureOrgSettings :exec
-- Makes sure the org has an org_settings row, so LockOrgSettings has a row to lock.
INSERT INTO org_settings (org_id) VALUES (@org_id) ON CONFLICT (org_id) DO NOTHING;

-- name: LockOrgSettings :one
-- Serializes the writers of one org's fetch-trusted list: the cap is checked under this lock.
SELECT org_id FROM org_settings WHERE org_id = @org_id FOR UPDATE;

-- name: LockOrgSandboxCreations :exec
-- Serializes the reconciler workers that reserve a creation for one org, for the transaction.
SELECT pg_advisory_xact_lock(hashtextextended('sandbox_creations:' || (@org_id::uuid)::text, 0));

-- name: CountOrgSandboxCreations :one
-- Creations of the org in the last minute.
SELECT count(*)::int FROM sandbox_creations
WHERE org_id = @org_id AND created_at > clock_timestamp() - interval '1 minute';

-- name: SandboxCreationSlotFreesIn :one
-- Seconds until the oldest creation still inside the minute leaves it, so a slot frees.
SELECT coalesce(extract(epoch FROM min(created_at) + interval '1 minute' - clock_timestamp()), 0)::float8 FROM sandbox_creations
WHERE org_id = @org_id AND created_at > clock_timestamp() - interval '1 minute';

-- name: RecordSandboxCreation :one
INSERT INTO sandbox_creations (org_id, agent_id, kind) VALUES (@org_id, @agent_id, @kind)
RETURNING id;

-- name: ReleaseSandboxCreation :exec
-- Gives a slot back when the backend call it was taken for failed, so a failing agent cannot starve
-- the org's healthy ones.
DELETE FROM sandbox_creations WHERE org_id = @org_id AND id = @id;

-- name: PruneOrgSandboxCreations :exec
DELETE FROM sandbox_creations WHERE org_id = @org_id AND created_at < now() - interval '1 hour';
