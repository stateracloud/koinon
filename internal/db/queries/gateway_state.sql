-- SPDX-License-Identifier: Apache-2.0
--
-- The State stream's revisions and acks (ADR 0097 decision 2.1). Install-wide tables: no org_id.

-- name: LockGatewayStatePublish :exec
-- Serialises publishers for the length of the transaction. The reconciler leads alone, but a lost
-- lease can leave two for a moment, and "only when it differs from the latest" must not race.
SELECT pg_advisory_xact_lock(hashtextextended('koinon.gateway_state_revisions', 0));

-- name: GetLatestGatewayStateRevision :one
SELECT revision, snapshot, digest, created_at FROM gateway_state_revisions
ORDER BY revision DESC
LIMIT 1;

-- name: InsertGatewayStateRevision :one
INSERT INTO gateway_state_revisions (snapshot, digest) VALUES (@snapshot, @digest)
RETURNING revision;

-- name: NotifyGatewayStateRevision :exec
SELECT pg_notify('koinon_gateway_state', @payload::text);

-- name: PruneGatewayStateRevisions :execrows
-- Keeps the newest @keep revisions and drops any older than @max_age_seconds; never the newest one.
-- Their acks go with them.
DELETE FROM gateway_state_revisions
WHERE revision < (SELECT max(revision) FROM gateway_state_revisions)
  AND (revision NOT IN (SELECT revision FROM gateway_state_revisions ORDER BY revision DESC LIMIT @keep::int)
       OR created_at < now() - make_interval(secs => @max_age_seconds::int));

-- name: AckGatewayStateRevision :exec
INSERT INTO gateway_state_acks (copy_id, revision, half)
VALUES (@copy_id, @revision, @half)
ON CONFLICT DO NOTHING;

-- name: CountGatewayCopiesAcked :one
-- How many of the given copies confirmed @half at @revision or later (an ack covers every earlier
-- revision of its half).
SELECT count(DISTINCT copy_id) FROM gateway_state_acks
WHERE half = @half AND revision >= @revision AND copy_id = ANY(@copy_ids::text[]);

-- name: InsertMCPCallLog :exec
-- One row per tool call the gateway served. No arguments, results or token values (ADR 0097).
INSERT INTO mcp_call_log (org_slug, agent, target, tool, outcome, verdict, credential_kind, recipient_key_id, duration_ms)
VALUES (@org_slug, @agent, @target, @tool, @outcome, @verdict, @credential_kind, @recipient_key_id, @duration_ms);

-- name: TouchGatewayStateCopy :exec
-- A gateway copy holds the State stream open: it is live now.
INSERT INTO gateway_state_copies (copy_id, last_seen, serves_mcp, stream_id) VALUES (@copy_id, now(), @serves_mcp, @stream_id)
ON CONFLICT (copy_id) DO UPDATE SET last_seen = now(), serves_mcp = EXCLUDED.serves_mcp, stream_id = EXCLUDED.stream_id;

-- name: DeleteGatewayStateCopy :exec
-- Only the stream that touched the row last removes it.
DELETE FROM gateway_state_copies WHERE copy_id = @copy_id AND stream_id = @stream_id;

-- name: ListLiveGatewayStateCopies :many
-- The copies seen within @max_age_seconds (the database clock, so replicas need no common clock).
-- With @mcp_only, only the copies that serve the MCP half.
SELECT copy_id FROM gateway_state_copies
WHERE last_seen > now() - make_interval(secs => @max_age_seconds::int)
  AND (NOT @mcp_only::bool OR serves_mcp)
ORDER BY copy_id;

-- name: PruneGatewayStateCopies :execrows
DELETE FROM gateway_state_copies WHERE last_seen < now() - make_interval(secs => @max_age_seconds::int);
