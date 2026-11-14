-- SPDX-License-Identifier: Apache-2.0
--
-- One queued run per task (issue 678). A queued run of a task sees every message appended before
-- it starts, so a second one is waste at best. Until now the rule held only because every enqueue
-- path holds the task row lock; RequeueRun (starting -> queued) could break it without a new
-- caller. This index makes the database refuse a second queued run of one task.
--
-- Rows that already break the rule are repaired first. They are possible: RequeueRun could put a
-- starting run back in the queue next to a queued run of the same task. The oldest queued run of a
-- task stays, the others are canceled, and each cancel gets the run event and the task event that
-- the Go path (runoutcome.recordCanceled) writes for every cancel (kind run.canceled, same payload
-- keys), so the timelines say why the run ended. A queued run has no owner and no lease, so the
-- cancel needs no other column.
--
-- Expand only (ADR 0085): one new index.
-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    dup record;
    run_seq bigint;
    run_epoch bigint;
    task_seq bigint;
    payload jsonb := jsonb_build_object(
        'reason', 'superseded',
        'message', 'superseded by an older queued run of the task');
BEGIN
    FOR dup IN
        SELECT r.org_id, r.id, r.task_id FROM runs r
        WHERE r.state = 'queued'
          AND EXISTS (SELECT 1 FROM runs o
                      WHERE o.org_id = r.org_id AND o.task_id = r.task_id AND o.state = 'queued'
                        AND (o.created_at, o.id) < (r.created_at, r.id))
        ORDER BY r.created_at, r.id
    LOOP
        UPDATE runs
        SET state = 'canceled',
            outcome_reason = 'superseded by an older queued run of the task',
            finished_at = now(),
            ready_at = NULL,
            last_event_seq = last_event_seq + 1
        WHERE org_id = dup.org_id AND id = dup.id
        RETURNING last_event_seq, epoch INTO run_seq, run_epoch;
        INSERT INTO run_events (org_id, run_id, seq, epoch, kind, payload)
        VALUES (dup.org_id, dup.id, run_seq, run_epoch, 'run.canceled', payload);
        UPDATE tasks SET last_event_seq = last_event_seq + 1
        WHERE org_id = dup.org_id AND id = dup.task_id
        RETURNING last_event_seq INTO task_seq;
        INSERT INTO task_events (org_id, task_id, seq, kind, actor_kind, run_id, payload)
        VALUES (dup.org_id, dup.task_id, task_seq, 'run.canceled', 'system', dup.id, payload);
    END LOOP;
END
$$;
-- +goose StatementEnd

CREATE UNIQUE INDEX runs_one_queued_per_task ON runs (org_id, task_id)
    WHERE state = 'queued';
