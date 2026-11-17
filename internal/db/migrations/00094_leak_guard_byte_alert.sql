-- SPDX-License-Identifier: Apache-2.0
--
-- Leak guard: the byte cap alerts and never refuses (issue #1058, ADR 0091 amendment 2026-11-17).
--
--   * leak_guard_decisions gets the bytes the guard charged to the run for the call, the run's count
--     before the call (NULL when the guard did not read the count), the destination host and a mark on the one call that took the run over the
--     threshold. The lab then reads the cap from the rows and no longer rebuilds it from rows plus
--     cassettes. A host is a fetch host (an http or https address argument), never a path or a query.
--   * leak_guard_runs gets the one over-threshold event of the run: when it happened and the count
--     it reached. NULL while the run is under the threshold.
--
-- Only new columns with defaults: an expand migration, legal on a patch line (ADR 0085).

-- +goose Up

ALTER TABLE leak_guard_decisions
    ADD COLUMN charged_bytes   integer NOT NULL DEFAULT 0 CHECK (charged_bytes >= 0),
    ADD COLUMN cap_used_before integer CHECK (cap_used_before IS NULL OR cap_used_before >= 0),
    ADD COLUMN dest_host       text    NOT NULL DEFAULT '',
    ADD COLUMN over_threshold  boolean NOT NULL DEFAULT false;

ALTER TABLE leak_guard_runs
    ADD COLUMN over_threshold_at    timestamptz,
    ADD COLUMN over_threshold_bytes integer CHECK (over_threshold_bytes IS NULL OR over_threshold_bytes >= 0);
