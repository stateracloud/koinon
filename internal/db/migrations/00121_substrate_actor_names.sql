-- SPDX-License-Identifier: Apache-2.0
--
-- The actor names that the Substrate driver sent to CreateActor (issue #1092). atenet-egress caches an actor
-- JWT by atespace and actor name (Substrate v1.0.1 actorjwt.go), so a new actor under the name of a deleted
-- actor got the deleted actor's token (lab, rc.33). The driver claims each name here before CreateActor and
-- refuses a name that is here already. The record outlives a restart and is shared by every replica.
--
--   * atespace, name: the Substrate atespace (the org's workspace) and the actor name.
--   * claimed_at: when the first create claimed the name (the database clock).
--
-- A row is never removed, also not with its org: it is the proof that a name was used. The table has no
-- foreign key to the org for that reason. One row per actor ever created.
--
-- The backfill records the actor names that agent_sandboxes holds now, so a name that the reconciler recorded
-- before this migration is not created again.
--
-- Expand only (ADR 0085): a new table.

-- +goose Up

CREATE TABLE substrate_actor_names (
    -- Substrate checks the format of both names; a name it refuses is still claimed, so only the length is
    -- bounded here.
    atespace   text        NOT NULL CHECK (length(atespace) BETWEEN 1 AND 253),
    name       text        NOT NULL CHECK (length(name) BETWEEN 1 AND 253),
    claimed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (atespace, name)
);

INSERT INTO substrate_actor_names (atespace, name)
SELECT workspace, actor_name FROM agent_sandboxes
WHERE actor_name IS NOT NULL
ON CONFLICT DO NOTHING;
