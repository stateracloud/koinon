-- SPDX-License-Identifier: Apache-2.0
--
-- The data the leak guard reads (ADR 0084 decisions 3, 5 and 8; #350). Expand-only: it adds columns
-- and a table and changes nothing that exists.
--
-- 1. An MCP server declares which session legs its results can set (untrusted input, private data)
--    and its kind, so the guard and the templates test read a declaration instead of guessing from
--    names. Outbound is not a server leg: ADR 0091 makes it the sink class of each call.
-- 2. An org's leak-guard match threshold (NULL is the platform default, 24 characters).
-- 3. An org's fetch-trusted host list: the hosts the automatic URL rule exempts. It starts empty.
-- 4. A sink class per tool, which arguments are address arguments, and what the tool's results carry
--    (ADR 0091 decisions 1 to 3). The default is external-high with the server's legs.
-- 5. The org's code-host scope: the repositories of its code-host connections with their visibility,
--    which the code-host derivation reads (ADR 0091 decision 3).

-- +goose Up

-- fetch: reads web pages or other untrusted documents. search: sends a query to a search provider.
-- other: anything else (the default).
CREATE TYPE mcp_server_kind AS ENUM ('fetch', 'search', 'other');

-- A server that has not been classified yet is assumed to carry both legs. That is the
-- cautious reading: a guard that treats it as carrying fewer legs would exempt it by default, and
-- nothing reads these columns for a decision yet, so existing servers behave as before. An admin
-- narrows a server once they know what it exposes.
ALTER TABLE mcp_servers
    ADD COLUMN kind                   mcp_server_kind NOT NULL DEFAULT 'other',
    ADD COLUMN exposes_untrusted_input boolean        NOT NULL DEFAULT true,
    ADD COLUMN exposes_private_data    boolean        NOT NULL DEFAULT true;

-- The bounds are those of leakguard.Params (MinThreshold, MaxThreshold).
ALTER TABLE org_settings
    ADD COLUMN leak_match_threshold integer CHECK (leak_match_threshold IS NULL OR leak_match_threshold BETWEEN 12 AND 256);

CREATE TABLE org_fetch_trusted_hosts (
    org_id     uuid        NOT NULL REFERENCES orgs (id),
    id         uuid        NOT NULL DEFAULT uuidv7(),
    -- A lower-case DNS host name (punycode for an international one) or "*." and one: no scheme,
    -- port or path. The rules of orgsettings.NormalizeTrustedEntry: at least two labels after the
    -- optional "*.", and a last label that is not a number a resolver reads as an IPv4 address
    -- (digits, or 0x and hex digits). A wildcard over a public suffix is refused by the application,
    -- which has the suffix list. The URL rule matches an exact entry exactly and a wildcard entry
    -- against any subdomain, not the apex.
    host       text        NOT NULL CHECK (
        length(host) <= 253
        AND host ~ '^(\*\.)?[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$'
        AND regexp_replace(host, '^\*\.', '') ~ '\.'
        AND regexp_replace(host, '^.*\.', '') !~ '^([0-9]+|0x[0-9a-f]*)$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, host)
);

-- The sink class of a tool call (ADR 0091 decision 1). An unknown or unclassified tool is
-- external-high: the cautious reading.
CREATE TYPE mcp_sink_class AS ENUM ('none', 'internal', 'external-low', 'external-high');

-- How the class is decided. fixed: the class column. code_host: per call from the owner and repo
-- address arguments and the org's code-host scope. fetch: per call from the url address argument
-- and the org's fetch-trusted hosts. The class column is the fallback of a derived tool and is then
-- always external-high.
CREATE TYPE mcp_sink_derivation AS ENUM ('fixed', 'code_host', 'fetch');

-- address_args lists the arguments that say where a call goes; every other argument is payload. The
-- order is the order the derivation reads them in: code_host takes [owner, repo], fetch takes [url].
-- result_untrusted_input and result_private_data say what the tool's results carry; NULL is the
-- server's own leg. A tool that was never declared has the defaults, which is also the state a changed
-- URL or content hash puts it back to (ADR 0091 decision 2, ADR 0057).
ALTER TABLE mcp_tools
    ADD COLUMN sink_class             mcp_sink_class      NOT NULL DEFAULT 'external-high',
    ADD COLUMN sink_derivation        mcp_sink_derivation NOT NULL DEFAULT 'fixed',
    ADD COLUMN address_args           text[]              NOT NULL DEFAULT '{}',
    ADD COLUMN result_untrusted_input boolean,
    ADD COLUMN result_private_data    boolean,
    ADD CONSTRAINT mcp_tools_sink_derivation_check CHECK (
        (sink_derivation = 'fixed' AND cardinality(address_args) <= 8)
        OR (sink_derivation = 'code_host' AND cardinality(address_args) = 2 AND sink_class = 'external-high')
        OR (sink_derivation = 'fetch' AND cardinality(address_args) = 1 AND sink_class = 'external-high')),
    ADD CONSTRAINT mcp_tools_address_args_check CHECK (
        array_position(address_args, NULL) IS NULL
        AND address_args::text !~ '[[:space:]]');

-- The repository's visibility as the connector last read it. unknown is a repository the connector
-- could not classify; the derivation treats unknown, public and a stale reading alike as external.
CREATE TYPE code_host_repo_visibility AS ENUM ('private', 'internal', 'public', 'unknown');

CREATE TABLE org_code_host_repos (
    org_id        uuid                      NOT NULL REFERENCES orgs (id),
    connection_id uuid                      NOT NULL,
    -- Lower case: the code hosts compare owner and repository names without case.
    owner         text                      NOT NULL CHECK (owner <> '' AND owner = lower(owner) AND length(owner) <= 255),
    repo          text                      NOT NULL CHECK (repo <> '' AND repo = lower(repo) AND length(repo) <= 255),
    visibility    code_host_repo_visibility NOT NULL,
    checked_at    timestamptz               NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, connection_id, owner, repo),
    FOREIGN KEY (org_id, connection_id) REFERENCES connections (org_id, id) ON DELETE CASCADE
);
