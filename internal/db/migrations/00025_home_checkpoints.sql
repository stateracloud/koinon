-- SPDX-License-Identifier: Apache-2.0
--
-- Home checkpoints (plan item HC1; ADRs 0032, 0051, 0054; practice B3): the agent's home is
-- snapshotted to object storage with kopia, hourly while the sandbox runs and before it is stopped
-- or recreated, and restored into the recreated sandbox before the harness starts.

-- +goose Up

-- One kopia repository per agent. Its random password is sealed with the secret store (envelope
-- bound to this row), opened only by koinon-agentd's writer, and never reaches a sandbox.
CREATE TABLE home_repositories (
    org_id            uuid        NOT NULL,
    agent_id          uuid        NOT NULL,
    -- The storage prefix, orgs/<org>/homes/<agent>/, fixed when the repository is created.
    prefix            text        NOT NULL CHECK (prefix ~ '^orgs/[0-9a-f-]{36}/homes/[0-9a-f-]{36}/$'),
    password_envelope jsonb       NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, agent_id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id)
);

CREATE TYPE checkpoint_status AS ENUM ('requested', 'running', 'complete', 'failed');
-- hourly: while running; stop: before an idle stop; recreate: before the sandbox is deleted for a
-- changed spec, a stuck or failed sandbox, or an archive.
CREATE TYPE checkpoint_reason AS ENUM ('hourly', 'stop', 'recreate');

CREATE TABLE home_checkpoints (
    org_id       uuid              NOT NULL,
    id           uuid              NOT NULL DEFAULT uuidv7(),
    agent_id     uuid              NOT NULL,
    reason       checkpoint_reason NOT NULL,
    status       checkpoint_status NOT NULL DEFAULT 'requested',
    -- The OpenShell sandbox read, and the home generation (the lineage, ADR 0051) it belongs to.
    sandbox_id   text              NOT NULL DEFAULT '',
    generation   text              NOT NULL DEFAULT '',
    -- The agent's highest run ownership epoch when the checkpoint was taken (ADR 0030).
    epoch        bigint            NOT NULL DEFAULT 0,
    snapshot_id  text,
    size_bytes   bigint,
    attempts     integer           NOT NULL DEFAULT 0,
    error        text,
    requested_at timestamptz       NOT NULL DEFAULT now(),
    started_at   timestamptz,
    finished_at  timestamptz,
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
    CHECK ((status = 'complete') = (snapshot_id IS NOT NULL)),
    CHECK ((status IN ('complete', 'failed')) = (finished_at IS NOT NULL))
);

-- At most one open request per agent: a recreate waits on the request that is already there.
CREATE UNIQUE INDEX home_checkpoints_open ON home_checkpoints (org_id, agent_id)
    WHERE status IN ('requested', 'running');
CREATE INDEX home_checkpoints_agent ON home_checkpoints (org_id, agent_id, requested_at DESC);

-- sandbox_id is the OpenShell ID of the sandbox last observed. home_generation used to be that
-- ID; it is now the lineage of the home, which a sandbox restored from a checkpoint keeps.
-- restore_checkpoint_id is set when the sandbox is deleted for a recreation whose home was
-- checkpointed: the next sandbox keeps the generation, and the first run restores before the
-- harness starts (restore_claimed_at is that run's lease).
ALTER TABLE agent_sandboxes
    ADD COLUMN sandbox_id            text,
    ADD COLUMN restore_checkpoint_id uuid,
    ADD COLUMN restore_claimed_at    timestamptz;
UPDATE agent_sandboxes SET sandbox_id = home_generation;
