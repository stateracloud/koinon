-- SPDX-License-Identifier: Apache-2.0
--
-- TLS trust per MCP server (#278, ADR 0057). The standard tool pattern: curl --cacert / -k, kubectl
-- certificate-authority / insecure-skip-tls-verify.
--
-- ca_bundle: the PEM certificates of the one CA that signs this server's certificate. The server is
-- verified against ONLY this bundle, which REPLACES the public roots for this server (as curl --cacert
-- does), and it never widens trust for another server. Public (certificates only). An empty bundle
-- means the public (system) roots.
--
-- insecure_skip_verify: TLS is used but the certificate is not verified (as curl -k). It is an https
-- feature, and it excludes ca_bundle: there is nothing to verify against.
--
-- Expand only (ADR 0085): two new columns with defaults, and the reconciler trigger now also fires
-- when either changes. An N-1 binary ignores the columns.

-- +goose Up

ALTER TABLE mcp_servers
    ADD COLUMN ca_bundle text NOT NULL DEFAULT '' CHECK (length(ca_bundle) <= 65536),
    ADD COLUMN insecure_skip_verify boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT mcp_servers_tls_trust_exclusive CHECK (NOT (insecure_skip_verify AND ca_bundle <> '')),
    ADD CONSTRAINT mcp_servers_insecure_needs_https CHECK (NOT insecure_skip_verify OR lower(url) LIKE 'https://%');

DROP TRIGGER mcp_servers_notify_reconciler ON mcp_servers;
CREATE TRIGGER mcp_servers_notify_reconciler
    AFTER INSERT OR DELETE OR UPDATE OF name, url, transport, credential_header, credential_prefix, credential, disabled_reason, removed_at, credential_lost_at, admin_disabled_at, ca_bundle, insecure_skip_verify ON mcp_servers
    FOR EACH ROW EXECUTE FUNCTION notify_reconciler_mcp_server();
