-- SPDX-License-Identifier: Apache-2.0
--
-- Continuation runs after a time cap, and a per-task USD cap (issue #99, ADR 0081 amendment
-- 2026-11-12). All additive.

-- +goose Up

-- How many continuation runs the task has been given. A run that hits its wall-clock cap is
-- followed by a new run of the same task while this is below the limit and the task's spend is
-- below its cap.
ALTER TABLE tasks
    ADD COLUMN continuation_count integer NOT NULL DEFAULT 0 CHECK (continuation_count >= 0);

-- The org's default per-task USD cap for continuation runs. NULL is the platform default.
ALTER TABLE org_settings
    ADD COLUMN continuation_task_spend_usd numeric(24, 12) CHECK (continuation_task_spend_usd > 0);

-- The agent's overrides of the template's continuation limit and of the org's per-task cap. NULL
-- follows the template and the org; max_continuations 0 switches continuation off for the agent.
ALTER TABLE agent_run_limits
    ADD COLUMN max_continuations   integer         CHECK (max_continuations >= 0),
    ADD COLUMN task_spend_usd      numeric(24, 12) CHECK (task_spend_usd > 0);
