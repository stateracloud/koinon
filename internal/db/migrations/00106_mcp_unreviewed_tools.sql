-- SPDX-License-Identifier: Apache-2.0
--
-- Unreviewed MCP tools (#1216, ADR 0125). A tool that no admin reviewed (approved_hash IS NULL) is
-- hidden from the model until an admin reviews it, as a changed tool already is (#1132). A server
-- that an admin trusts can show new tools before the review. Koinon stores at most a fixed number of
-- unreviewed tools for each server; at that limit the sync stores no new tool and marks the server.
-- Expand only (ADR 0085): new columns with defaults.

-- +goose Up

ALTER TABLE mcp_servers
    -- Show a tool that no admin reviewed to agents, as sanitised text. Off by default: an admin turns
    -- it on for a server the admin trusts. A changed tool stays hidden whatever this says.
    ADD COLUMN show_unreviewed_tools      boolean     NOT NULL DEFAULT false,
    -- When the sync first found the server at the limit of unreviewed tools and stored no new tool.
    -- NULL while the server is under the limit.
    ADD COLUMN unreviewed_tools_capped_at timestamptz;
