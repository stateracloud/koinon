-- SPDX-License-Identifier: Apache-2.0
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateTask :one
INSERT INTO tasks (org_id, title, agent_id, created_by_kind, created_by_user_id,
                   created_by_agent_id, created_by_trigger_id,
                   notify_kind, notify_user_id, notify_role)
VALUES (@org_id, @title, sqlc.narg(agent_id), @created_by_kind,
        sqlc.narg(created_by_user_id), sqlc.narg(created_by_agent_id),
        sqlc.narg(created_by_trigger_id),
        sqlc.narg(notify_kind), sqlc.narg(notify_user_id), sqlc.narg(notify_role))
RETURNING *;

-- name: GetTask :one
SELECT * FROM tasks
WHERE org_id = @org_id AND id = @id;

-- name: GetTaskForUpdate :one
-- Like GetTask, and locks the row until the transaction ends, so a read-then-change of the state
-- cannot interleave with another one.
SELECT * FROM tasks
WHERE org_id = @org_id AND id = @id
FOR UPDATE;

-- name: TaskLockHeld :one
-- Whether the caller is in a transaction that holds a row lock (a locked row gives the
-- transaction an id; outside a transaction the lock ended with the statement). The scheduler
-- asserts it after GetTaskForUpdate. Org-scoped through the task it names.
SELECT EXISTS (SELECT 1 FROM tasks WHERE org_id = @org_id AND id = @id)
       AND pg_current_xact_id_if_assigned() IS NOT NULL;

-- name: ListTasks :many
SELECT * FROM tasks
WHERE org_id = @org_id
ORDER BY created_at, id;

-- name: ListTasksByStates :many
-- The tasks in any of the states, oldest first (a board column of the ticket API is a set of
-- states).
SELECT * FROM tasks
WHERE org_id = @org_id AND state = ANY((@states::text[])::task_state[])
ORDER BY created_at, id;

-- name: SetTaskState :one
-- Sets a task's state. Callers moving a task to an end state also cancel its queued runs
-- (CancelQueuedRunsForTask) in the same transaction.
UPDATE tasks
SET state = @state::task_state,
    finished_at = CASE WHEN @state::task_state IN ('completed', 'canceled', 'failed')
                       THEN coalesce(finished_at, now()) END
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: SetTaskHarnessSession :one
-- Stores the harness session for the task with the home generation of the
-- sandbox it was created in. Fenced by the run epoch: only the current owner
-- of one of the task's active runs can set it.
UPDATE tasks t
SET harness_session_id = @harness_session_id::text, harness_session_cwd = @harness_session_cwd::text,
    harness_session_generation = sqlc.narg(harness_session_generation),
    session_restart_pending = false
WHERE t.org_id = @org_id AND t.id = @id
  AND EXISTS (
      SELECT 1 FROM runs r
      WHERE r.org_id = @org_id AND r.task_id = t.id AND r.id = @run_id
        AND r.epoch = @epoch AND r.state IN ('starting', 'running'))
RETURNING t.*;

-- name: AppendTaskEvent :one
WITH task AS (
    UPDATE tasks
    SET last_event_seq = last_event_seq + 1
    WHERE tasks.org_id = @org_id AND tasks.id = @task_id
    RETURNING tasks.org_id, tasks.id, tasks.last_event_seq
)
INSERT INTO task_events (org_id, task_id, seq, kind, actor_kind, actor_user_id,
                           actor_agent_id, run_id, payload)
SELECT task.org_id, task.id, task.last_event_seq, @kind, @actor_kind,
       sqlc.narg(actor_user_id), sqlc.narg(actor_agent_id), sqlc.narg(run_id), @payload
FROM task
RETURNING *;

-- name: ListTaskEvents :many
-- The timeline after a sequence number (0 for the start), in order.
SELECT * FROM task_events
WHERE org_id = @org_id AND task_id = @task_id AND seq > @after_seq
ORDER BY seq
LIMIT @max_events;

-- name: ListTaskMessagesAfter :many
-- The users' messages on the task after a timeline seq, in order: what
-- agentd sends the harness as a run's prompt. A trigger's instruction (a system message whose
-- payload carries its source) is part of it, so a scheduled run sees what it was scheduled to do.
SELECT * FROM task_events
WHERE org_id = @org_id AND task_id = @task_id AND seq > @after_seq
  AND kind = 'message'
  AND (actor_kind = 'user' OR (actor_kind = 'system' AND payload ->> 'source' IS NOT NULL))
ORDER BY seq;

-- name: LatestUserMessageActor :one
-- The person who wrote the task's latest user message (the person on the other end of a chat thread).
SELECT actor_user_id FROM task_events
WHERE org_id = @org_id AND task_id = @task_id AND kind = 'message' AND actor_kind = 'user'
  AND actor_user_id IS NOT NULL
ORDER BY seq DESC
LIMIT 1;

-- name: SetTaskStateFrom :one
-- Moves a task between two non-end states, only if it is in one of from_states: parking on a human
-- moves submitted or working to input_required, and a resolved approval moves it back. No row: the
-- task is elsewhere (for example in an end state, or a person moved it).
UPDATE tasks
SET state = @to_state::task_state
WHERE org_id = @org_id AND id = @id AND state = ANY((@from_states::text[])::task_state[])
  AND @to_state::task_state NOT IN ('completed', 'canceled', 'failed')
RETURNING *;

-- name: EndTaskFrom :one
-- Ends a task with its run's outcome (completed, or failed), only if it is in one of from_states and
-- no run of it is queued (a follow-up message already asked for another turn). No row: the task is
-- elsewhere (canceled, ended by a person) or has more work queued.
UPDATE tasks
SET state = @to_state::task_state,
    finished_at = coalesce(finished_at, now())
WHERE tasks.org_id = @org_id AND tasks.id = @id AND tasks.state = ANY((@from_states::text[])::task_state[])
  AND @to_state::task_state IN ('completed', 'failed')
  AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.org_id = tasks.org_id AND runs.task_id = tasks.id AND runs.state = 'queued')
RETURNING *;

-- name: BumpTaskContinuation :one
-- Counts one more continuation run on a task that is not ended, only while the count is below
-- max (issue #99). No row: the limit is reached or the task has ended.
UPDATE tasks
SET continuation_count = continuation_count + 1
WHERE org_id = @org_id AND id = @id AND continuation_count < @max::int
  AND state NOT IN ('completed', 'canceled', 'failed')
RETURNING *;
