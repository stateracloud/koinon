-- SPDX-License-Identifier: Apache-2.0
--
-- Third-party MCP tool descriptions are sanitised at sync (issue #267, ADR 0057 amendment): the
-- stored description and the content hash are of the sanitised text. These columns keep what the
-- admin needs to see: the published length and whether it was cut or cleaned.

-- +goose Up

-- 0 on a row synced before this migration; the next sync that changes the sanitised text flags the
-- tool for review like any other change.
ALTER TABLE mcp_tools
    ADD COLUMN description_original_length bigint NOT NULL DEFAULT 0 CHECK (description_original_length >= 0),
    ADD COLUMN description_capped          boolean NOT NULL DEFAULT false,
    ADD COLUMN description_cleaned         boolean NOT NULL DEFAULT false;

