-- SPDX-License-Identifier: Apache-2.0
--
-- Storage probes for keys new-repositories and keys forget (#285).
--
-- The keys command holds the KEKs but no bucket credentials; koinon-agentd holds the bucket's
-- credentials but no KEK (ADR 0032), so neither can see alone whether the prefix of the epoch a
-- rotation would start already holds a repository the database has no record of (#262 records a
-- floor only after agentd has met such a prefix). The command writes a probe request here (the
-- candidate epoch of one agent) and waits; agentd claims it, lists the agent's epoch folders in the
-- bucket and answers in the row: free, taken, or error with a message that carries no secret. The
-- command rotates only on free: a taken or errored probe, or none within its timeout, refuses the
-- rotation and writes nothing (fail closed, maintainer decision 2026-11-07).
--
-- probe_after is when agentd may claim the row (a claim moves it forward by its lease, so a dead
-- agentd leaves the row to another replica); expires_at is when the command gives up, after which
-- agentd no longer claims the row. Answered and expired rows are deleted by the command that read
-- them and, for those it left, by agentd.
--
-- Expand only (ADR 0085): a new table. An older binary of the line does not know it.

-- +goose Up

CREATE TABLE home_repository_probes (
    org_id           uuid        NOT NULL,
    id               uuid        NOT NULL DEFAULT gen_random_uuid(),
    agent_id         uuid        NOT NULL,
    repository_epoch integer     NOT NULL CHECK (repository_epoch >= 1),
    requested_at     timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    probe_after      timestamptz NOT NULL DEFAULT now(),
    answered_at      timestamptz,
    outcome          text,
    message          text,
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, agent_id) REFERENCES home_repositories (org_id, agent_id) ON DELETE CASCADE,
    CONSTRAINT home_repository_probes_outcome CHECK (
        (answered_at IS NULL) = (outcome IS NULL)
        AND (outcome IS NULL OR outcome IN ('free', 'taken', 'error'))
        AND (message IS NULL OR length(message) <= 1000))
);

-- The probes koinon-agentd still has to answer.
CREATE INDEX home_repository_probes_due ON home_repository_probes (probe_after) WHERE answered_at IS NULL;
-- The rows to delete.
CREATE INDEX home_repository_probes_expiry ON home_repository_probes (expires_at);
