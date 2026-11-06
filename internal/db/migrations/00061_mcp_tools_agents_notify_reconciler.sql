-- SPDX-License-Identifier: Apache-2.0
--
-- Wake the reconciler when a tool's tier or sink declaration changes, a tool appears, goes or turns absent or present again, or an
-- agent's MCP allowlist changes (#249, step 6b of the quickstart). Migration 00059 covered mcp_servers
-- only, so SetTier and SetAgentServers waited for the next resync (30 s) before the gateway plan
-- followed them. It reuses notify_reconciler_mcp_server() from 00059: both tables carry org_id.
--
-- mcp_tools: INSERT and DELETE (a new tool changes the plan once; the prober inserts each tool one
-- time), and UPDATE OF only the columns an admin or a content change writes: tier and the sink
-- declaration. Not present, last_seen_at, read_only_hint or the description report columns: the prober's
-- TouchMCPTool names those on every probe (UPDATE OF fires on every statement that names the column,
-- changed or not), so a trigger on them would wake the reconciler after every probe, which would wake the
-- prober again.
-- present has its own trigger below that fires only when the value changes, so a re-probe of an
-- unchanged tool stays silent while MarkMCPToolsAbsent (and a tool coming back) wakes the plan.
-- agent_mcp_servers: only the API writes it (delete and insert of an agent's rows).
-- Expand only (ADR 0085): triggers; an N-1 binary is unaffected by an extra NOTIFY.

-- +goose Up

CREATE TRIGGER mcp_tools_notify_reconciler
    AFTER INSERT OR DELETE OR UPDATE OF tier, sink_class, sink_derivation, address_args, result_untrusted_input, result_private_data ON mcp_tools
    FOR EACH ROW EXECUTE FUNCTION notify_reconciler_mcp_server();

CREATE TRIGGER mcp_tools_present_notify_reconciler
    AFTER UPDATE OF present ON mcp_tools
    FOR EACH ROW WHEN (OLD.present IS DISTINCT FROM NEW.present)
    EXECUTE FUNCTION notify_reconciler_mcp_server();

CREATE TRIGGER agent_mcp_servers_notify_reconciler
    AFTER INSERT OR DELETE OR UPDATE ON agent_mcp_servers
    FOR EACH ROW EXECUTE FUNCTION notify_reconciler_mcp_server();
