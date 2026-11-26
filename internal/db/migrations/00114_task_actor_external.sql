-- SPDX-License-Identifier: Apache-2.0
--
-- The actor kind `external`: a person who writes to the Koinon bot in Slack or Teams and has no
-- linked Koinon account (ADR 0130, issue #1408). The value is alone in this migration because
-- Postgres does not let a transaction use an enum value that the same transaction added; 00115
-- adds the table and the checks that use it.

-- +goose Up

-- Expand only (ADR 0085): old binaries never write the value.
ALTER TYPE task_actor_kind ADD VALUE 'external';
