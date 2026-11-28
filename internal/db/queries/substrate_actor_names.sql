-- SPDX-License-Identifier: Apache-2.0
--
-- The ledger of Substrate actor names (issue #1092), written by the Substrate driver in koinon-reconciler
-- through package actornames.

-- name: ClaimSubstrateActorName :one
-- Claims a Substrate actor name before the driver creates the actor, for the org whose workspace is the
-- atespace. known is false when no org has that workspace: nothing is claimed. claimed is true when the name
-- is new, and false when a create claimed it before (the driver refuses it). A claim goes only with its org.
WITH ws AS (
    SELECT w.org_id FROM org_workspaces w WHERE w.workspace = @atespace::text
), ins AS (
    INSERT INTO substrate_actor_names (org_id, atespace, name)
    SELECT ws.org_id, @atespace::text, @name::text FROM ws
    ON CONFLICT DO NOTHING
    RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM ws) AS known, EXISTS (SELECT 1 FROM ins) AS claimed;
