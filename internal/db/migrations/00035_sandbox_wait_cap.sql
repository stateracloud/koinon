-- SPDX-License-Identifier: Apache-2.0
--
-- A cap on the total time a run waits for its sandbox (issue #160; ADR 0066 amendment 2026-10-28).
-- The LT1 requeue was unbounded: a sandbox that never becomes ready (a missing image, an admission
-- refusal, no node that fits) kept the run in the queue for ever. The run now fails with the class
-- sandbox_unavailable once the org's cap is reached, naming the cause from the sandbox's condition.

-- +goose Up

-- A run that waits for a sandbox that never becomes ready fails with this class (ADR 0066 amendment
-- 2026-10-28). Not retried.
ALTER TYPE run_failure_class ADD VALUE 'sandbox_unavailable';

-- The org's cap on the total time a run waits for its sandbox, across requeues. NULL: the platform
-- default (1 hour, agentd --sandbox-wait-cap).
ALTER TABLE org_settings
    ADD COLUMN sandbox_wait_cap_seconds integer CHECK (sandbox_wait_cap_seconds BETWEEN 300 AND 604800);

-- When the run first found its sandbox not ready in the current wait. Kept across requeues (the
-- queue time between claims counts). Cleared for every run of the agent when any of its runs finds
-- the sandbox ready, and when an admin resumes the agent (ClearSandboxWaitForAgent): a sandbox that
-- was ready since is not one that "never became ready", so time a run spent queued behind a sibling
-- that had the sandbox does not count. Set by agentd's clock.
ALTER TABLE runs ADD COLUMN sandbox_wait_started_at timestamptz;
-- ClearSandboxWaitForAgent runs on every run start; this keeps it to the few waiting rows.
CREATE INDEX runs_sandbox_waiting ON runs (org_id, agent_id) WHERE sandbox_wait_started_at IS NOT NULL;

-- The reason of the latest condition that is not true (a Kubernetes-style reason such as
-- ErrImagePull or Unschedulable), next to its message. agentd uses both to name why a sandbox never
-- became ready.
ALTER TABLE agent_sandboxes ADD COLUMN reason text NOT NULL DEFAULT '';
