-- SPDX-License-Identifier: Apache-2.0

-- +goose Up

-- The live koinon-gateway copies (ADR 0097 decision 2.1). A copy that holds the State stream open
-- touches its row at the poll interval and removes it when the stream ends. A copy that dies without
-- a clean end leaves a row that goes stale: the reader counts only rows seen within a bound. The
-- reconciler names these copies to staterev.AllAcked before it probes an MCP server (#1260).
CREATE TABLE gateway_state_copies (
    copy_id   text        PRIMARY KEY CHECK (char_length(copy_id) BETWEEN 1 AND 253),
    last_seen timestamptz NOT NULL DEFAULT now()
);

-- +goose Down

DROP TABLE gateway_state_copies;
