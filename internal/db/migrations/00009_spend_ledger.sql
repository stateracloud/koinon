-- SPDX-License-Identifier: Apache-2.0
--
-- The minimal spend ledger (plan item L1; ADRs 0041, 0049): one row per model
-- request agentgateway served, from its OTLP access-log export.

-- +goose Up

CREATE TABLE spend_entries (
    org_id        uuid           NOT NULL REFERENCES orgs (id),
    id            uuid           NOT NULL DEFAULT uuidv7(),
    agent_id      uuid           NOT NULL,
    -- The run the agent had at the time of the request; NULL when it had none
    -- (the request still counts against the agent's spend limit).
    run_id        uuid,
    ticket_id     uuid,
    -- agentgateway's per-request id (the koinon.request access-log field).
    -- An export replayed after an ambiguous failure is recorded once.
    request_id    text           NOT NULL CHECK (length(request_id) BETWEEN 1 AND 128),
    -- The gateway key that made the request (keys rotate; the agent does not).
    key_id        text           NOT NULL,
    model         text           NOT NULL,
    input_tokens  bigint         NOT NULL CHECK (input_tokens >= 0),
    output_tokens bigint         NOT NULL CHECK (output_tokens >= 0),
    -- USD from agentgateway's model cost catalog, exact as it was logged.
    cost_usd      numeric(20, 10) NOT NULL CHECK (cost_usd >= 0),
    -- False when the gateway logged no cost (an unpriced model): cost_usd is 0.
    priced        boolean        NOT NULL,
    -- When the gateway logged the request.
    occurred_at   timestamptz    NOT NULL,
    created_at    timestamptz    NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, request_id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
    FOREIGN KEY (org_id, ticket_id, run_id) REFERENCES runs (org_id, ticket_id, id),
    CHECK ((run_id IS NULL) = (ticket_id IS NULL))
);

CREATE INDEX spend_entries_run ON spend_entries (org_id, run_id) WHERE run_id IS NOT NULL;
CREATE INDEX spend_entries_ticket ON spend_entries (org_id, ticket_id) WHERE ticket_id IS NOT NULL;
CREATE INDEX spend_entries_agent_time ON spend_entries (org_id, agent_id, occurred_at);

-- Runs of an agent by start, to attribute a request to the run active at its time.
CREATE INDEX runs_agent_started_at ON runs (org_id, agent_id, started_at)
    WHERE started_at IS NOT NULL;
