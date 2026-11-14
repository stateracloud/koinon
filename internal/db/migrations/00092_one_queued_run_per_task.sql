-- SPDX-License-Identifier: Apache-2.0
--
-- One queued run per task (issue 678). A queued run of a task sees every message appended before
-- it starts, so a second one is waste at best. Until now the rule held only because every enqueue
-- path holds the task row lock; RequeueRun (starting -> queued) could break it without a new
-- caller. This index makes the database refuse a second queued run of one task.
--
-- Rows that already break the rule (none are expected) are repaired first: the oldest queued run
-- of a task stays, the others are canceled. A queued run has no owner and no lease, so the cancel
-- needs no other column.
--
-- Expand only (ADR 0085): one new index.
-- +goose Up

UPDATE runs r
SET state = 'canceled',
    outcome_reason = 'superseded by an older queued run of the task',
    finished_at = now(),
    ready_at = NULL
WHERE r.state = 'queued'
  AND EXISTS (SELECT 1 FROM runs o
              WHERE o.org_id = r.org_id AND o.task_id = r.task_id AND o.state = 'queued'
                AND (o.created_at, o.id) < (r.created_at, r.id));

CREATE UNIQUE INDEX runs_one_queued_per_task ON runs (org_id, task_id)
    WHERE state = 'queued';
