-- SPDX-License-Identifier: Apache-2.0
--
-- One row per MCP tool call that Koinon's own gateway (koinon-gateway, ADR 0097 decision 5, #278)
-- served: who called, which target and tool, how the call ended, the guard's reason on a refusal and
-- the kind of credential the gateway sent. A row never holds an argument, a result or a token value.
--
-- Only the gateway writes it. The gateway's database role gets INSERT on it and nothing else
-- (migrations.GrantGateway, run by "koinon-api migrate" after goose, as for the install audit
-- writer). The org and the agent are kept as the text the gateway saw, with no foreign key: a row must
-- outlive a deleted agent or org, and the gateway need not look either up to write it.
--
-- Expand only (ADR 0085): a new table. An N-1 binary ignores it.

-- +goose Up

CREATE TABLE mcp_call_log (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    created_at      timestamptz NOT NULL DEFAULT now(),
    org_slug        text NOT NULL CHECK (length(org_slug) <= 128),
    agent           text NOT NULL CHECK (length(agent) <= 128),
    target          text NOT NULL CHECK (length(target) <= 256),
    tool            text NOT NULL CHECK (length(tool) <= 256),
    outcome         text NOT NULL CHECK (outcome IN ('ok', 'tool_error', 'refused_grant', 'refused_guard', 'upstream_error')),
    verdict         text NOT NULL DEFAULT '' CHECK (length(verdict) <= 1024),
    credential_kind text NOT NULL CHECK (length(credential_kind) <= 64),
    duration_ms     integer NOT NULL CHECK (duration_ms >= 0)
);

CREATE INDEX mcp_call_log_created_at ON mcp_call_log (created_at);
CREATE INDEX mcp_call_log_org_created_at ON mcp_call_log (org_slug, created_at);
