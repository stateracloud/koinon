-- SPDX-License-Identifier: Apache-2.0
--
-- A stuck sandbox is saved and recreated (ADR 0054), and open tickets can start a fresh session
-- with a handoff (ADR 0051 amendment 2026-10-24). Plan item AG1.

-- +goose Up

-- Sandbox health. agentd marks the sandbox it found stuck: the reconciler deletes that sandbox
-- (whatever parked runs hold it) and creates a new one, and the mark clears when the reconciler
-- records a home generation other than unhealthy_generation, so only the sandbox found stuck is
-- ever recreated for it.
ALTER TABLE agent_sandboxes
    ADD COLUMN unhealthy_at         timestamptz,
    ADD COLUMN unhealthy_reason     text,
    ADD COLUMN unhealthy_generation text,
    ADD CONSTRAINT agent_sandboxes_unhealthy
        CHECK ((unhealthy_at IS NULL) = (unhealthy_reason IS NULL)
               AND (unhealthy_at IS NULL) = (unhealthy_generation IS NULL));

-- Agent health. After three recreations in 24 hours the agent is unhealthy: new runs are refused
-- until an admin resumes it. resumed_at starts a new count.
ALTER TABLE agents
    ADD COLUMN unhealthy_at     timestamptz,
    ADD COLUMN unhealthy_reason text,
    ADD COLUMN resumed_at       timestamptz,
    ADD CONSTRAINT agents_unhealthy CHECK ((unhealthy_at IS NULL) = (unhealthy_reason IS NULL));

-- Uncommitted work saved from a stuck sandbox. The files are sealed with the envelope secret store
-- (internal/secretstore), bound to this row; the manifest (paths, modes, line counts) is not
-- secret and feeds the approval card. The envelope is dropped when the work is pushed or expires.
CREATE TABLE saved_work (
    org_id     uuid        NOT NULL REFERENCES orgs (id),
    id         uuid        NOT NULL DEFAULT uuidv7(),
    ticket_id  uuid        NOT NULL,
    run_id     uuid        NOT NULL,
    agent_id   uuid        NOT NULL,
    -- The GitHub repository as owner/name and the commit the changes apply to.
    repo       text        NOT NULL CHECK (length(repo) BETWEEN 3 AND 200),
    base_sha   text        NOT NULL CHECK (base_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    branch     text        NOT NULL CHECK (length(branch) BETWEEN 1 AND 200),
    manifest   jsonb       NOT NULL CHECK (jsonb_typeof(manifest) = 'object'),
    -- Size of the files before sealing; at most 50 MB.
    size_bytes bigint      NOT NULL CHECK (size_bytes BETWEEN 0 AND 52428800),
    envelope   jsonb       CHECK (jsonb_typeof(envelope) = 'object'),
    status     text        NOT NULL DEFAULT 'saved' CHECK (status IN ('saved', 'pushed', 'expired')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, ticket_id, run_id) REFERENCES runs (org_id, ticket_id, id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
    CHECK ((status = 'saved') = (envelope IS NOT NULL))
);

CREATE INDEX saved_work_expiry ON saved_work (expires_at) WHERE status = 'saved';
CREATE INDEX saved_work_ticket ON saved_work (org_id, ticket_id, created_at);

-- One row per stuck sandbox found: what was still running, what became of the uncommitted work
-- and what was done about the sandbox. The loop guard counts these.
CREATE TABLE sandbox_incidents (
    org_id        uuid        NOT NULL REFERENCES orgs (id),
    id            uuid        NOT NULL DEFAULT uuidv7(),
    agent_id      uuid        NOT NULL,
    run_id        uuid        NOT NULL,
    ticket_id     uuid        NOT NULL,
    detected_at   timestamptz NOT NULL DEFAULT now(),
    -- [{"pid":..,"comm":..,"state":..,"wchan":..}] as read from /proc.
    processes     jsonb       NOT NULL,
    -- recreate: the sandbox was marked unhealthy; agent_unhealthy: the loop guard stopped the agent.
    outcome       text        NOT NULL CHECK (outcome IN ('recreate', 'agent_unhealthy')),
    -- saved: see saved_work_id; nothing: no uncommitted work; lost: why in work_note.
    work          text        NOT NULL CHECK (work IN ('saved', 'nothing', 'lost')),
    work_note     text        NOT NULL DEFAULT '',
    saved_work_id uuid,
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
    FOREIGN KEY (org_id, ticket_id, run_id) REFERENCES runs (org_id, ticket_id, id),
    FOREIGN KEY (org_id, saved_work_id) REFERENCES saved_work (org_id, id)
);

CREATE INDEX sandbox_incidents_agent ON sandbox_incidents (org_id, agent_id, detected_at DESC);

-- Approval requests the system makes on its own (saving work from a stuck sandbox) do not belong
-- to the agent's turn: the run neither parks on them nor is told their outcome, and they outlive
-- the run.
ALTER TABLE approval_requests
    ADD COLUMN system boolean NOT NULL DEFAULT false;

-- The end-of-turn summary and the fresh-session request (ADR 0051 amendment).
ALTER TABLE tickets
    -- The agent's own short summary of state and next steps, written after each turn.
    ADD COLUMN agent_summary            text CHECK (length(agent_summary) <= 20000),
    ADD COLUMN agent_summary_at         timestamptz,
    -- Set by activating a version with "apply to open tickets"; the ticket's next run starts a new
    -- session with a handoff and clears it.
    ADD COLUMN session_restart_pending  boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT tickets_agent_summary CHECK ((agent_summary IS NULL) = (agent_summary_at IS NULL));

-- While agentd asks the agent for its summary, the tool servers refuse calls that record side
-- effects (GetRunningRunForAgent).
ALTER TABLE runs
    ADD COLUMN summarizing boolean NOT NULL DEFAULT false;

-- Nonces of saved-work uploads already accepted (ADR 0054): a captured request cannot be replayed.
-- The upload key itself is a Kubernetes Secret that koinon-api creates, not a database value.
CREATE TABLE saved_work_upload_nonces (
    org_id     uuid        NOT NULL,
    run_id     uuid        NOT NULL,
    nonce      text        NOT NULL CHECK (nonce ~ '^[0-9a-f]{32}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, run_id, nonce),
    FOREIGN KEY (org_id, run_id) REFERENCES runs (org_id, id)
);
