-- SPDX-License-Identifier: Apache-2.0
--
-- The gateway key that opened the sealed credential of an MCP tool call (ADR 0097 decision 12.6, #1151).
-- The ID is a name (gk- and 16 hex characters), never key material; "" means the call used no sealed
-- credential (none, or a plaintext v1 credential). A call whose credential could not be opened keeps the key ID
-- the envelope names, so an operator can see which key was missing.
--
-- Expand only (ADR 0085): a new column with a default. An N-1 binary writes rows without it. The gateway's
-- INSERT grant on the table covers the new column.

-- +goose Up

ALTER TABLE mcp_call_log
    ADD COLUMN recipient_key_id text NOT NULL DEFAULT '' CHECK (recipient_key_id ~ '^(gk-[0-9a-f]{16})?$');
