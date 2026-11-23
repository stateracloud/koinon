-- SPDX-License-Identifier: Apache-2.0
--
-- The optional org setting "the approver must not be the requester" (issue #1175, ADR 0005
-- amendment 2026-11-23). When it is on, the person who created a task cannot approve the task's
-- calls, and an approval scope does not cover the calls of tasks its creator requested. Expand
-- only (ADR 0085): one new column with a default.

-- +goose Up

ALTER TABLE org_settings
    ADD COLUMN approver_not_requester boolean NOT NULL DEFAULT false;

