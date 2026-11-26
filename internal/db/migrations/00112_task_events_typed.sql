-- SPDX-License-Identifier: Apache-2.0
--
-- Typed Timeline events (issue #735, Task module ticket 4).
--
-- payload_version says how to read payload. Version 0 is the free-form JSON that every row had
-- before this migration; the reader in internal/tasks translates it on read, so old rows are kept
-- as they are. Version 1 is the protojson form of one koinon.task.v1alpha1.TimelineEvent.
--
-- prompt marks an event that agentd sends to the harness as part of a run's prompt. Before this
-- column the prompt query found those events by their kind, actor and payload (a user's message,
-- or a system message whose payload names its source). The update below gives the old rows the
-- same mark, so a prompt reads the same events as before.
--
-- Expand only (ADR 0085): two new columns with defaults, a backfill and a trigger; nothing is
-- renamed or dropped. An old binary does not know the columns: its rows get version 0, and the
-- trigger gives its prompt messages the mark with the old rule. Without the trigger, a message that
-- an old pod writes during a rolling upgrade, or after an app-only rollback, would not be in the
-- next run's prompt. The new writers set the mark themselves; for them the trigger changes nothing.
-- The contract step drops the trigger when no binary without the column can run (issue #1393).

-- +goose Up

ALTER TABLE task_events
    ADD COLUMN payload_version smallint NOT NULL DEFAULT 0 CHECK (payload_version >= 0),
    ADD COLUMN prompt boolean NOT NULL DEFAULT false;

UPDATE task_events SET prompt = true
WHERE kind = 'message'
  AND (actor_kind = 'user' OR (actor_kind = 'system' AND payload ->> 'source' IS NOT NULL));

-- +goose StatementBegin
CREATE FUNCTION task_events_prompt_by_old_rule() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.prompt := NEW.prompt OR (NEW.kind = 'message'
        AND (NEW.actor_kind = 'user' OR (NEW.actor_kind = 'system' AND NEW.payload ->> 'source' IS NOT NULL)));
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER task_events_prompt_by_old_rule BEFORE INSERT ON task_events
    FOR EACH ROW EXECUTE FUNCTION task_events_prompt_by_old_rule();
