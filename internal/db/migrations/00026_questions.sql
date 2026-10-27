-- SPDX-License-Identifier: Apache-2.0
--
-- Questions (plan item TK3; ADR 0065 decision 8 and 10a): what an agent asks a person with the
-- ask_user tool. A question is created 'asked' by the tool while the agent's turn runs; it is
-- invisible until the turn ends, when agentd opens it in the same transaction that parks the run
-- (reason input_required) and moves the task to input-required. A person's answer, a dismissal or
-- an expiry resolves it and the run's park; every question carries the safe default that dismissal
-- and expiry apply (10a). TK4 adds reminders and the expiry sweep on this table.

-- +goose Up

CREATE TYPE question_state AS ENUM ('asked', 'open', 'answered', 'dismissed', 'expired', 'canceled');
-- 'canceled': the run ended (canceled, failed, finished) while the question was still asked or open,
-- so nobody can answer it any more.

-- How the question was resolved: a listed choice, the draft as offered, free text, or the safe
-- default (dismissed or expired).
CREATE TYPE question_answer_kind AS ENUM ('choice', 'draft', 'text', 'default');

CREATE TABLE questions (
    org_id         uuid                 NOT NULL REFERENCES orgs (id),
    id             uuid                 NOT NULL DEFAULT uuidv7(),
    task_id        uuid                 NOT NULL,
    run_id         uuid                 NOT NULL,
    agent_id       uuid                 NOT NULL,
    -- The person who is asked: a member of the org when the question was made.
    asked_to       uuid                 NOT NULL REFERENCES users (id),
    question       text                 NOT NULL CHECK (length(question) BETWEEN 1 AND 4000),
    -- A JSON array of the answers offered as buttons; empty means free text only.
    choices        jsonb                NOT NULL DEFAULT '[]',
    -- Text the agent proposes (a reply to post, a PR title); the person can accept or edit it.
    draft          text                 CHECK (draft IS NULL OR length(draft) <= 8000),
    -- Used when the question is dismissed or expires; the agent is told it was applied.
    default_answer text                 NOT NULL CHECK (length(default_answer) BETWEEN 1 AND 4000),
    state          question_state       NOT NULL DEFAULT 'asked',
    answer         text,
    answer_kind    question_answer_kind,
    -- The person who resolved it; NULL for an expiry.
    answered_by    uuid                 REFERENCES users (id),
    -- Hash of the call: the same question asked again in the same run is the one question.
    call_sha256    text                 NOT NULL CHECK (call_sha256 ~ '^[0-9a-f]{64}$'),
    -- After this the question expires and the default applies (ADR 0065 decision 10: 7 days).
    expires_at     timestamptz          NOT NULL,
    opened_at      timestamptz,
    -- The in-app notification (and later chat messages) went out for this question.
    notified_at    timestamptz,
    -- Failed deliveries of the notification; at the limit the handler gives up and sets notified_at,
    -- so a question whose notification cannot be written is not retried by every sweep forever.
    notify_failures int NOT NULL DEFAULT 0,
    resolved_at    timestamptz,
    -- The outcome was sent to the agent in a prompt.
    delivered_at   timestamptz,
    created_at     timestamptz          NOT NULL DEFAULT now(),
    updated_at     timestamptz          NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, run_id, call_sha256),
    FOREIGN KEY (org_id, task_id, run_id) REFERENCES runs (org_id, task_id, id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
    CHECK (jsonb_typeof(choices) = 'array'),
    CHECK ((state IN ('answered', 'dismissed', 'expired', 'canceled')) = (resolved_at IS NOT NULL)),
    CHECK ((state IN ('answered', 'dismissed', 'expired')) = (answer IS NOT NULL)),
    CHECK (state <> 'asked' OR opened_at IS NULL),
    CHECK (state NOT IN ('open', 'answered', 'dismissed', 'expired') OR opened_at IS NOT NULL),
    -- A question that was never notified and is not in a state a person acted on.
    CHECK (state <> 'canceled' OR answered_by IS NULL)
);

-- The inbox: what waits on a person.
CREATE INDEX questions_open_to ON questions (org_id, asked_to, created_at) WHERE state = 'open';
-- The run's questions (agentd's park and the prompt).
CREATE INDEX questions_run ON questions (org_id, run_id, created_at);
-- Expiry and reminders (TK4).
CREATE INDEX questions_open_expiry ON questions (org_id, expires_at) WHERE state = 'open';
-- Notifications still to send.
CREATE INDEX questions_unnotified ON questions (org_id, opened_at) WHERE state IN ('open', 'answered', 'dismissed', 'expired') AND notified_at IS NULL;

CREATE TRIGGER questions_set_updated_at BEFORE UPDATE ON questions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
