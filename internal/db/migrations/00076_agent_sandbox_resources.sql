-- SPDX-License-Identifier: Apache-2.0
--
-- A per-agent sandbox resource override (#75, ADR 0083 amendment 2026-11-11).
--
-- A template may size its sandbox and the platform sets the default and the bounds (ADR 0083). An
-- org admin can now override the four values for one agent. A NULL column means "follow the
-- template, then the platform default"; a value wins over the template's. The API refuses a
-- value outside the platform bounds (the bounds come from the chart, so the database only checks
-- the shape) and the reconciler clamps again, because the bounds may change after the write.
--
-- Expand-only: four nullable columns on agents, which is keyed by (org_id, id) already.

-- +goose Up

ALTER TABLE agents
    ADD COLUMN sandbox_cpu_request    text CHECK (sandbox_cpu_request    IS NULL OR length(sandbox_cpu_request)    BETWEEN 1 AND 32),
    ADD COLUMN sandbox_cpu_limit      text CHECK (sandbox_cpu_limit      IS NULL OR length(sandbox_cpu_limit)      BETWEEN 1 AND 32),
    ADD COLUMN sandbox_memory_request text CHECK (sandbox_memory_request IS NULL OR length(sandbox_memory_request) BETWEEN 1 AND 32),
    ADD COLUMN sandbox_memory_limit   text CHECK (sandbox_memory_limit   IS NULL OR length(sandbox_memory_limit)   BETWEEN 1 AND 32);
