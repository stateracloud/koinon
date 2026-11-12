-- SPDX-License-Identifier: Apache-2.0
--
-- Extra MCP hosts of one connection (#240). An MCP server may be linked to a code-host connection
-- only when its URL names the connection's own host or a host the connector declares (#117, ADR
-- 0091). A self-hosted MCP server on its own host name is refused, and its tools count as
-- external-high. mcp_hosts is the org admin's list of further host names for this connection: exact,
-- lower-case host names, each added and removed through ConnectionService and audited. Declared
-- connections keep it too: the importer does not write the column.

-- +goose Up

ALTER TABLE connections ADD COLUMN mcp_hosts text[] NOT NULL DEFAULT '{}';
ALTER TABLE connections ADD CONSTRAINT connections_mcp_hosts_count CHECK (cardinality(mcp_hosts) <= 20);
