-- SPDX-License-Identifier: Apache-2.0
--
-- Indexes for the org lists of koinon-agentd's checkpoint, seed, resync and sweep ticks (#576).
--
-- Each tick used to walk every live org, whatever it held (as the run claim loop did before #378).
-- ListOrgIDsWithCheckpointWork, ListOrgIDsWithSeedWork, ListOrgIDsWithResyncWork and
-- ListOrgIDsWithRepositoryDeleteWork now name only the orgs with work, and these partial indexes
-- keep each of them to the rows that count. Expand only: indexes, nothing renamed or dropped.
-- +goose Up

-- The sandboxes the hourly request considers: ready, healthy, no restore pending. An org with
-- none has nothing to request, and the planner reads only these rows.
CREATE INDEX agent_sandboxes_checkpointable ON agent_sandboxes (org_id)
    WHERE state = 'ready' AND unhealthy_at IS NULL AND restore_checkpoint_id IS NULL;

-- The seeds the seed gauge counts (pending and given up on): a seed that is not yet due still
-- counts, so the tick lists its org.
CREATE INDEX home_repository_epochs_seed_open ON home_repository_epochs (org_id)
    WHERE seed_from_epoch IS NOT NULL AND seeded_at IS NULL AND retired_at IS NULL
      AND credential_lost_at IS NULL;

-- The resyncs the resync gauge counts (pending and given up on).
CREATE INDEX home_checkpoint_resyncs_open ON home_checkpoint_resyncs (org_id)
    WHERE finished_at IS NULL;
