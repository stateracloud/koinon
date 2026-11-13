-- SPDX-License-Identifier: Apache-2.0
--
-- Per-org sandbox creation rate limit (issue #27, ADR 0066 amendment 2026-11-12).
--   * org_settings.sandbox_creations_per_minute is the most sandbox creations (a new sandbox or a
--     start of a stopped one) the reconciler makes for the org in a sliding minute. NULL is the
--     platform default (10).
--   * sandbox_creations is the ledger the limit counts: one row per creation, written by the
--     reconciler in the same transaction that checks the count. Rows older than an hour are pruned
--     by the writer. A creation held back by the limit is not a failure: the run stays queued or
--     starting and the reconciler retries on its next pass.

-- +goose Up

ALTER TABLE org_settings
    ADD COLUMN sandbox_creations_per_minute integer
        CHECK (sandbox_creations_per_minute BETWEEN 1 AND 10000);

CREATE TABLE sandbox_creations (
    org_id     uuid        NOT NULL,
    id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    agent_id   uuid        NOT NULL,
    kind       text        NOT NULL CHECK (kind IN ('create', 'start')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id) ON DELETE CASCADE
);

CREATE INDEX sandbox_creations_org_time_idx ON sandbox_creations (org_id, created_at DESC);
