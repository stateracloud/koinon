-- SPDX-License-Identifier: Apache-2.0
--
-- Remote MCP servers per org, their tools and the tier of each tool (plan item MC1; ADR 0057
-- amendment 2026-10-24).

-- +goose Up

CREATE TYPE mcp_transport AS ENUM ('streamable-http', 'sse');
-- ask: a call parks the run and a human approves it (ADR 0008, 0042); auto: the agent calls it
-- directly; never: no agent may call it.
CREATE TYPE mcp_tier AS ENUM ('ask', 'auto', 'never');
CREATE TYPE mcp_health AS ENUM ('unknown', 'healthy', 'unhealthy');

CREATE TABLE mcp_servers (
    org_id               uuid          NOT NULL REFERENCES orgs (id),
    id                   uuid          NOT NULL DEFAULT uuidv7(),
    -- A DNS label of at most 24 characters: the gateway target is "<org slug>-<name>", and a
    -- target name is at most 63 characters (an org slug has at most 40).
    name                 text          NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,22}[a-z0-9])?$'),
    url                  text          NOT NULL CHECK (url ~ '^https?://[^[:space:]]+$' AND length(url) <= 2048),
    transport            mcp_transport NOT NULL DEFAULT 'streamable-http',
    -- The static credential: the header's name and the part of its value before the secret
    -- ('Bearer ' or empty). The secret is the envelope; NULL for a server that needs none.
    credential_header    text          CHECK (credential_header ~ '^[A-Za-z0-9!#$%&''*+.^_|~-]{1,128}$'),
    credential_prefix    text          NOT NULL DEFAULT '' CHECK (credential_prefix ~ '^[ -~]{0,32}$'),
    credential           jsonb,
    source               config_source NOT NULL DEFAULT 'ui',
    managed_by_secret    text,
    declared_version     text,
    removed_at           timestamptz,
    disabled_reason      text,
    -- Health (plan item MC1): written by the reconciler's prober, every 60 s.
    health               mcp_health    NOT NULL DEFAULT 'unknown',
    health_reason        text,
    health_checked_at    timestamptz,
    -- Since when health has its current value.
    health_since         timestamptz,
    consecutive_failures integer       NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    -- The last successful tools/list that was recorded in mcp_tools.
    tools_synced_at      timestamptz,
    created_by           uuid          REFERENCES users (id),
    created_at           timestamptz   NOT NULL DEFAULT now(),
    updated_at           timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    CHECK (credential IS NULL OR jsonb_typeof(credential) = 'object'),
    CHECK ((credential IS NULL) = (credential_header IS NULL)),
    CHECK ((source = 'secret') = (managed_by_secret IS NOT NULL))
);

CREATE UNIQUE INDEX mcp_servers_org_name ON mcp_servers (org_id, name, source);
CREATE UNIQUE INDEX mcp_servers_org_name_enabled ON mcp_servers (org_id, name) WHERE disabled_reason IS NULL;

CREATE TRIGGER mcp_servers_set_updated_at BEFORE UPDATE ON mcp_servers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE mcp_tools (
    org_id           uuid        NOT NULL,
    server_id        uuid        NOT NULL,
    name             text        NOT NULL CHECK (name ~ '^[A-Za-z0-9_.-]{1,128}$'),
    description      text        NOT NULL DEFAULT '',
    -- The tool's input schema as the server published it (for the ask-tier proxy, which offers
    -- the same schema to the agent).
    input_schema     jsonb       NOT NULL DEFAULT '{"type":"object"}',
    -- SHA-256 (hex) of the description and the canonical input schema. A change of the hash of a
    -- known tool flags it for review.
    content_hash     text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    -- The server's readOnlyHint annotation: only a suggestion; an admin decides the tier.
    read_only_hint   boolean     NOT NULL DEFAULT false,
    tier             mcp_tier    NOT NULL DEFAULT 'ask',
    -- Set while the tool's description or schema changed since an admin last looked.
    review_reason    text,
    -- False once the server stopped listing the tool. The row stays: the tier survives a server
    -- that lists the tool again.
    present          boolean     NOT NULL DEFAULT true,
    first_seen_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at     timestamptz NOT NULL DEFAULT now(),
    -- A call needs two approvers (distinct, neither the ticket's creator) before Koinon makes it.
    -- Only meaningful in the ask tier.
    requires_two_approvers boolean NOT NULL DEFAULT false,
    tier_set_by      text,
    tier_set_at      timestamptz,
    PRIMARY KEY (org_id, server_id, name),
    FOREIGN KEY (org_id, server_id) REFERENCES mcp_servers (org_id, id) ON DELETE CASCADE,
    CHECK (jsonb_typeof(input_schema) = 'object')
);

-- The servers an agent may use, optionally narrowed to some tools (ADR 0057). An admin edits it
-- directly, not through an agent version: what an agent may do with a server is bounded by the
-- tiers an admin set on its tools, and adding a server is not a grant of a new kind of power.
CREATE TABLE agent_mcp_servers (
    org_id    uuid NOT NULL,
    agent_id  uuid NOT NULL,
    server_id uuid NOT NULL,
    -- NULL allows every tool the server has, including tools it adds later (in the ask tier);
    -- a list allows those tools only.
    tools     text[],
    PRIMARY KEY (org_id, agent_id, server_id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, server_id) REFERENCES mcp_servers (org_id, id) ON DELETE CASCADE
);

CREATE INDEX agent_mcp_servers_server ON agent_mcp_servers (org_id, server_id);

-- MCP tool names are not limited to lower case and underscores, and the ask-tier proxy records
-- them as "<server>/<tool>" calls.
ALTER TABLE approval_requests DROP CONSTRAINT approval_requests_tool_check;
ALTER TABLE approval_requests ADD CONSTRAINT approval_requests_tool_check
    CHECK (tool ~ '^[A-Za-z0-9_./-]{1,200}$');

-- Two approvers (plan item MC1): how many approvals a request needs, who gave the first, and what an
-- MCP call was bound to when it was recorded (server, URL hash, the tool's content hash), which the
-- executor checks again.
ALTER TABLE approval_requests
    ADD COLUMN approvals_required smallint NOT NULL DEFAULT 1 CHECK (approvals_required IN (1, 2)),
    ADD COLUMN first_approver     uuid REFERENCES users (id),
    ADD COLUMN call_binding       jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(call_binding) = 'object');
