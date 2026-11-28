-- SPDX-License-Identifier: Apache-2.0
--
-- The actor names that the Substrate driver sent to CreateActor (issue #1092). atenet-egress caches an actor
-- JWT by atespace and actor name (Substrate v1.0.1 actorjwt.go), so a new actor under the name of a deleted
-- actor got the deleted actor's token (lab, rc.33). The driver claims each name here before CreateActor and
-- refuses a name that is here already. The record outlives a restart and is shared by every replica.
--
--   * org_id: the org whose workspace is the atespace (org_workspaces.workspace).
--   * atespace, name: the Substrate atespace and the actor name. Unique.
--   * claimed_at: when the first create claimed the name (the database clock).
--
-- A row stays while its org exists, and goes with the org: the atespace name derives from the org ID, which
-- is never used again, so no actor can be created in that atespace after the org is gone.
--
-- The backfill records the actor names that agent_sandboxes holds now, so a name that the reconciler recorded
-- before this migration is not created again.
--
-- Expand only (ADR 0085): a new table.

-- +goose Up

CREATE TABLE substrate_actor_names (
    org_id     uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    -- Substrate checks the format of both names; a name it refuses is still claimed, so only the length is
    -- bounded here.
    atespace   text        NOT NULL CHECK (length(atespace) BETWEEN 1 AND 253),
    name       text        NOT NULL CHECK (length(name) BETWEEN 1 AND 253),
    claimed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, atespace, name),
    UNIQUE (atespace, name)
);

INSERT INTO substrate_actor_names (org_id, atespace, name)
SELECT org_id, workspace, actor_name FROM agent_sandboxes
WHERE actor_name IS NOT NULL
ON CONFLICT DO NOTHING;
