-- SPDX-License-Identifier: Apache-2.0
--
-- Reminders for questions (plan item TK4; ADR 0065 decision 10). A question gets an in-app reminder
-- for the person asked at remind_at (24 hours after it was asked by default; per agent or
-- template). Expiry already has expires_at (TK3). Both are set when the question is asked, from the
-- agent's active version, so a later change of the version does not move a question's deadlines.

-- +goose Up

ALTER TABLE questions
    -- NULL: no reminder for this question.
    ADD COLUMN remind_at         timestamptz,
    -- The reminder went out (set by the handler of the reminder notification).
    ADD COLUMN reminded_at       timestamptz,
    -- Failed deliveries of the reminder; at the limit the handler gives up and sets reminded_at.
    ADD COLUMN remind_failures   int NOT NULL DEFAULT 0,
    ADD CONSTRAINT questions_remind_before_expiry CHECK (remind_at IS NULL OR remind_at < expires_at);

-- Questions asked before this migration get the default reminder when it still lies ahead of expiry.
UPDATE questions SET remind_at = created_at + interval '24 hours'
WHERE state IN ('asked', 'open') AND created_at + interval '24 hours' < expires_at;

-- Reminders still to send.
CREATE INDEX questions_remind_due ON questions (org_id, remind_at) WHERE state = 'open' AND remind_at IS NOT NULL AND reminded_at IS NULL;
