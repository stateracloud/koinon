-- SPDX-License-Identifier: Apache-2.0
--
-- The data the leak guard reads (ADR 0084 decisions 3, 5 and 8; #350). Expand-only: it adds columns
-- and a table and changes nothing that exists.
--
-- 1. An MCP server declares which legs of the lethal trifecta it carries and its kind, so the guard
--    and the templates test read a declaration instead of guessing from names. A server may carry
--    several legs (a fetch server reads untrusted input and is an outbound sink).
-- 2. An org's leak-guard match threshold (NULL is the platform default, 24 characters).
-- 3. An org's fetch-trusted host list: the hosts the automatic URL rule exempts. It starts empty.

-- +goose Up

-- fetch: reads web pages or other untrusted documents. search: sends a query to a search provider.
-- other: anything else (the default).
CREATE TYPE mcp_server_kind AS ENUM ('fetch', 'search', 'other');

-- A server that has not been classified yet is assumed to carry all three legs. That is the
-- cautious reading: a guard that treats it as carrying fewer legs would exempt it by default, and
-- nothing reads these columns for a decision yet, so existing servers behave as before. An admin
-- narrows a server once they know what it exposes.
ALTER TABLE mcp_servers
    ADD COLUMN kind                   mcp_server_kind NOT NULL DEFAULT 'other',
    ADD COLUMN exposes_untrusted_input boolean        NOT NULL DEFAULT true,
    ADD COLUMN exposes_private_data    boolean        NOT NULL DEFAULT true,
    ADD COLUMN exposes_outbound        boolean        NOT NULL DEFAULT true;

-- The bounds are those of leakguard.Params (MinThreshold, MaxThreshold).
ALTER TABLE org_settings
    ADD COLUMN leak_match_threshold integer CHECK (leak_match_threshold IS NULL OR leak_match_threshold BETWEEN 12 AND 256);

CREATE TABLE org_fetch_trusted_hosts (
    org_id     uuid        NOT NULL REFERENCES orgs (id),
    id         uuid        NOT NULL DEFAULT uuidv7(),
    -- A lower-case DNS host name: no scheme, port, path or wildcard. The URL rule matches a URL's
    -- host against it exactly.
    host       text        NOT NULL CHECK (
        length(host) <= 253
        AND host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, host)
);
