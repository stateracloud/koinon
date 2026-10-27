-- SPDX-License-Identifier: Apache-2.0
--
-- Scoped approvals (plan item AP1; ADR 0042 amendment, ADR 0065 decision 10a). An org admin who
-- approves a request can widen the approval from "this exact call" (once, the default and no row
-- here) to "this task" or "this agent until a date", for the same connector, tool and target.
-- Matching is exact on all three; arguments are not part of a scope and still pass every check.
-- A scope can be revoked and ends at expires_at.

-- +goose Up

CREATE TYPE approval_scope_kind AS ENUM ('task', 'agent');

CREATE TABLE approval_scopes (
    org_id             uuid                NOT NULL REFERENCES orgs (id),
    id                 uuid                NOT NULL DEFAULT uuidv7(),
    agent_id           uuid                NOT NULL,
    connector          text                NOT NULL CHECK (connector ~ '^[a-z][a-z0-9]*(-[a-z0-9]+)*$'),
    tool               text                NOT NULL CHECK (tool ~ '^[a-z][a-z0-9_]{0,63}$'),
    target             text                NOT NULL CHECK (length(target) BETWEEN 1 AND 300),
    kind               approval_scope_kind NOT NULL,
    -- The task a 'task' scope is for; NULL for an 'agent' scope.
    task_id            uuid,
    expires_at         timestamptz         NOT NULL,
    created_by         uuid                NOT NULL REFERENCES users (id),
    -- The approval that granted it.
    source_approval_id uuid                NOT NULL,
    revoked_at         timestamptz,
    revoked_by         uuid                REFERENCES users (id),
    created_at         timestamptz         NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
    FOREIGN KEY (org_id, task_id) REFERENCES tasks (org_id, id),
    FOREIGN KEY (org_id, source_approval_id) REFERENCES approval_requests (org_id, id),
    CHECK ((kind = 'task') = (task_id IS NOT NULL)),
    CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);

-- Matching a call, and the agent page's list.
CREATE INDEX approval_scopes_agent ON approval_scopes (org_id, agent_id, connector, tool, target)
    WHERE revoked_at IS NULL;

-- The scope that let a call run without a human decision (NULL otherwise).
ALTER TABLE approval_requests
    ADD COLUMN scope_id uuid,
    ADD FOREIGN KEY (org_id, scope_id) REFERENCES approval_scopes (org_id, id);
