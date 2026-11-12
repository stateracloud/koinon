-- SPDX-License-Identifier: Apache-2.0
--
-- A Scoped approval names its tool the way the approval request it came from does (issue #659). The
-- request's check was relaxed for MCP tool names in 00015 (upper case, dot, hyphen: "createIssue"),
-- but approval_scopes kept the code-host pattern, so widening the approval of an MCP call to its task
-- or agent failed. This only relaxes a check: every row the old one accepted still passes, so it is an
-- expand migration and legal on a patch line too (ADR 0085).

-- +goose Up

ALTER TABLE approval_scopes DROP CONSTRAINT approval_scopes_tool_check;
ALTER TABLE approval_scopes ADD CONSTRAINT approval_scopes_tool_check
    CHECK (tool ~ '^[A-Za-z0-9_./-]{1,200}$');
