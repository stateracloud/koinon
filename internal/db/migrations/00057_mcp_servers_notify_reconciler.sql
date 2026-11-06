-- SPDX-License-Identifier: Apache-2.0
--
-- Wake the reconciler when an MCP server or its credential changes (#57). Until now a new or changed
-- server waited for the next resync (30 s) before the reconciler planned the gateway target and the
-- prober listed its tools, which made step 6b of the quickstart slow. It is a trigger, like the
-- agents and runs triggers (00006), so every writer wakes it: the API, the declared-config path and
-- the CLI.
--
-- Only the columns that change the gateway target or the credential wake it. The prober's own
-- writes (health, tools_synced_at, consecutive_failures) are left out on purpose: a trigger on
-- them would wake the reconciler after every probe, which would wake the prober again.
-- Expand only (ADR 0085): a trigger and a function; an N-1 binary is unaffected by an extra NOTIFY.

-- +goose Up

-- +goose StatementBegin
CREATE FUNCTION notify_reconciler_mcp_server() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        PERFORM pg_notify('koinon_reconcile', OLD.org_id::text);
    ELSE
        PERFORM pg_notify('koinon_reconcile', NEW.org_id::text);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER mcp_servers_notify_reconciler
    AFTER INSERT OR DELETE OR UPDATE OF url, transport, credential_header, credential_prefix, credential, disabled_reason, removed_at ON mcp_servers
    FOR EACH ROW EXECUTE FUNCTION notify_reconciler_mcp_server();
