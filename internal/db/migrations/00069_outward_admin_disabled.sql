-- SPDX-License-Identifier: Apache-2.0
--
-- The outward switch (ADR 0110 decision 5, issue 516): one audited admin "disabled" state for every
-- table that names something Koinon calls outside the install. The columns are the same on all five and
-- separate from the system states (disabled_reason, removed_at), so re-declaring or un-shadowing an item
-- never clears an admin's disable. Expand only (ADR 0085): nullable columns.
--
-- admin_disabled_by is the actor as the audit log reads it ("user <id>", "token <id>", "secret <name>").
-- A "secret <name>" actor means the declaring Secret says disabled: true; the UI cannot enable that.
--
-- admin_disabled_under_by and admin_disabled_under_note keep an admin's disable that a Secret claimed: when
-- the Secret drops disabled: true, the item stays disabled with the admin as owner.

-- +goose Up

ALTER TABLE mcp_servers ADD COLUMN admin_disabled_at timestamptz, ADD COLUMN admin_disabled_by text, ADD COLUMN admin_disabled_note text, ADD COLUMN admin_disabled_under_by text, ADD COLUMN admin_disabled_under_note text;
ALTER TABLE providers ADD COLUMN admin_disabled_at timestamptz, ADD COLUMN admin_disabled_by text, ADD COLUMN admin_disabled_note text, ADD COLUMN admin_disabled_under_by text, ADD COLUMN admin_disabled_under_note text;
ALTER TABLE connections ADD COLUMN admin_disabled_at timestamptz, ADD COLUMN admin_disabled_by text, ADD COLUMN admin_disabled_note text, ADD COLUMN admin_disabled_under_by text, ADD COLUMN admin_disabled_under_note text;
ALTER TABLE trace_destinations ADD COLUMN admin_disabled_at timestamptz, ADD COLUMN admin_disabled_by text, ADD COLUMN admin_disabled_note text, ADD COLUMN admin_disabled_under_by text, ADD COLUMN admin_disabled_under_note text;
ALTER TABLE platform_trace_destination ADD COLUMN admin_disabled_at timestamptz, ADD COLUMN admin_disabled_by text, ADD COLUMN admin_disabled_note text, ADD COLUMN admin_disabled_under_by text, ADD COLUMN admin_disabled_under_note text;

-- ListServedMCPServers filters on admin_disabled_at, so an admin disable has to wake the reconciler like
-- any other change that takes a server out of the served list (00059). The trigger is recreated with the
-- column added; the function is unchanged.
DROP TRIGGER mcp_servers_notify_reconciler ON mcp_servers;
CREATE TRIGGER mcp_servers_notify_reconciler
    AFTER INSERT OR DELETE OR UPDATE OF name, url, transport, credential_header, credential_prefix, credential, disabled_reason, removed_at, credential_lost_at, admin_disabled_at ON mcp_servers
    FOR EACH ROW EXECUTE FUNCTION notify_reconciler_mcp_server();
