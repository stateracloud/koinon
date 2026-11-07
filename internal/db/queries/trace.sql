-- SPDX-License-Identifier: Apache-2.0
--
-- Run traces (rc.9, ADR 0074): destinations, content levels, sampling, the traced flag of a run and
-- the turns of a run. Every query on an org-owned table takes @org_id (TestQueriesAreOrgScoped).

-- name: ListTraceDestinations :many
SELECT * FROM trace_destinations
WHERE org_id = @org_id
ORDER BY name;

-- name: GetTraceDestination :one
SELECT * FROM trace_destinations
WHERE org_id = @org_id AND id = @id;

-- name: CountEnabledTraceDestinations :one
-- A destination whose credentials were lost (#85) receives nothing, so it does not count.
SELECT count(*) FROM trace_destinations
WHERE org_id = @org_id AND enabled AND credential_lost_at IS NULL;

-- name: InsertTraceDestination :one
INSERT INTO trace_destinations (org_id, id, name, backend, endpoint_url, params, credentials, secret_fields,
                                resource_attributes, enabled, last_test_at, last_test_ok, last_test_message, created_by)
VALUES (@org_id, @id, @name, @backend, @endpoint_url, @params, @credentials, @secret_fields,
        @resource_attributes, @enabled, @last_test_at, @last_test_ok, @last_test_message, @created_by)
RETURNING *;

-- name: UpdateTraceDestination :one
-- Replaces the settings of a destination. Applies only to the version the caller read
-- (@expected_version), so two admins editing at once do not overwrite each other's secrets, and bumps
-- config_version. @set_test says the change was tested and the test columns are recorded; otherwise the
-- previous test result stays. The router's health columns stay. A tested change ends lost credentials
-- (#85): it carried every secret the backend needs (the trigger credential_lost_clear also clears the
-- mark when credentials are stored).
UPDATE trace_destinations
SET name = @name, backend = @backend, endpoint_url = @endpoint_url, params = @params,
    credentials = @credentials, secret_fields = @secret_fields, resource_attributes = @resource_attributes,
    credential_lost_at = CASE WHEN @set_test::boolean THEN NULL ELSE credential_lost_at END,
    enabled = @enabled, config_version = config_version + 1,
    last_test_at = CASE WHEN @set_test::boolean THEN @last_test_at ELSE last_test_at END,
    last_test_ok = CASE WHEN @set_test::boolean THEN @last_test_ok ELSE last_test_ok END,
    last_test_message = CASE WHEN @set_test::boolean THEN @last_test_message ELSE last_test_message END
WHERE org_id = @org_id AND id = @id AND config_version = @expected_version
RETURNING *;

-- name: SetTraceDestinationTest :execrows
-- Records the result of a re-test, only if the configuration is the one that was tested.
UPDATE trace_destinations
SET last_test_at = @last_test_at, last_test_ok = @last_test_ok, last_test_message = @last_test_message
WHERE org_id = @org_id AND id = @id AND config_version = @expected_version;

-- name: DeleteTraceDestination :execrows
DELETE FROM trace_destinations
WHERE org_id = @org_id AND id = @id;

-- name: RecordTraceDestinationSuccess :exec
-- The router's health: a batch was accepted.
UPDATE trace_destinations
SET last_success_at = now()
WHERE org_id = @org_id AND id = @id;

-- name: RecordTraceDestinationError :exec
-- The router's health: a batch failed, or spans were dropped. The message is secret-free.
UPDATE trace_destinations
SET last_error_at = now(), last_error = @last_error,
    spans_dropped = spans_dropped + @spans_dropped::bigint
WHERE org_id = @org_id AND id = @id;

-- name: GetOrgTraceSettings :one
SELECT * FROM org_trace_settings
WHERE org_id = @org_id;

-- name: UpsertOrgTraceSettings :one
INSERT INTO org_trace_settings (org_id, content_level, sample_rate, platform_copy_level, updated_by)
VALUES (@org_id, @content_level, @sample_rate, @platform_copy_level, @updated_by)
ON CONFLICT (org_id) DO UPDATE
SET content_level = EXCLUDED.content_level, sample_rate = EXCLUDED.sample_rate,
    platform_copy_level = EXCLUDED.platform_copy_level, updated_by = EXCLUDED.updated_by
RETURNING *;

-- name: LowerOrgTraceLevels :many
-- Lowers every org above a new platform ceiling to it, and the platform copies that follow, in the
-- caller's transaction. Returns the orgs it changed with their levels before, for the audit log, in org ID
-- order: the row locks are taken in that order, and the caller writes one audit event per org in that
-- order (ADR 0100 decision 1.7).
WITH before AS (
    SELECT org_id, content_level AS old_level, platform_copy_level AS old_copy_level
    FROM org_trace_settings
    WHERE content_level > @ceiling::trace_content_level
    ORDER BY org_id
    FOR UPDATE
), changed AS (
    UPDATE org_trace_settings s
    SET content_level = @ceiling::trace_content_level,
        platform_copy_level = LEAST(s.platform_copy_level, @ceiling::trace_content_level),
        updated_by = @updated_by
    FROM before b
    WHERE s.org_id = b.org_id
    RETURNING s.org_id
)
SELECT b.org_id, b.old_level, b.old_copy_level
FROM before b
JOIN changed c ON c.org_id = b.org_id
ORDER BY b.org_id;

-- name: GetPlatformTraceSettings :one
SELECT * FROM platform_trace_settings
WHERE singleton;

-- name: ShareLockPlatformTraceSettings :one
-- Reads the ceiling and keeps it from being lowered until the transaction ends, without blocking
-- other readers of it (org settings writes).
SELECT * FROM platform_trace_settings
WHERE singleton
FOR SHARE;

-- name: LockPlatformTraceSettings :one
-- Reads the ceiling and holds the row until the transaction ends, so an org's level and a lowering
-- of the ceiling cannot pass each other.
SELECT * FROM platform_trace_settings
WHERE singleton
FOR UPDATE;

-- name: SetPlatformTraceCeiling :one
UPDATE platform_trace_settings
SET content_ceiling = @content_ceiling, updated_by = @updated_by, updated_at = now()
WHERE singleton
RETURNING *;

-- name: GetPlatformTraceDestination :one
SELECT * FROM platform_trace_destination
WHERE singleton;

-- name: LockPlatformTraceDestination :one
SELECT * FROM platform_trace_destination
WHERE singleton
FOR UPDATE;

-- name: UpsertPlatformTraceDestination :one
-- The platform destination has one writer at a time: a platform admin. A change bumps config_version.
-- @set_test as for UpdateTraceDestination.
INSERT INTO platform_trace_destination (singleton, backend, endpoint_url, params, credentials, secret_fields,
                                        resource_attributes, enabled, last_test_at, last_test_ok, last_test_message, updated_by)
VALUES (true, @backend, @endpoint_url, @params, @credentials, @secret_fields,
        @resource_attributes, @enabled, @last_test_at, @last_test_ok, @last_test_message, @updated_by)
ON CONFLICT (singleton) DO UPDATE
SET backend = EXCLUDED.backend, endpoint_url = EXCLUDED.endpoint_url, params = EXCLUDED.params,
    credentials = EXCLUDED.credentials, secret_fields = EXCLUDED.secret_fields,
    resource_attributes = EXCLUDED.resource_attributes, enabled = EXCLUDED.enabled,
    credential_lost_at = CASE WHEN @set_test::boolean THEN NULL ELSE platform_trace_destination.credential_lost_at END,
    config_version = platform_trace_destination.config_version + 1,
    last_test_at = CASE WHEN @set_test::boolean THEN EXCLUDED.last_test_at ELSE platform_trace_destination.last_test_at END,
    last_test_ok = CASE WHEN @set_test::boolean THEN EXCLUDED.last_test_ok ELSE platform_trace_destination.last_test_ok END,
    last_test_message = CASE WHEN @set_test::boolean THEN EXCLUDED.last_test_message ELSE platform_trace_destination.last_test_message END,
    updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING *;

-- name: SetPlatformTraceDestinationTest :execrows
-- Records the result of a re-test, only if the configuration is the one that was tested. A deleted
-- destination matches nothing and is not brought back.
UPDATE platform_trace_destination
SET last_test_at = @last_test_at, last_test_ok = @last_test_ok, last_test_message = @last_test_message
WHERE singleton AND config_version = @expected_version;

-- name: DeletePlatformTraceDestination :execrows
DELETE FROM platform_trace_destination
WHERE singleton;

-- name: RecordPlatformTraceDestinationSuccess :exec
UPDATE platform_trace_destination
SET last_success_at = now()
WHERE singleton;

-- name: RecordPlatformTraceDestinationError :exec
UPDATE platform_trace_destination
SET last_error_at = now(), last_error = @last_error,
    spans_dropped = spans_dropped + @spans_dropped::bigint
WHERE singleton;

-- name: DecideRunTraced :one
-- Stores whether the run is traced, once: a run decided earlier keeps its decision (a resume after a
-- park claims the run again). Returns the stored decision.
UPDATE runs
SET traced = coalesce(traced, @traced::boolean)
WHERE org_id = @org_id AND id = @id
RETURNING traced::boolean AS traced;

-- name: GetRunTraced :one
SELECT traced FROM runs
WHERE org_id = @org_id AND id = @id;

-- name: InsertRunTurn :one
-- Starts the run's next turn with the given trace ID. seq comes from the run's counter, taken under
-- the run's row lock (the way run_events.seq does), so concurrent writers cannot pick the same number.
WITH run AS (
    UPDATE runs
    SET last_turn_seq = last_turn_seq + 1
    WHERE runs.org_id = @org_id AND runs.id = @run_id
    RETURNING runs.org_id, runs.id, runs.last_turn_seq
)
INSERT INTO run_turns (org_id, run_id, seq, trace_id)
SELECT run.org_id, run.id, run.last_turn_seq, @trace_id
FROM run
RETURNING *;

-- name: InsertOwnedRunTurn :one
-- InsertRunTurn for the run's current owner only (the epoch matches and the run is starting or
-- running), so a fenced replica cannot add a turn. A turn row the previous owner left open (it died or
-- lost the run) is ended as interrupted in the same statement, before the new one starts.
WITH run AS (
    UPDATE runs
    SET last_turn_seq = last_turn_seq + 1
    WHERE runs.org_id = @org_id AND runs.id = @run_id AND runs.epoch = @epoch
      AND runs.state IN ('starting', 'running')
    RETURNING runs.org_id, runs.id, runs.last_turn_seq
), interrupted AS (
    UPDATE run_turns
    SET ended_at = now(), end_reason = 'interrupted'
    WHERE run_turns.org_id = @org_id AND run_turns.run_id = @run_id AND run_turns.ended_at IS NULL
      AND EXISTS (SELECT 1 FROM run)
)
INSERT INTO run_turns (org_id, run_id, seq, trace_id)
SELECT run.org_id, run.id, run.last_turn_seq, @trace_id
FROM run
RETURNING *;

