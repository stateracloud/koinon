-- SPDX-License-Identifier: Apache-2.0
--
-- A run that repeats itself is stopped with its own failure class (issue #119; ADR 0030 amendment
-- 2026-11-11). agentd reads the harness's ACP stream, nudges the agent once and, if the pattern
-- continues, fails the run with this class instead of letting a cap file it as `budget`.

-- +goose Up

-- Not retried. Expand-only: the old binaries never write or switch on it (ADR 0085).
ALTER TYPE run_failure_class ADD VALUE 'loop';

