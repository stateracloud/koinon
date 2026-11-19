-- SPDX-License-Identifier: Apache-2.0
--
-- Remote agents (#1129, ADR 0123 decisions 9, 11 and 13): an external A2A agent is its own resource of an
-- org. It is not an MCP server and not a value of the mcp_transport enum.
--
-- remote_agents holds the agent's base URL, its credential (the same sealed envelope and the same checks as
-- mcp_servers), its TLS trust, the pin of its agent card and its health. The card is data from another
-- party and is for admins only. card and card_hash are the pinned card: only an admin changes them (at
-- registration, on a new URL, or with AcceptRemoteAgentCard). observed_card and observed_card_hash are the
-- last card the health check saw when it differed from the pin: a finding, never an update.
--
-- agent_remote_agents holds the grants: an agent with no row calls no remote agent. Both tables are keyed
-- by (org_id, id) and use composite foreign keys.
--
-- A change of the rows that decide what is served wakes the reconciler, as the MCP tables do (00059, 00061).
-- The function notify_reconciler_mcp_server (00059) only reads org_id, so it serves these tables too. A
-- change of health wakes it only when the value changes: the health check writes the row on each pass, and
-- a trigger on each write would wake the reconciler after every check.
--
-- Expand only (ADR 0085): two new tables, two triggers. Nothing that exists changes. An N-1 binary ignores
-- the tables.

-- +goose Up

CREATE TABLE remote_agents (
    org_id                uuid        NOT NULL REFERENCES orgs (id),
    id                    uuid        NOT NULL DEFAULT uuidv7(),
    -- A DNS label of at most 24 characters (the same rule as an MCP server name): the model passes it as
    -- the `agent` argument of the built-in tools.
    name                  text        NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,22}[a-z0-9])?$'),
    -- The base URL of the external agent. The card is at /.well-known/agent-card.json under it.
    card_url              text        NOT NULL CHECK (card_url ~ '^https?://[^[:space:]]+$' AND length(card_url) <= 2048),
    -- The static credential, as in mcp_servers: the header's name, the part of the value before the secret
    -- and the envelope (internal/secretstore). NULL for an agent that needs none.
    credential_header     text        CHECK (credential_header ~ '^[A-Za-z0-9!#$%&''*+.^_|~-]{1,128}$'),
    credential_prefix     text        NOT NULL DEFAULT '' CHECK (credential_prefix ~ '^[ -~]{0,32}$'),
    credential            jsonb,
    credential_lost_at    timestamptz,
    -- TLS trust, as in mcp_servers (00095).
    ca_bundle             text        NOT NULL DEFAULT '' CHECK (length(ca_bundle) <= 65536),
    insecure_skip_verify  boolean     NOT NULL DEFAULT false,
    -- The pin. card_hash is the SHA-256 (hex) of the canonical JSON of the card. card is that canonical
    -- JSON, at most 256 KiB, as text so the stored bytes are the hashed bytes.
    card_hash             text        NOT NULL CHECK (card_hash ~ '^[0-9a-f]{64}$'),
    card                  text        NOT NULL CHECK (length(card) <= 262144),
    -- What the health check saw when it was not the pin. Both are set or both are NULL.
    observed_card_hash    text        CHECK (observed_card_hash ~ '^[0-9a-f]{64}$'),
    observed_card         text        CHECK (length(observed_card) <= 262144),
    card_checked_at       timestamptz,
    -- Health: written by the health check of the reconciler, every 60 s. The type is the one of
    -- mcp_servers.
    health                mcp_health  NOT NULL DEFAULT 'unknown',
    health_reason         text,
    health_checked_at     timestamptz,
    health_since          timestamptz,
    consecutive_failures  integer     NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    disabled_reason       text,
    created_by            uuid        REFERENCES users (id),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    CHECK (credential IS NULL OR jsonb_typeof(credential) = 'object'),
    -- A lost credential (keys forget) keeps its header name and prefix: the form then asks for the value only.
    CONSTRAINT remote_agents_credential_header CHECK ((credential IS NULL) = (credential_header IS NULL)
        OR (credential IS NULL AND credential_lost_at IS NOT NULL)),
    CONSTRAINT remote_agents_credential_lost CHECK (credential_lost_at IS NULL OR credential IS NULL),
    CHECK (NOT (insecure_skip_verify AND ca_bundle <> '')),
    CHECK (NOT insecure_skip_verify OR lower(card_url) LIKE 'https://%'),
    CHECK ((observed_card_hash IS NULL) = (observed_card IS NULL)),
    -- An observed card is a finding only when it differs from the pin.
    CHECK (observed_card_hash IS NULL OR observed_card_hash <> card_hash)
);

CREATE UNIQUE INDEX remote_agents_org_name ON remote_agents (org_id, name);

-- Writing an envelope clears the lost mark (00052). Named so it runs before set_updated_at.
CREATE TRIGGER credential_lost_clear BEFORE UPDATE ON remote_agents
    FOR EACH ROW EXECUTE FUNCTION credential_lost_clear('credential');

CREATE TRIGGER remote_agents_set_updated_at BEFORE UPDATE ON remote_agents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER remote_agents_notify_reconciler
    AFTER INSERT OR DELETE OR UPDATE OF name, card_url, credential_header, credential_prefix, credential, credential_lost_at, ca_bundle, insecure_skip_verify, card_hash, disabled_reason ON remote_agents
    FOR EACH ROW EXECUTE FUNCTION notify_reconciler_mcp_server();

CREATE TRIGGER remote_agents_health_notify_reconciler
    AFTER UPDATE OF health ON remote_agents
    FOR EACH ROW WHEN (OLD.health IS DISTINCT FROM NEW.health)
    EXECUTE FUNCTION notify_reconciler_mcp_server();

-- The remote agents an agent may call. An admin edits it directly, not through an agent version, as
-- agent_mcp_servers (00015). An agent with no row calls no remote agent.
CREATE TABLE agent_remote_agents (
    org_id          uuid NOT NULL,
    agent_id        uuid NOT NULL,
    remote_agent_id uuid NOT NULL,
    PRIMARY KEY (org_id, agent_id, remote_agent_id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, remote_agent_id) REFERENCES remote_agents (org_id, id) ON DELETE CASCADE
);

CREATE INDEX agent_remote_agents_remote ON agent_remote_agents (org_id, remote_agent_id);

CREATE TRIGGER agent_remote_agents_notify_reconciler
    AFTER INSERT OR DELETE OR UPDATE ON agent_remote_agents
    FOR EACH ROW EXECUTE FUNCTION notify_reconciler_mcp_server();
