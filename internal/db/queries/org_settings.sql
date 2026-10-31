-- SPDX-License-Identifier: Apache-2.0
--
-- Per-org latency tunables (plan item LT1). A NULL column is the platform default.

-- name: GetOrgSettings :one
SELECT * FROM org_settings
WHERE org_id = @org_id;

-- name: UpsertOrgSettings :one
INSERT INTO org_settings (org_id, idle_timeout_seconds, park_warm_seconds, queue_wait_seconds,
                          max_concurrent_runs, trigger_concurrency, trigger_rate_per_minute,
                          sandbox_wait_cap_seconds)
VALUES (@org_id, sqlc.narg(idle_timeout_seconds), sqlc.narg(park_warm_seconds), sqlc.narg(queue_wait_seconds),
        sqlc.narg(max_concurrent_runs), sqlc.narg(trigger_concurrency), sqlc.narg(trigger_rate_per_minute),
        sqlc.narg(sandbox_wait_cap_seconds))
ON CONFLICT (org_id) DO UPDATE
SET idle_timeout_seconds = EXCLUDED.idle_timeout_seconds,
    park_warm_seconds = EXCLUDED.park_warm_seconds,
    queue_wait_seconds = EXCLUDED.queue_wait_seconds,
    max_concurrent_runs = EXCLUDED.max_concurrent_runs,
    trigger_concurrency = EXCLUDED.trigger_concurrency,
    trigger_rate_per_minute = EXCLUDED.trigger_rate_per_minute,
    sandbox_wait_cap_seconds = EXCLUDED.sandbox_wait_cap_seconds
RETURNING *;
