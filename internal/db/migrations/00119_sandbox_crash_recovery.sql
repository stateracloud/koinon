-- SPDX-License-Identifier: Apache-2.0
--
-- The crash path of issue #1481: the restored-home mark and the time of the last snapshot.
--
-- The restored-home mark. After a home restore, the sandbox holds a home that no Substrate snapshot
-- has. The reconciler suspends the idle sandbox to take that snapshot, and until it does, a crash must
-- not be reverted to an older snapshot without the restore being applied again.
--
--   * restored_sandbox_id: the sandbox ID that got the restore. The mark counts only while it equals
--     sandbox_id, so a recreate with no restore does not keep it. NULL: no mark.
--   * restored_checkpoint_id: a checkpoint that was restored. It is a flag with a usable ID: a restore
--     takes the newest complete checkpoint of the home, which may be a newer one than this.
--   * restored_at: when the restore ended (the database clock). A snapshot counts as holding the
--     restored home only when last_snapshot_at is after it.
--
-- agentd sets the three when it clears restore_checkpoint_id after a restore. The reconciler clears
-- them when a stop of the sandbox has taken the snapshot.
--
-- last_snapshot_at: when the newest snapshot of the sandbox_id was taken, as Koinon knows it (the
-- Substrate API gives no snapshot time): the database clock read just before the call that took it, a
-- lower bound. It is never a clock of another host, so it compares with restored_at and the checkpoint
-- times. NULL: unknown. After a crash, the newer of this snapshot and the
-- newest complete home checkpoint is used; an unknown time loses to the checkpoint. It belongs to one
-- sandbox_id: RecordAgentSandbox resets it for another sandbox.
--
-- Expand only (ADR 0085): new nullable columns.

-- +goose Up

ALTER TABLE agent_sandboxes
    ADD COLUMN restored_sandbox_id    text,
    ADD COLUMN restored_checkpoint_id uuid,
    ADD COLUMN restored_at            timestamptz,
    ADD COLUMN last_snapshot_at       timestamptz;
