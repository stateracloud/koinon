-- SPDX-License-Identifier: Apache-2.0
--
-- Home repository epochs (ADR 0085 decisions 13 and 14; #213). When a key-encryption key is lost or
-- leaked, each agent's home gets a new kopia repository with a new random password, at
-- orgs/<org>/homes/<agent>/r<N>/; the old one is kept until its deletion deadline, so its
-- checkpoints stay listed and, while its password still opens, restorable.
--
-- Expand-only (ADR 0085 decisions 5 and 6): one new table and new columns; nothing that exists
-- changes, and the schema_compat declaration stays as it is.
--
-- 1. Epoch 0 is the existing home_repositories row (prefix orgs/<org>/homes/<agent>/), which old
--    binaries keep using. Epochs 1 and up are rows of home_repository_epochs. The agent's current
--    repository is its highest epoch.
-- 2. Each checkpoint records the repository epoch it was written to; a restore opens that one.
-- 3. A superseded repository records when it was retired and when its blobs may be deleted; the
--    sweeper sets deleted_at once they are.
-- 4. A repository whose password was lost with its key-encryption key (koinon-api keys forget,
--    #85) has no envelope and credential_lost_at set, like the other sealed credentials (00052
--    credential_lost): its checkpoints cannot be restored. Dropping NOT NULL is an expansion; an
--    older binary reading the row fails to open it, as it would with the key gone.

-- +goose Up

ALTER TABLE home_repositories
    ADD COLUMN retired_at   timestamptz,
    ADD COLUMN delete_after timestamptz,
    ADD COLUMN deleted_at   timestamptz,
    ADD COLUMN credential_lost_at timestamptz,
    ADD CONSTRAINT home_repositories_lifecycle
        CHECK ((delete_after IS NULL OR retired_at IS NOT NULL) AND (deleted_at IS NULL OR delete_after IS NOT NULL)),
    ADD CONSTRAINT home_repositories_credential_lost
        CHECK ((password_envelope IS NULL) = (credential_lost_at IS NOT NULL));
ALTER TABLE home_repositories ALTER COLUMN password_envelope DROP NOT NULL;

CREATE TABLE home_repository_epochs (
    org_id            uuid        NOT NULL,
    agent_id          uuid        NOT NULL,
    repository_epoch  integer     NOT NULL CHECK (repository_epoch >= 1),
    -- orgs/<org>/homes/<agent>/r<N>/, fixed when the epoch starts.
    prefix            text        NOT NULL CHECK (prefix ~ '^orgs/[0-9a-f-]{36}/homes/[0-9a-f-]{36}/r[1-9][0-9]*/$'),
    -- Sealed like home_repositories.password_envelope, bound to binding_row; NULL once lost.
    password_envelope jsonb,
    credential_lost_at timestamptz,
    -- The envelope's AAD row (secretstore.Binding.RowID): one agent has several epochs.
    binding_row       text        GENERATED ALWAYS AS (agent_id::text || '/r' || repository_epoch::text) STORED,
    -- Why the epoch started: the previous repository's password was lost with its key, or leaked.
    reason            text        NOT NULL CHECK (reason IN ('key_lost', 'key_compromised')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    retired_at        timestamptz,
    delete_after      timestamptz,
    deleted_at        timestamptz,
    PRIMARY KEY (org_id, agent_id, repository_epoch),
    UNIQUE (org_id, binding_row),
    FOREIGN KEY (org_id, agent_id) REFERENCES home_repositories (org_id, agent_id),
    CONSTRAINT home_repository_epochs_lifecycle
        CHECK ((delete_after IS NULL OR retired_at IS NOT NULL) AND (deleted_at IS NULL OR delete_after IS NOT NULL)),
    CONSTRAINT home_repository_epochs_credential
        CHECK ((password_envelope IS NULL) = (credential_lost_at IS NOT NULL))
);

-- The repositories whose blobs are due for deletion.
CREATE INDEX home_repositories_delete_due ON home_repositories (delete_after) WHERE deleted_at IS NULL AND delete_after IS NOT NULL;
CREATE INDEX home_repository_epochs_delete_due ON home_repository_epochs (delete_after) WHERE deleted_at IS NULL AND delete_after IS NOT NULL;

-- 0 for every checkpoint written before epochs existed: they are in home_repositories' prefix.
ALTER TABLE home_checkpoints ADD COLUMN repository_epoch integer NOT NULL DEFAULT 0 CHECK (repository_epoch >= 0);
