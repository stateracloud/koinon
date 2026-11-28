-- SPDX-License-Identifier: Apache-2.0
--
-- When koinon-gateway serves an agent's forge credential (#1086 part C2). koinon-reconciler, the only
-- writer, keeps a row for each active agent that has a forge read grant and so expects a sealed
-- code-host credential on its forge routes (GitHubReadAccessAt). koinon-agentd reads the row before the
-- harness starts, the same way it waits for a model: no forge read grant on the run's version, start;
-- no row, wait for a pass (or fail when the active version has no grant); a failure, fail the run with
-- it; served_at set, start; else wait. The row never holds a token, only a State revision and a cause.

-- +goose Up

CREATE TABLE gateway_agent_forge (
    org_id         uuid        NOT NULL,
    agent_id       uuid        NOT NULL,
    -- The first State revision (gateway_state_revisions) that holds the agent's credential; NULL while
    -- no stored revision holds it (its file is still reaching the gateway pod, or the publish failed).
    state_revision bigint,
    -- Set when every live koinon-gateway copy acked state_revision (staterev.AllAcked).
    served_at      timestamptz,
    -- Why the reconciler has no credential for the agent (no minter, no connection, a failed mint, no
    -- gateway key); empty when it has one. Never a token.
    failure        text        NOT NULL DEFAULT '',
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, agent_id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id) ON DELETE CASCADE
);
