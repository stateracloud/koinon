-- SPDX-License-Identifier: Apache-2.0
--
-- Whether a spend entry's thinking-token split is known (issue #78).
--
-- spend_entries.reasoning_tokens is the part of output_tokens the provider reported as reasoning
-- (thinking). It is NOT NULL DEFAULT 0, so a 0 cannot tell "no thinking" from "not reported":
-- Anthropic reports the split as usage.output_tokens_details.thinking_tokens, but agentgateway
-- v1.7.0's Messages path never reads it, so every Claude request on Messages logged none.
--
-- - spend_entries.reasoning_known: true when the gateway logged the split (koinon.reasoning) or a
--   settlement took it from the provider's record (OpenRouter's native_tokens_reasoning), false
--   when neither did, so reasoning_tokens is 0 for want of a figure. NULL for entries written
--   before this migration, or by a binary of the same minor line that predates it: those read as
--   known when reasoning_tokens > 0, the rule the spend report used until now.
--
-- Expand only (ADR 0085): one new nullable column; reasoning_tokens is unchanged.

-- +goose Up

ALTER TABLE spend_entries ADD COLUMN reasoning_known boolean;
