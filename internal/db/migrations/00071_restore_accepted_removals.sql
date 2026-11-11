-- SPDX-License-Identifier: Apache-2.0
--
-- The declarative removals of a restore (ADR 0110 decision 1, issue 516). The release step of a
-- restored copy records what the importer would remove (a provider, an MCP server or a connection
-- whose Secret is missing) and whether the operator accepted it. The importer removes an item on the
-- list only in state 'accepted'.
--
-- Expand only (ADR 0085): one new table.
-- +goose Up

CREATE TABLE restore_accepted_removals (
    kind        text        NOT NULL CHECK (kind IN ('model-provider', 'mcp-server', 'connection')),
    org_slug    text        NOT NULL,
    secret_name text        NOT NULL,
    release_id  uuid        NOT NULL,
    state       text        NOT NULL CHECK (state IN ('waiting', 'accepted')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, org_slug, secret_name)
);
