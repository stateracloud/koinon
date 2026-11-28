-- SPDX-License-Identifier: Apache-2.0
--
-- The actor names that the Substrate driver sent to CreateActor (issue #1092). atenet-egress caches an actor
-- JWT by atespace and actor name (Substrate v1.0.1 actorjwt.go), so a new actor under the name of a deleted
-- actor got the deleted actor's token (lab, rc.33). The gateway refuses that token by its actor UID (403
-- unknown_actor), so the harm is lost egress for the new actor, not a leak of identity. This ledger is a
-- second layer next to the random name: the driver claims each name here before CreateActor and refuses a name
-- that is here already. The record outlives a restart and every replica shares it.
--
--   * atespace, name: the Substrate atespace (the org's workspace) and the actor name.
--   * claimed_at: when the first create claimed the name (the database clock).
--
-- Growth: one row per create attempt, bounded by the org creation rate of sandboxes. A row is never removed
-- here; a prune by claimed_at is a separate decision.
--
-- The backfill records only the current actor_name of each agent_sandboxes row. Older names are not recorded:
-- the 64-bit random suffix of each name ("<sandbox_name>-<16 hex>"; 00118 says 8 hex, which is wrong) covers
-- them.
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
