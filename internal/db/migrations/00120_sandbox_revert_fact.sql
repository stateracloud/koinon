-- SPDX-License-Identifier: Apache-2.0
--
-- The revert fact of issue #1526 (follow-up of #1481 decision 7).
--
-- The reconciler reverts a crashed sandbox to its last snapshot. It is not the owner of a run, so it
-- cannot write a run event. It writes this fact on the sandbox row. The run's owner (koinon-agentd)
-- reads it and writes one run event, at its own epoch.
--
--   * revert_count: how many reverts of the agent's sandbox the reconciler did. It only goes up. The
--     owner tells a revert it did not report yet by this count.
--   * reverted_at: when the newest revert was done (the database clock). The owner reports a revert
--     only when it is after the start of the run.
--   * revert_snapshot_at: the snapshot time of the sandbox at that revert (last_snapshot_at then).
--     NULL: the time was not known. The writes after it are lost.
--
-- Expand only (ADR 0085): a new column with a default, and two new nullable columns.

-- +goose Up

ALTER TABLE agent_sandboxes
    ADD COLUMN revert_count       integer NOT NULL DEFAULT 0,
    ADD COLUMN reverted_at        timestamptz,
    ADD COLUMN revert_snapshot_at timestamptz;
