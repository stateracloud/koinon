-- SPDX-License-Identifier: Apache-2.0
--
-- The ledger of Substrate actor names (issue #1092), written by the Substrate driver in koinon-reconciler
-- and koinon-agentd through package actornames.

-- name: ClaimSubstrateActorName :execrows
-- Claims a Substrate actor name before the driver creates the actor. One row: the name is new. No row: a
-- create claimed it before, and the driver refuses it. A claim is never removed.
INSERT INTO substrate_actor_names (atespace, name)
VALUES (@atespace::text, @name::text)
ON CONFLICT DO NOTHING;
