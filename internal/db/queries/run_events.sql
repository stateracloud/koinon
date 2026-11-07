-- SPDX-License-Identifier: Apache-2.0
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: AppendRunEvent :one
-- Appends an event as the run's owner. seq is taken from the run row under its
-- lock, so events are gap-free and ordered per run. No row: stale epoch or the
-- run is not active, and nothing is written.
WITH run AS (
    UPDATE runs
    SET last_event_seq = last_event_seq + 1
    WHERE runs.org_id = @org_id AND runs.id = @run_id AND runs.epoch = @epoch
      AND runs.state IN ('starting', 'running')
    RETURNING runs.org_id, runs.id, runs.epoch, runs.last_event_seq
)
INSERT INTO run_events (org_id, run_id, seq, epoch, kind, payload)
SELECT run.org_id, run.id, run.last_event_seq, run.epoch, @kind, @payload
FROM run
RETURNING *;

-- name: AppendRunControlEvent :one
-- Appends an event from the control plane (API, scheduler) without an epoch,
-- for example a cancel notice. Stamped with the run's current epoch.
WITH run AS (
    UPDATE runs
    SET last_event_seq = last_event_seq + 1
    WHERE runs.org_id = @org_id AND runs.id = @run_id
    RETURNING runs.org_id, runs.id, runs.epoch, runs.last_event_seq
)
INSERT INTO run_events (org_id, run_id, seq, epoch, kind, payload)
SELECT run.org_id, run.id, run.last_event_seq, run.epoch, @kind, @payload
FROM run
RETURNING *;

-- name: ListRunEventsAfter :many
-- Events after a sequence number (0 for the start), in order. WatchRun replays
-- from the client's last seq, then follows.
SELECT * FROM run_events
WHERE org_id = @org_id AND run_id = @run_id AND seq > @after_seq
ORDER BY seq
LIMIT @max_events;

-- name: AppendRunEventOnce :one
-- AppendRunEvent with the writer's own sequence number: an event already
-- recorded under (run, epoch, source_seq) is not recorded again, so retrying
-- after an ambiguous failure is safe. No row: stale epoch, the run is not
-- active, or the event exists (GetRunEventBySource tells which).
WITH run AS (
    UPDATE runs
    SET last_event_seq = last_event_seq + 1
    WHERE runs.org_id = @org_id AND runs.id = @run_id AND runs.epoch = @epoch
      AND runs.state IN ('starting', 'running')
      AND NOT EXISTS (
          SELECT 1 FROM run_events e
          WHERE e.org_id = @org_id AND e.run_id = @run_id AND e.epoch = @epoch
            AND e.source_seq = @source_seq::bigint)
    RETURNING runs.org_id, runs.id, runs.epoch, runs.last_event_seq
)
INSERT INTO run_events (org_id, run_id, seq, epoch, kind, payload, source_seq)
SELECT run.org_id, run.id, run.last_event_seq, run.epoch, @kind, @payload, @source_seq::bigint
FROM run
RETURNING *;

-- name: AppendCanceledRunEventOnce :one
-- AppendRunEventOnce for the run's own epoch after the control plane canceled it: the worker
-- stores the item (a message or a thought) the agent was in the middle of, so a stopped run keeps
-- what was said (old issue 344). Nothing else is recorded this way. No row: another epoch, the run is not
-- canceled, or the event exists.
WITH run AS (
    UPDATE runs
    SET last_event_seq = last_event_seq + 1
    WHERE runs.org_id = @org_id AND runs.id = @run_id AND runs.epoch = @epoch
      AND runs.state = 'canceled'
      AND NOT EXISTS (
          SELECT 1 FROM run_events e
          WHERE e.org_id = @org_id AND e.run_id = @run_id AND e.epoch = @epoch
            AND e.source_seq = @source_seq::bigint)
    RETURNING runs.org_id, runs.id, runs.epoch, runs.last_event_seq
)
INSERT INTO run_events (org_id, run_id, seq, epoch, kind, payload, source_seq)
SELECT run.org_id, run.id, run.last_event_seq, run.epoch, @kind, @payload, @source_seq::bigint
FROM run
RETURNING *;

-- name: ListRunsToPurgeContent :many
-- Finished runs of the org that finished before @cutoff and whose content the purge has not
-- cleared yet (issue #69), oldest first and after the (finished_at, id) keyset the caller passes
-- (the epoch and the nil UUID to start). It reads runs_content_unpurged only, never run_events.
SELECT r.id, r.finished_at
FROM runs r
WHERE r.org_id = @org_id
  AND r.finished_at IS NOT NULL
  AND r.content_purged_at IS NULL
  AND r.finished_at < @cutoff::timestamptz
  AND (r.finished_at, r.id) > (@after_finished_at::timestamptz, @after_id::uuid)
ORDER BY r.finished_at, r.id
LIMIT @batch_size::int;

-- name: PurgeRunEventContent :execrows
-- Blanks the payload of up to @batch_size content events (@kinds: messages, thoughts, tool calls)
-- of one run (issue #69). The row stays, so the run page keeps its skeleton and seq stays gap-free;
-- only the payload goes and content_purged_at says when. A kind that is a key of @strip (a JSON
-- object of kind -> content keys) loses only those keys and keeps its state fields (run.prompt keeps
-- throughSeq, acp.permission keeps decision). Events of other kinds (state, park, cost notes) keep
-- their payload. Rows are locked with SKIP LOCKED, so two purgers never
-- wait for each other. Rows 0: nothing left to blank in this run.
WITH batch AS (
    SELECT e.org_id, e.id
    FROM run_events e
    WHERE e.org_id = @org_id AND e.run_id = @run_id
      AND e.kind = ANY(@kinds::text[])
      AND e.content_purged_at IS NULL
    ORDER BY e.seq
    LIMIT @batch_size::int
    FOR UPDATE OF e SKIP LOCKED
)
UPDATE run_events
SET payload = CASE
        WHEN @strip::jsonb -> run_events.kind IS NOT NULL
        THEN run_events.payload - ARRAY(SELECT jsonb_array_elements_text(@strip::jsonb -> run_events.kind))
        ELSE '{}'::jsonb
    END,
    content_purged_at = @purged_at::timestamptz
FROM batch
WHERE run_events.org_id = batch.org_id AND run_events.id = batch.id;

-- name: MarkRunContentPurged :execrows
-- Marks a finished run as purged once none of its content events carries content, so later passes
-- skip it. A row still locked by another purger leaves the run unmarked; the next pass finishes it.
UPDATE runs
SET content_purged_at = @purged_at::timestamptz
WHERE runs.org_id = @org_id AND runs.id = @run_id
  AND runs.finished_at IS NOT NULL
  AND runs.content_purged_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM run_events e
      WHERE e.org_id = @org_id AND e.run_id = @run_id
        AND e.kind = ANY(@kinds::text[])
        AND e.content_purged_at IS NULL);

-- name: GetRunEventBySource :one
SELECT * FROM run_events
WHERE org_id = @org_id AND run_id = @run_id AND epoch = @epoch AND source_seq = @source_seq::bigint;
