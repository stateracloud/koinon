-- SPDX-License-Identifier: Apache-2.0
--
-- The leak guard's open calls (#254, ADR 0097 step 1, ADR 0091 decision 4). Expand-only: one new
-- table, nothing that exists changes.
--
-- agentgateway v1.7.0 asks the guard about a tools/call's successful result only: a JSON-RPC error
-- reply, and a call the gateway or the server cut, reach the agent without the guard seeing them.
-- The guard opens a row when it passes a call whose result would set a leg, and closes it when it
-- records that result. A row still open counts as a reply the guard never saw: the session's
-- decisions take both legs while it is open, and once it is older than the guard's deadline both
-- legs are recorded on the session and the row goes.

-- +goose Up

CREATE TABLE leak_guard_open_calls (
    org_id      uuid        NOT NULL,
    id          uuid        NOT NULL DEFAULT uuidv7(),
    agent_id    uuid        NOT NULL,
    generation  text        NOT NULL,
    -- The gateway target (McpRequest.service_names) and the tool's own name, as the call and its
    -- result both name them.
    target      text        NOT NULL CHECK (target <> '' AND length(target) <= 253),
    tool        text        NOT NULL CHECK (tool <> '' AND length(tool) <= 253),
    opened_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, agent_id, generation) REFERENCES leak_guard_sessions (org_id, agent_id, generation) ON DELETE CASCADE
);

CREATE INDEX leak_guard_open_calls_session ON leak_guard_open_calls (org_id, agent_id, generation, target, tool, opened_at);
