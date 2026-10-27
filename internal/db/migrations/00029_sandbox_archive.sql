-- SPDX-License-Identifier: Apache-2.0
--
-- Archive tier (plan item AR1; ADR 0032 amendment, #146 A7). archived_at is set when the
-- reconciler deletes a long-stopped sandbox and its volume after the home's latest checkpoint
-- completed. The row keeps its restore_checkpoint_id (HC1), so the next sandbox keeps the home
-- generation and the first run restores the home before the harness starts. Any observed
-- starting, ready, stopping, stopped or error sandbox clears it (RecordAgentSandbox).

-- +goose Up

ALTER TABLE agent_sandboxes ADD COLUMN archived_at timestamptz;
