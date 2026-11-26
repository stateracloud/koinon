-- SPDX-License-Identifier: Apache-2.0
--
-- Runs of an org by creation time (issue #1268, ADR 0126 section 4).
--
-- The usage and cost read model counts the runs of an org that were created today and how many of
-- them failed. Without this index the count reads every run of the org. The platform total reads
-- it once per org. Expand only: one new index.

-- +goose Up

CREATE INDEX runs_org_created ON runs (org_id, created_at DESC);
