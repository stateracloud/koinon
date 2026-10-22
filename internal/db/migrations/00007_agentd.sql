-- SPDX-License-Identifier: Apache-2.0
--
-- Run driving by koinon-agentd (plan item AG1; ADRs 0029, 0030, 0049).

-- +goose Up

-- The ticket timeline seq through which the ticket's messages were sent to the
-- harness as this run's prompt; NULL until the prompt is sent. The next run of
-- the ticket sends only messages after the highest value of its earlier runs,
-- and a run resumed after a takeover knows it was already prompted.
ALTER TABLE runs ADD COLUMN prompt_through_seq bigint CHECK (prompt_through_seq >= 0);

-- How often another owner took the run over after a lease expired. agentd fails
-- a run that keeps losing its harness instead of resuming it forever.
ALTER TABLE runs ADD COLUMN takeovers integer NOT NULL DEFAULT 0 CHECK (takeovers >= 0);

-- The writer's own sequence number for the event within its epoch (for agentd:
-- the order in which the harness sent it). (run, epoch, source_seq) is unique,
-- so a writer that retries an append after an ambiguous failure does not record
-- the event twice. NULL for writers that do not retry (control-plane notes).
ALTER TABLE run_events ADD COLUMN source_seq bigint CHECK (source_seq >= 1);

CREATE UNIQUE INDEX run_events_source_seq ON run_events (org_id, run_id, epoch, source_seq)
    WHERE source_seq IS NOT NULL;
