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
SELECT count(*) FROM trace_destinations
WHERE org_id = @org_id AND enabled;

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
-- previous test result stays. The router's health columns stay.
UPDATE trace_destinations
SET name = @name, backend = @backend, endpoint_url = @endpoint_url, params = @params,
    credentials = @credentials, secret_fields = @secret_fields, resource_attributes = @resource_attributes,
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
-- caller's transaction. Returns the orgs it changed with their levels before, for the audit log.
WITH before AS (
    SELECT org_id, content_level AS old_level, platform_copy_level AS old_copy_level
    FROM org_trace_settings
    WHERE content_level > @ceiling::trace_content_level
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
