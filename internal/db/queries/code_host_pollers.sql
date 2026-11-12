-- SPDX-License-Identifier: Apache-2.0
--
-- The shared code-host pollers (ADR 0066, amendment of 2026-11-12; migration 00083).

-- name: EnsureCodeHostPoller :exec
-- A trigger needs the poller of (connection, resource): created due at @now (the pass's clock),
-- or left as it is.
INSERT INTO code_host_pollers (org_id, connection_id, resource, next_poll_at)
VALUES (@org_id, @connection_id, @resource, @now)
ON CONFLICT (org_id, connection_id, resource) DO NOTHING;

-- name: DeleteCodeHostPollersNotIn :execrows
-- Drops the org's pollers no trigger in effect needs any more; keep holds connection_id || ' ' ||
-- resource of the needed ones.
DELETE FROM code_host_pollers
WHERE org_id = @org_id
  AND NOT ((connection_id::text || ' ' || resource) = ANY (@keep::text[]));

-- name: ListOrgIDsWithCodeHostPollers :many
-- The orgs that have a poller, returning org ids only; the sync and the claim after it are scoped
-- by @org_id.
SELECT DISTINCT p.org_id FROM code_host_pollers p;

-- name: ClaimDueCodeHostPollers :many
-- Claims the org's pollers due at @now by moving next_poll_at to @lease_until, so a poll that
-- dies with its leader is retried after the lease and not polled twice meanwhile (SKIP LOCKED
-- leaves a row another transaction holds). SavePoll writes the real next_poll_at.
UPDATE code_host_pollers p SET next_poll_at = @lease_until
WHERE p.org_id = @org_id AND (p.org_id, p.id) IN (
    SELECT d.org_id, d.id FROM code_host_pollers d
    WHERE d.org_id = @org_id AND d.next_poll_at <= @now
    ORDER BY d.next_poll_at
    LIMIT @max_rows
    FOR UPDATE SKIP LOCKED
)
RETURNING p.*;

-- name: NextCodeHostPollAt :one
-- The earliest next_poll_at over every org's pollers, NULL when there are none: how long the
-- leader may sleep. A time only, no org.
SELECT min(next_poll_at)::timestamptz AS next_poll_at FROM code_host_pollers;

-- name: SaveCodeHostPoll :exec
-- Stores one poll's outcome.
UPDATE code_host_pollers SET
    validator_kind = @validator_kind, validator = @validator, cursor = @cursor,
    baseline_at = coalesce(baseline_at, sqlc.narg(baseline_at)),
    provider_interval_s = @provider_interval_s, next_poll_at = @next_poll_at,
    backoff_until = sqlc.narg(backoff_until), consecutive_failures = @consecutive_failures,
    last_status = @last_status, last_error = @last_error, last_polled_at = @polled_at
WHERE org_id = @org_id AND id = @id;

-- name: GetCodeHostPoller :one
SELECT * FROM code_host_pollers
WHERE org_id = @org_id AND connection_id = @connection_id AND resource = @resource;

-- name: ListCodeHostPollers :many
SELECT * FROM code_host_pollers WHERE org_id = @org_id ORDER BY connection_id, resource;
