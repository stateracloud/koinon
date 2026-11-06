-- SPDX-License-Identifier: Apache-2.0
--
-- Follow-ups of checkpoints resync (#89): a repository prefix that already holds another
-- repository (#262), and a checkpoint whose snapshot is missing from the bucket (#263).
--
-- 1. A repository's prefix follows from the agent and its epoch (checkpoint.EpochPrefix). After a
--    database restore, the prefix of the agent's next epoch can already hold a kopia repository
--    started after the dump, whose password only the lost database had.
--    - home_repositories.next_epoch_at_least: the lowest epoch a new repository epoch of the agent
--      may take, raised by koinon-agentd when it finds such a repository at the next prefixes
--      (checkpoints resync, or a backup or seed into a prefix that holds one). keys forget and keys
--      new-repositories start the next epoch at max(current + 1, next_epoch_at_least). NULL: no
--      floor.
--    - prefix_taken_at, on home_repositories (epoch 0) and home_repository_epochs: the row's prefix
--      held a repository its password does not open when koinon-agentd first wrote there. Nothing
--      of the row's is in it, so the sweeper marks such a row deleted without deleting the blobs.
-- 2. home_checkpoints.snapshot_missing_at: checkpoints resync marked the checkpoint failed because
--    its snapshot is not in the bucket. That says nothing about the home, which stays on its
--    volume: the reconciler's recreate gate asks for a new backup instead of blocking the
--    recreation as it does after a backup that failed.
--
-- Expand only (ADR 0085): new nullable columns. An older binary of the line ignores them: it
-- starts the next epoch at current + 1, sweeps a taken prefix and blocks on a missing snapshot, as
-- before.

-- +goose Up

ALTER TABLE home_repositories
    ADD COLUMN next_epoch_at_least integer CHECK (next_epoch_at_least >= 1),
    ADD COLUMN prefix_taken_at timestamptz;

ALTER TABLE home_repository_epochs ADD COLUMN prefix_taken_at timestamptz;

ALTER TABLE home_checkpoints ADD COLUMN snapshot_missing_at timestamptz;
