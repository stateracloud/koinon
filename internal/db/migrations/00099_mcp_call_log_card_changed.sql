-- SPDX-License-Identifier: Apache-2.0
--
-- The audit row of a call to a built-in remote agent target can have the outcome card_changed (#1129,
-- ADR 0123 decision 11): the agent card of the remote agent no longer matches its pin, so the call was
-- refused before any message went out.
--
-- Expand only (ADR 0085): the check allows one more value. An N-1 binary writes only the old values, which
-- the new check allows too.

-- +goose Up

ALTER TABLE mcp_call_log DROP CONSTRAINT mcp_call_log_outcome_check;
ALTER TABLE mcp_call_log ADD CONSTRAINT mcp_call_log_outcome_check
    CHECK (outcome IN ('ok', 'tool_error', 'refused_grant', 'refused_guard', 'upstream_error', 'card_changed'));

-- +goose Down

ALTER TABLE mcp_call_log DROP CONSTRAINT mcp_call_log_outcome_check;
ALTER TABLE mcp_call_log ADD CONSTRAINT mcp_call_log_outcome_check
    CHECK (outcome IN ('ok', 'tool_error', 'refused_grant', 'refused_guard', 'upstream_error'));
