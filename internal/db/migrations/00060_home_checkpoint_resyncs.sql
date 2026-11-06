-- SPDX-License-Identifier: Apache-2.0
--
-- checkpoints resync (ADR 0085 decision 17; #89). After a database restore, home_checkpoints can
-- miss snapshots that were written after the dump and can name snapshots that kopia retention has
-- deleted since. `koinon-api checkpoints resync` asks for each agent's checkpoints to be rebuilt from
-- the bucket, which is the truth for what exists: koinon-agentd, the only holder of the bucket's
-- credentials, lists the snapshots of each of the agent's repositories that still opens, records the
-- ones no checkpoint names and marks failed the complete checkpoints whose snapshot is gone.
--
-- Expand-only (ADR 0085 decisions 5 and 6): one new table; an older binary neither reads nor writes
-- it, and nothing that exists changes.
--
-- One row per agent: the latest request. request_id is new on every request, so a try of an
-- earlier request, still running when the next one came, records nothing (with attempts, the claim
-- token, as seed_attempts is a seed's). resync_after is when koinon-agentd may next try it (a claim
-- moves it forward by its lease). finished_at: done, with the counts of the try that did it;
-- failed_at: given up after the tries, with the last error.

-- +goose Up

CREATE TABLE home_checkpoint_resyncs (
    org_id       uuid        NOT NULL,
    agent_id     uuid        NOT NULL,
    request_id   uuid        NOT NULL DEFAULT gen_random_uuid(),
    requested_at timestamptz NOT NULL DEFAULT now(),
    resync_after timestamptz NOT NULL DEFAULT now(),
    attempts     integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    error        text,
    finished_at  timestamptz,
    failed_at    timestamptz,
    -- What the try that finished found: the complete snapshots listed in the agent's repositories,
    -- the checkpoints it added for snapshots no checkpoint named, the complete checkpoints it marked
    -- failed because their snapshot is gone, and the snapshots it left out: a backup is still
    -- recording one, the attempt that wrote one was recorded as failed, or its tags are unreadable.
    snapshots    integer,
    added        integer,
    missing      integer,
    skipped      integer,
    PRIMARY KEY (org_id, agent_id),
    FOREIGN KEY (org_id, agent_id) REFERENCES home_repositories (org_id, agent_id),
    CONSTRAINT home_checkpoint_resyncs_outcome CHECK (
        (finished_at IS NULL OR failed_at IS NULL)
        AND ((finished_at IS NOT NULL) = (snapshots IS NOT NULL AND added IS NOT NULL AND missing IS NOT NULL AND skipped IS NOT NULL)))
);

-- The resyncs koinon-agentd still has to do.
CREATE INDEX home_checkpoint_resyncs_due ON home_checkpoint_resyncs (resync_after)
    WHERE finished_at IS NULL AND failed_at IS NULL;
