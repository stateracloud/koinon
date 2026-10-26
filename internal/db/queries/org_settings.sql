-- SPDX-License-Identifier: Apache-2.0
--
-- Per-org latency tunables (plan item LT1). A NULL column is the platform default.

-- name: GetOrgSettings :one
SELECT * FROM org_settings
WHERE org_id = @org_id;

-- name: UpsertOrgSettings :one
INSERT INTO org_settings (org_id, idle_timeout_seconds, park_warm_seconds, queue_wait_seconds)
VALUES (@org_id, sqlc.narg(idle_timeout_seconds), sqlc.narg(park_warm_seconds), sqlc.narg(queue_wait_seconds))
ON CONFLICT (org_id) DO UPDATE
SET idle_timeout_seconds = EXCLUDED.idle_timeout_seconds,
    park_warm_seconds = EXCLUDED.park_warm_seconds,
    queue_wait_seconds = EXCLUDED.queue_wait_seconds
RETURNING *;
