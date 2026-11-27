-- SPDX-License-Identifier: Apache-2.0
--
-- The actor of a sandbox (issue #1386, ADR 0120 amendment of 2026-11-26, items 5 and 6). On Agent Substrate
-- the sandbox is an actor, and the egress layer adds a JWT that Substrate signs for the actor name and UID.
-- The gateway finds the org and the agent in this record, never in the token.
--
--   * actor_name: the name of the sandbox's actor, "<sandbox_name>-<8 hex>". The reconciler makes a new name
--     for each new actor and never uses one again: atenet-egress keeps a token for each actor name. The
--     sandbox_name CHECK allows only 19 characters, so the name has its own column. NULL: the driver names
--     the sandbox sandbox_name (OpenShell), or no actor was named yet.
--   * actor_uid: the UID that Substrate gave the actor named actor_name, once the reconciler observed it.
--     NULL while no actor with that name was observed.
--
-- Expand only (ADR 0085): two new nullable columns.

-- +goose Up

ALTER TABLE agent_sandboxes
    ADD COLUMN actor_name text CHECK (actor_name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    ADD COLUMN actor_uid  text,
    -- A UID belongs to a named actor.
    ADD CONSTRAINT agent_sandboxes_actor_uid_named CHECK (actor_uid IS NULL OR actor_name IS NOT NULL);
