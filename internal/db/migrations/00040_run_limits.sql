-- SPDX-License-Identifier: Apache-2.0
--
-- Run limits replace the token cap (issue #243, ADR 0081). A run stops at the first of its time
-- limit, its model-request limit and its spend limit; tokens are reported, not enforced.

-- +goose Up

-- The limits a run was queued with, copied from the agent version (or the agent's override) like
-- max_wall_clock_seconds. NULL on a run queued before this migration: it has no request or spend
-- limit, and its token cap is no longer enforced.
ALTER TABLE runs
    ADD COLUMN max_model_requests bigint CHECK (max_model_requests > 0),
    ADD COLUMN max_spend_usd      numeric(24, 12) CHECK (max_spend_usd > 0);

-- max_tokens is deprecated: kept for the runs that have it, written by no new run, enforced by none.
ALTER TABLE runs ALTER COLUMN max_tokens DROP NOT NULL;

-- An agent's run limits as set by an org admin, the way agent_budgets holds its daily limit. A
-- column that is NULL falls back to the agent version's value (the template's default).
CREATE TABLE agent_run_limits (
    org_id             uuid            NOT NULL,
    agent_id           uuid            NOT NULL,
    max_model_requests bigint          CHECK (max_model_requests > 0),
    max_spend_usd      numeric(24, 12) CHECK (max_spend_usd > 0),
    max_duration_secs  integer         CHECK (max_duration_secs > 0),
    updated_by         text            NOT NULL,
    updated_at         timestamptz     NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, agent_id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id)
);
