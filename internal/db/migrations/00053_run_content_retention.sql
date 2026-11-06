-- SPDX-License-Identifier: Apache-2.0
--
-- Retention for stored run content (issue #69, #50). The content of a finished run's events
-- (run_events.payload) is kept for the org's retention period, 30 days by default, and then blanked:
-- the row stays, with its run, seq, epoch, kind and created_at, so the run page keeps its skeleton
-- and its sequence stays gap-free. Traces and the spend ledger are other tables and are not touched.
-- Expand only (ADR 0085): new nullable columns and an index. Only the content kinds (messages,
-- thoughts, tool calls) are blanked; kinds that mix content and state lose only the content keys
-- (run.prompt text, acp.permission toolCall, run.refusal refusals); state, park and cost events keep
-- their payload.

-- +goose Up

-- The org's retention in days. NULL: the platform default (30 days, orgsettings.DefaultRunContentRetention).
ALTER TABLE org_settings
    ADD COLUMN run_content_retention_days integer CHECK (run_content_retention_days BETWEEN 1 AND 365);

-- When the purge job blanked the event's payload; NULL while the content is stored.
ALTER TABLE run_events ADD COLUMN content_purged_at timestamptz;

-- When the purge job blanked the run's content; NULL while the run may still carry content. The
-- purge is driven from runs, so a pass reads only finished runs past the cutoff, never every event.
ALTER TABLE runs ADD COLUMN content_purged_at timestamptz;

-- The runs the purge still has to visit, oldest first. Once a run is purged it leaves the index.
CREATE INDEX runs_content_unpurged ON runs (org_id, finished_at, id)
    WHERE finished_at IS NOT NULL AND content_purged_at IS NULL;
