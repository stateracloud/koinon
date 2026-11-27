-- SPDX-License-Identifier: Apache-2.0
--
-- Contract step of 00112 (issue #1393). Migration 00112 added the trigger
-- task_events_prompt_by_old_rule. It gave the prompt messages of a binary from before 00112 the
-- prompt mark. No such binary can run against this schema: there is no upgrade promise before v0.3
-- (ADR 0085), so v0.1.0 starts with a clean schema. Every writer sets the column prompt itself.
--
-- This migration drops the trigger and its function. The column prompt stays. The marker
-- hack/migrate-patch-line is none, so a contract step is allowed.

-- +goose Up

DROP TRIGGER task_events_prompt_by_old_rule ON task_events;
DROP FUNCTION task_events_prompt_by_old_rule();
