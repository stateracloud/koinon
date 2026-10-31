-- SPDX-License-Identifier: Apache-2.0
--
-- A warning before a run's time limit (issue #183; ADR 0066 amendment 2026-10-28). agentd tells the
-- agent that its wall-clock cap is near so it can wrap up; the notice is sent at most once per run.

-- +goose Up

-- Set when the wrap-up notice of a run is no longer armed (ADR 0066 amendment 2026-10-28): it was
-- sent, or it was suppressed because the harness ignored the cancel. Only a sent notice has a
-- run.wrap_up_notice event. At most once per run, across takeovers.
ALTER TABLE runs ADD COLUMN wrap_up_notice_at timestamptz;
