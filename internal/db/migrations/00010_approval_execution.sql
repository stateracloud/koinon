-- SPDX-License-Identifier: Apache-2.0
--
-- Deciding, executing and delivering ask-tier approval requests (plan item
-- P1; ADRs 0008, 0042, 0049).

-- +goose Up

-- Lifecycle (ADR 0042): pending -> approved -> executed | failed, or
-- pending -> denied | expired | canceled. The Trusted preset records a call as
-- approved at once (decision 'allow').
ALTER TABLE approval_requests
    -- How often an executor started the call. Execution is idempotent (the
    -- request ID is the idempotency key, and a pre-create lookup finds what
    -- an earlier attempt created), so a crash between the GitHub calls and
    -- the result being recorded is retried safely; the count makes retries
    -- visible.
    ADD COLUMN executions          integer     NOT NULL DEFAULT 0 CHECK (executions >= 0),
    -- An executor holds the request until this time; another executor takes
    -- it over afterwards. NULL when nobody executes it.
    ADD COLUMN execution_lease_until timestamptz,
    ADD COLUMN executed_at         timestamptz,
    -- Why execution failed (status 'failed'); never contains credentials.
    ADD COLUMN error               text,
    -- When the outcome (executed, failed, denied, expired, canceled) was sent
    -- to the agent as the prompt of its resumed turn. NULL until then.
    ADD COLUMN delivered_at        timestamptz,
    ADD CONSTRAINT approval_requests_decided
        CHECK (status IN ('pending', 'expired', 'canceled') OR decision = 'allow' OR decided_by IS NOT NULL),
    ADD CONSTRAINT approval_requests_delivered_terminal
        CHECK (delivered_at IS NULL OR status IN ('executed', 'failed', 'denied', 'expired', 'canceled'));

-- Approved requests waiting for an executor, oldest first.
CREATE INDEX approval_requests_approved ON approval_requests (org_id, updated_at)
    WHERE status = 'approved';

-- A run's requests whose outcome the agent has not seen yet: they keep the
-- run parked (pending, approved) or go into its next prompt (the rest).
CREATE INDEX approval_requests_undelivered ON approval_requests (org_id, run_id)
    WHERE delivered_at IS NULL;
