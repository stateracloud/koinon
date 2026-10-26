-- SPDX-License-Identifier: Apache-2.0
--
-- Tasks replace tickets (plan item TK1; ADR 0065). The work record is an A2A-shaped task with the
-- states submitted, working, input_required, completed, canceled and failed. Every ticket keeps
-- its id, its timeline and everything that points at it (runs, approvals, spend, saved work,
-- incidents, the harness session and the agent's summary): the tables and columns are renamed in
-- place and the board column becomes a state.
--
-- Board column to state (recorded in ADR 0065):
--   todo           -> submitted
--   in_progress    -> working
--   waiting_on_you -> input_required
--   done           -> completed

-- +goose Up

CREATE TYPE task_state AS ENUM (
    'submitted', 'working', 'input_required', 'completed', 'canceled', 'failed'
);

-- Who created the task: a person, a trigger (ADR 0066) or an agent (ask_agent, ADR 0065). The
-- trigger and agent that did it are columns of their own; triggers get their table with TR1, which
-- adds the foreign key.
CREATE TYPE task_creator_kind AS ENUM ('user', 'trigger', 'agent');

ALTER TABLE tickets RENAME TO tasks;
ALTER TABLE ticket_events RENAME TO task_events;
ALTER TYPE ticket_actor_kind RENAME TO task_actor_kind;

-- Board column -> state. Dropping status drops the (status = 'done') = (done_at IS NOT NULL) check
-- and tickets_org_id_status with it.
ALTER TABLE tasks ADD COLUMN state task_state;
UPDATE tasks SET state = CASE status
    WHEN 'todo' THEN 'submitted'::task_state
    WHEN 'in_progress' THEN 'working'::task_state
    WHEN 'waiting_on_you' THEN 'input_required'::task_state
    WHEN 'done' THEN 'completed'::task_state
END;
ALTER TABLE tasks
    ALTER COLUMN state SET NOT NULL,
    ALTER COLUMN state SET DEFAULT 'submitted';
ALTER TABLE tasks DROP COLUMN status;
DROP TYPE ticket_status;

-- done_at becomes finished_at: set in every end state, not only when a person closed the ticket.
ALTER TABLE tasks RENAME COLUMN done_at TO finished_at;
ALTER TABLE tasks ADD CONSTRAINT tasks_finished_at
    CHECK ((state IN ('completed', 'canceled', 'failed')) = (finished_at IS NOT NULL));

-- Creator as an actor. Existing tickets were all created through the API by a person (the user may
-- be NULL if that user is gone).
ALTER TABLE tasks RENAME COLUMN created_by TO created_by_user_id;
ALTER TABLE tasks
    ADD COLUMN created_by_kind       task_creator_kind NOT NULL DEFAULT 'user',
    ADD COLUMN created_by_agent_id   uuid,
    ADD COLUMN created_by_trigger_id uuid;
ALTER TABLE tasks
    ALTER COLUMN created_by_kind DROP DEFAULT,
    ADD CONSTRAINT tasks_created_by_agent
        FOREIGN KEY (org_id, created_by_agent_id) REFERENCES agents (org_id, id),
    ADD CONSTRAINT tasks_created_by_actor CHECK (
        (created_by_kind = 'user' OR created_by_user_id IS NULL)
        AND (created_by_agent_id IS NOT NULL) = (created_by_kind = 'agent')
        AND (created_by_trigger_id IS NOT NULL) = (created_by_kind = 'trigger'));

-- A task is owned by one agent (ADR 0065). Tickets could be unassigned in the schema, though the
-- API never made one; NOT VALID enforces the rule for every new and changed row and leaves any
-- such old row alone.
ALTER TABLE tasks ADD CONSTRAINT tasks_owned_by_agent CHECK (agent_id IS NOT NULL) NOT VALID;

CREATE INDEX tasks_org_id_state ON tasks (org_id, state, created_at);

-- Everything that pointed at a ticket points at a task.
ALTER TABLE task_events RENAME COLUMN ticket_id TO task_id;
ALTER TABLE runs RENAME COLUMN ticket_id TO task_id;
ALTER TABLE approval_requests RENAME COLUMN ticket_id TO task_id;
ALTER TABLE spend_entries RENAME COLUMN ticket_id TO task_id;
ALTER TABLE saved_work RENAME COLUMN ticket_id TO task_id;
ALTER TABLE sandbox_incidents RENAME COLUMN ticket_id TO task_id;

-- The timeline: the ticket's board moves become state changes.
UPDATE task_events SET kind = 'task.created' WHERE kind = 'ticket.created';
UPDATE task_events e
SET payload = (e.payload - 'from' - 'to') || jsonb_build_object(
    'from', (SELECT m.state FROM (VALUES
        ('todo', 'submitted'), ('in_progress', 'working'),
        ('waiting_on_you', 'input_required'), ('done', 'completed')) AS m(col, state)
        WHERE m.col = e.payload ->> 'from'),
    'to', (SELECT m.state FROM (VALUES
        ('todo', 'submitted'), ('in_progress', 'working'),
        ('waiting_on_you', 'input_required'), ('done', 'completed')) AS m(col, state)
        WHERE m.col = e.payload ->> 'to'))
WHERE e.kind = 'status.changed'
  AND e.payload ->> 'from' IN ('todo', 'in_progress', 'waiting_on_you', 'done')
  AND e.payload ->> 'to' IN ('todo', 'in_progress', 'waiting_on_you', 'done');

-- Constraint, index and trigger names follow their tables.
-- +goose StatementBegin
DO $$
DECLARE r record;
BEGIN
    FOR r IN SELECT c.conrelid::regclass::text AS tbl, c.conname
             FROM pg_constraint c
             WHERE c.connamespace = current_schema()::regnamespace AND c.conname LIKE '%ticket%'
    LOOP
        EXECUTE format('ALTER TABLE %s RENAME CONSTRAINT %I TO %I', r.tbl, r.conname,
                       replace(replace(r.conname, 'tickets', 'tasks'), 'ticket', 'task'));
    END LOOP;
    FOR r IN SELECT indexname FROM pg_indexes
             WHERE schemaname = current_schema() AND indexname LIKE '%ticket%'
    LOOP
        EXECUTE format('ALTER INDEX %I RENAME TO %I', r.indexname,
                       replace(replace(r.indexname, 'tickets', 'tasks'), 'ticket', 'task'));
    END LOOP;
    FOR r IN SELECT t.tgrelid::regclass::text AS tbl, t.tgname
             FROM pg_trigger t
             WHERE NOT t.tgisinternal AND t.tgname LIKE '%ticket%'
    LOOP
        EXECUTE format('ALTER TRIGGER %I ON %s RENAME TO %I', r.tgname, r.tbl,
                       replace(replace(r.tgname, 'tickets', 'tasks'), 'ticket', 'task'));
    END LOOP;
END
$$;
-- +goose StatementEnd

-- run_claimable (00017) names runs.ticket_id in its body, which Postgres reads at call time.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION run_claimable(p_org_id uuid, p_run_id uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1
        FROM runs r
        JOIN agents a ON a.org_id = r.org_id AND a.id = r.agent_id
        WHERE r.org_id = p_org_id AND r.id = p_run_id
          AND r.state IN ('queued', 'parked')
          AND r.ready_at IS NOT NULL
          AND a.status = 'active'
          AND a.unhealthy_at IS NULL
          AND NOT EXISTS (
              SELECT 1 FROM runs act
              WHERE act.org_id = r.org_id AND act.agent_id = r.agent_id
                AND act.state IN ('starting', 'running'))
          -- One harness session per task (ADR 0029): a task's next run waits while another
          -- run of the same task is owned or parked.
          AND NOT EXISTS (
              SELECT 1 FROM runs sib
              WHERE sib.org_id = r.org_id AND sib.task_id = r.task_id AND sib.id <> r.id
                AND sib.state IN ('starting', 'running', 'parked'))
    )
$$;
-- +goose StatementEnd
