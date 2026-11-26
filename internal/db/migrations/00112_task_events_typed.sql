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
-- Expand only (ADR 0085): two new columns with defaults and a backfill; nothing is renamed or
-- dropped. An old binary does not know the columns: its rows get version 0 and no prompt mark.

-- +goose Up

ALTER TABLE task_events
    ADD COLUMN payload_version smallint NOT NULL DEFAULT 0 CHECK (payload_version >= 0),
    ADD COLUMN prompt boolean NOT NULL DEFAULT false;

UPDATE task_events SET prompt = true
WHERE kind = 'message'
  AND (actor_kind = 'user' OR (actor_kind = 'system' AND payload ->> 'source' IS NOT NULL));
