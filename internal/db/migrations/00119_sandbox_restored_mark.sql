-- SPDX-License-Identifier: Apache-2.0
--
-- The restored-home mark (issue #1481, decision 8). After a home restore, the sandbox holds a home that
-- no Substrate snapshot has. The reconciler suspends the idle sandbox to take that snapshot, and until it
-- does, a crash must not be reverted to the older snapshot (it would give an empty home).
--
--   * restored_sandbox_id: the sandbox ID that got the restore. The mark counts only while it equals
--     sandbox_id, so a recreate with no restore does not keep it. NULL: no mark.
--   * restored_checkpoint_id: the checkpoint that was restored. A crash before the seal reverts the
--     sandbox and points restore_checkpoint_id at it again, so the next run restores again.
--
-- agentd sets both when it clears restore_checkpoint_id after a restore. The reconciler clears both
-- when the stop of the sandbox has taken the snapshot.
--
-- Expand only (ADR 0085): two new nullable columns.

-- +goose Up

ALTER TABLE agent_sandboxes
    ADD COLUMN restored_sandbox_id    text,
    ADD COLUMN restored_checkpoint_id uuid;
