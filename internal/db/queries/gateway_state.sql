-- SPDX-License-Identifier: Apache-2.0
--
-- The State stream's revisions and acks (ADR 0097 decision 2.1). Install-wide tables: no org_id.

-- name: LockGatewayStatePublish :exec
-- Serialises publishers for the length of the transaction. The reconciler leads alone, but a lost
-- lease can leave two for a moment, and "only when it differs from the latest" must not race.
SELECT pg_advisory_xact_lock(hashtextextended('koinon.gateway_state_revisions', 0));

-- name: GetLatestGatewayStateRevision :one
SELECT revision, snapshot, created_at FROM gateway_state_revisions
ORDER BY revision DESC
LIMIT 1;

-- name: InsertGatewayStateRevision :one
INSERT INTO gateway_state_revisions (snapshot) VALUES (@snapshot)
RETURNING revision;

-- name: NotifyGatewayStateRevision :exec
SELECT pg_notify('koinon_gateway_state', @payload::text);

-- name: PruneGatewayStateRevisions :execrows
-- Keeps the newest @keep revisions; their acks go with the rest.
DELETE FROM gateway_state_revisions
WHERE revision NOT IN (
    SELECT revision FROM gateway_state_revisions ORDER BY revision DESC LIMIT @keep::int
);

-- name: AckGatewayStateRevision :exec
INSERT INTO gateway_state_acks (copy_id, revision, half)
VALUES (@copy_id, @revision, @half)
ON CONFLICT DO NOTHING;

-- name: CountGatewayCopiesAcked :one
-- How many of the given copies confirmed @half at @revision or later (an ack covers every earlier
-- revision of its half).
SELECT count(DISTINCT copy_id) FROM gateway_state_acks
WHERE half = @half AND revision >= @revision AND copy_id = ANY(@copy_ids::text[]);
