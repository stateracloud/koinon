-- SPDX-License-Identifier: Apache-2.0
--
-- The identity of an `external` actor (ADR 0130, issue #1408): one row for each person who writes
-- to the bot from the connection's Slack workspace or Teams tenant without a linked Koinon account.
-- The row holds only the platform, the team and the platform user ID: never a display name, an
-- email address or other profile data. A task event by such a person names the row.
--
-- An external actor can only write messages: the check on task_events refuses every other kind, so
-- no approval, answer or state change can name one. Each of their messages is part of the run's
-- prompt: a writer that forgets the prompt flag fails, and the agent never misses their words.
--
-- Expand only (ADR 0085): a new table, a new nullable column and checks that every old row passes.

-- +goose Up

CREATE TABLE external_actors (
    org_id           uuid        NOT NULL REFERENCES orgs (id),
    id               uuid        NOT NULL DEFAULT uuidv7(),
    platform         text        NOT NULL CHECK (platform IN ('slack', 'teams')),
    -- The Slack team ID or the Teams tenant ID of the workspace in which the person wrote.
    team             text        NOT NULL CHECK (team <> ''),
    -- The Slack user ID or the Teams user ID.
    platform_user_id text        NOT NULL CHECK (platform_user_id <> ''),
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, platform, team, platform_user_id)
);

ALTER TABLE task_events
    ADD COLUMN actor_external_id uuid,
    ADD CONSTRAINT task_events_actor_external_fkey
        FOREIGN KEY (org_id, actor_external_id) REFERENCES external_actors (org_id, id),
    ADD CONSTRAINT task_events_actor_external_check
        CHECK ((actor_kind = 'external') = (actor_external_id IS NOT NULL)),
    ADD CONSTRAINT task_events_external_message_check
        CHECK (actor_kind <> 'external' OR kind = 'message'),
    ADD CONSTRAINT task_events_external_prompt_check
        CHECK (actor_kind <> 'external' OR prompt);