-- name: EndRunTurn :one
UPDATE run_turns
SET ended_at = now(), end_reason = @end_reason
WHERE org_id = @org_id AND id = @id AND ended_at IS NULL
RETURNING *;

-- name: GetRunTurn :one
SELECT * FROM run_turns
WHERE org_id = @org_id AND run_id = @run_id AND seq = @seq;

-- name: GetRunTurnByTrace :one
SELECT * FROM run_turns
WHERE org_id = @org_id AND trace_id = @trace_id;

-- name: ListRunTurns :many
SELECT * FROM run_turns
WHERE org_id = @org_id AND run_id = @run_id
ORDER BY seq;

-- name: ListRunModelCalls :many
-- The model calls of a run in a turn's window, for the trace router's model-call spans (ADR 0074,
-- decision 3): the ledger's rows with the provider type and ID of the model the agent asked for. The
-- model name is the one the agent used, with or without the org slug the gateway prefixes. A row at or
-- after the next turn's start belongs to that turn, so a boundary row counts once.
SELECT e.request_id, e.model, e.agent_id, e.task_id, e.input_tokens, e.output_tokens,
       e.cache_read_tokens, e.cache_write_tokens, e.reasoning_tokens,
       coalesce(e.reasoning_known, e.reasoning_tokens > 0)::boolean AS reasoning_known,
       e.cost_usd, e.priced, e.gateway_cost_usd, e.provisional, e.estimated, e.upstream_status,
       e.occurred_at, coalesce(p.type, '')::text AS provider_type, m.id AS model_id
FROM spend_entries e
JOIN orgs o ON o.id = e.org_id
LEFT JOIN models m ON m.org_id = e.org_id AND (m.name = e.model OR o.slug || '/' || m.name = e.model)
LEFT JOIN providers p ON p.org_id = m.org_id AND p.id = m.provider_id
WHERE e.org_id = @org_id AND e.run_id = @run_id
  AND e.occurred_at >= @from_time AND e.occurred_at < @to_time
  AND e.occurred_at < coalesce((SELECT t.started_at FROM run_turns t
                                WHERE t.org_id = e.org_id AND t.run_id = e.run_id AND t.seq = sqlc.arg(turn_seq)::integer + 1), CAST('infinity' AS timestamptz))
ORDER BY e.occurred_at, e.request_id;
