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
