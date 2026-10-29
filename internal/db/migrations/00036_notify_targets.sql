-- SPDX-License-Identifier: Apache-2.0
--
-- Notify targets for triggers, several recipients per question, and inbox ageing (issue #182; ADR
-- 0065 and 0066 amendments 2026-10-28). A task that no person created asked "to" nobody; now the
-- trigger names who is asked (a person or a role), a question is addressed to a target resolved to
-- its recipients when it is asked, and an inbox item ages from when it started waiting on a person.

-- +goose Up

-- Who is asked when a task was not created by a person (ADR 0065/0066 amendments 2026-10-28):
-- a person, everyone with at least an org role, or (from v0.2) a chat channel.
CREATE TYPE notify_target_kind AS ENUM ('user', 'role', 'channel');

-- The task's notify target, copied from the trigger that created it. NULL kind: the org's
-- default at the time of the question (today the org's admins and owners).
ALTER TABLE tasks
    ADD COLUMN notify_kind       notify_target_kind,
    ADD COLUMN notify_user_id    uuid REFERENCES users (id),
    ADD COLUMN notify_role       org_role,
    -- v0.2: the chat channel, a row of the v0.2 channels table keyed (org_id, id uuid) like every
    -- org-owned table; v0.2 adds the composite FK (org_id, notify_channel_id). uuid now, so that FK
    -- needs no type change. Always NULL until chat connectors exist (the API refuses the kind).
    ADD COLUMN notify_channel_id uuid,
    ADD CONSTRAINT tasks_notify_target CHECK (
        (notify_kind IS NULL AND notify_user_id IS NULL AND notify_role IS NULL AND notify_channel_id IS NULL)
     OR (notify_kind = 'user' AND notify_user_id IS NOT NULL AND notify_role IS NULL AND notify_channel_id IS NULL)
     OR (notify_kind = 'role' AND notify_role IN ('member', 'admin', 'owner') AND notify_user_id IS NULL AND notify_channel_id IS NULL)
     OR (notify_kind = 'channel' AND notify_channel_id IS NOT NULL AND notify_user_id IS NULL AND notify_role IS NULL));

-- The question's effective target: who it was addressed to, as resolved when it was asked.
ALTER TABLE questions
    ADD COLUMN target_kind       notify_target_kind,
    ADD COLUMN target_user_id    uuid REFERENCES users (id),
    ADD COLUMN target_role       org_role,
    ADD COLUMN target_channel_id uuid;

UPDATE questions SET target_kind = 'user', target_user_id = asked_to;

ALTER TABLE questions
    ALTER COLUMN target_kind SET NOT NULL,
    ADD CONSTRAINT questions_target CHECK (
        (target_kind = 'user' AND target_user_id IS NOT NULL AND target_role IS NULL AND target_channel_id IS NULL)
     OR (target_kind = 'role' AND target_role IN ('member', 'admin', 'owner') AND target_user_id IS NULL AND target_channel_id IS NULL)
     OR (target_kind = 'channel' AND target_channel_id IS NOT NULL AND target_user_id IS NULL AND target_role IS NULL));

-- Each person a question waits on. Recipients are resolved when the question is asked: a person
-- who joins a role later is not added. waiting_since is when the question became visible to that
-- person (opened, or added later); a reassignment marks the old row removed and adds a new one,
-- so the age restarts for the new person. Reminders do not change it.
CREATE TABLE question_recipients (
    org_id        uuid        NOT NULL REFERENCES orgs (id),
    id            uuid        NOT NULL DEFAULT uuidv7(),
    question_id   uuid        NOT NULL,
    user_id       uuid        NOT NULL REFERENCES users (id),
    -- NULL while the question is still 'asked' (invisible).
    waiting_since timestamptz,
    removed_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, question_id) REFERENCES questions (org_id, id)
);
CREATE UNIQUE INDEX question_recipients_active ON question_recipients (org_id, question_id, user_id) WHERE removed_at IS NULL;
-- The inbox: what waits on a person.
CREATE INDEX question_recipients_inbox ON question_recipients (org_id, user_id, waiting_since) WHERE removed_at IS NULL;

INSERT INTO question_recipients (org_id, question_id, user_id, waiting_since, created_at)
SELECT org_id, id, asked_to, opened_at, created_at FROM questions;

-- Expand, then contract (the ADR 0056 precedent): asked_to stays for one release, nullable and
-- deprecated, so the old pods of a rolling upgrade keep working. New code writes it for a user
-- target (NULL otherwise) and never reads it. The two triggers translate an old pod's insert (it
-- sets asked_to and no target). A migration after v0.1.0 drops both triggers, the function and
-- the column.
ALTER TABLE questions ALTER COLUMN asked_to DROP NOT NULL;
COMMENT ON COLUMN questions.asked_to IS 'Deprecated (00036): read target_* and question_recipients. Dropped after v0.1.0.';

-- +goose StatementBegin
CREATE FUNCTION questions_target_from_asked_to() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND TG_WHEN = 'BEFORE' THEN
        IF NEW.target_kind IS NULL AND NEW.asked_to IS NOT NULL THEN
            NEW.target_kind := 'user';
            NEW.target_user_id := NEW.asked_to;
        END IF;
        RETURN NEW;
    END IF;
    -- AFTER INSERT: an old pod adds no recipient row. New code adds the same row itself; the
    -- partial unique index makes the second insert a no-op.
    INSERT INTO question_recipients (org_id, question_id, user_id)
    VALUES (NEW.org_id, NEW.id, NEW.asked_to)
    ON CONFLICT DO NOTHING;
    RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER questions_target_compat BEFORE INSERT ON questions
    FOR EACH ROW EXECUTE FUNCTION questions_target_from_asked_to();
CREATE TRIGGER questions_recipient_compat AFTER INSERT ON questions
    FOR EACH ROW WHEN (NEW.asked_to IS NOT NULL) EXECUTE FUNCTION questions_target_from_asked_to();
