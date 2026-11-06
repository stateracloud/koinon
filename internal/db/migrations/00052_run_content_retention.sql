-- SPDX-License-Identifier: Apache-2.0
--
-- Retention for stored run content (issue #69, #50). The content of a finished run's events
-- (run_events.payload) is kept for the org's retention period, 30 days by default, and then blanked:
-- the row stays, with its run, seq, epoch, kind and created_at, so the run page keeps its skeleton
-- and its sequence stays gap-free. Traces and the spend ledger are other tables and are not touched.
-- Expand only (ADR 0085): two new nullable columns and an index.

-- +goose Up

-- The org's retention in days. NULL: the platform default (30 days, orgsettings.DefaultRunContentRetention).
ALTER TABLE org_settings
    ADD COLUMN run_content_retention_days integer CHECK (run_content_retention_days BETWEEN 1 AND 3650);

-- When the purge job blanked the event's payload; NULL while the content is stored.
ALTER TABLE run_events ADD COLUMN content_purged_at timestamptz;

-- The purge job looks for events that still carry content. The index holds only those, so it stays
-- small once old runs are purged.
CREATE INDEX run_events_unpurged ON run_events (org_id, run_id) WHERE content_purged_at IS NULL;
