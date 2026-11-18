-- SPDX-License-Identifier: Apache-2.0
--
-- A private CA per MCP server (#278, ADR 0120). A server that lives behind a private CA, and that holds
-- a credential, must be https; its certificate is checked against the system roots plus this bundle.
-- The bundle is public (certificates only), kept as PEM text next to the URL, and belongs to this one
-- server: it never widens trust for another server. An empty bundle means the system roots.
--
-- Expand only (ADR 0085): one new column with a default, and the reconciler trigger now also fires
-- when the bundle changes. An N-1 binary ignores the column.

-- +goose Up

ALTER TABLE mcp_servers
    ADD COLUMN ca_bundle text NOT NULL DEFAULT '' CHECK (length(ca_bundle) <= 262144);

DROP TRIGGER mcp_servers_notify_reconciler ON mcp_servers;
CREATE TRIGGER mcp_servers_notify_reconciler
    AFTER INSERT OR DELETE OR UPDATE OF name, url, transport, credential_header, credential_prefix, credential, disabled_reason, removed_at, credential_lost_at, admin_disabled_at, ca_bundle ON mcp_servers
    FOR EACH ROW EXECUTE FUNCTION notify_reconciler_mcp_server();
