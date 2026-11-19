-- SPDX-License-Identifier: Apache-2.0
--
-- Pin the tool list to the text an admin approved (#1132, ADR 0097 step 2, ADR 0091 decision 4).
-- The tool text reaches the model as instructions. The sync keeps the text the server published last
-- (tool_json, tool_hash). An admin who sets a tier or acknowledges a review approves that text
-- (approved_json, approved_hash). The gateway serves a tool only when the hash of its sanitised text
-- equals approved_hash. A changed tool is left out until an admin approves it again.
-- Expand only (ADR 0085): new nullable columns. A row with no approved_hash is unreviewed; the sync
-- adopts a pin for a row that an admin reviewed before this migration (see SyncTools).

-- +goose Up

ALTER TABLE mcp_tools
    -- The sanitised tool as the server published it last (title, annotations and output schema too),
    -- and its hash. NULL until the next sync after this migration.
    ADD COLUMN tool_json     jsonb,
    ADD COLUMN tool_hash     text CHECK (tool_hash IS NULL OR tool_hash ~ '^[0-9a-f]{64}$'),
    -- The tool an admin approved, and its hash. NULL while no admin approved the tool.
    ADD COLUMN approved_json jsonb,
    ADD COLUMN approved_hash text CHECK (approved_hash IS NULL OR approved_hash ~ '^[0-9a-f]{64}$'),
    ADD COLUMN approved_at   timestamptz,
    ADD CONSTRAINT mcp_tools_tool_json_object CHECK (tool_json IS NULL OR jsonb_typeof(tool_json) = 'object'),
    ADD CONSTRAINT mcp_tools_approved_json_object CHECK (approved_json IS NULL OR jsonb_typeof(approved_json) = 'object'),
    ADD CONSTRAINT mcp_tools_approved_pair CHECK ((approved_json IS NULL) = (approved_hash IS NULL));
