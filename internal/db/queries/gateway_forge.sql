-- SPDX-License-Identifier: Apache-2.0
--
-- When koinon-gateway serves an agent's forge credential (#1086 part C2). Org-owned.

-- name: SetGatewayAgentForge :exec
-- Records what the agent expects of its forge credential (#1086 part C2). A new revision or cause
-- clears served_at; the same values change nothing.
INSERT INTO gateway_agent_forge (org_id, agent_id, state_revision, failure)
VALUES (@org_id, @agent_id, sqlc.narg(state_revision), @failure)
ON CONFLICT (org_id, agent_id) DO UPDATE
SET state_revision = EXCLUDED.state_revision,
    failure = EXCLUDED.failure,
    served_at = NULL,
    updated_at = now()
WHERE gateway_agent_forge.state_revision IS DISTINCT FROM EXCLUDED.state_revision
   OR gateway_agent_forge.failure IS DISTINCT FROM EXCLUDED.failure;

-- name: MarkGatewayAgentForgeServed :exec
-- Every live copy acked the revision that holds the agent's credential or actor. A row with a failure may be
-- marked too: it then proves only the actor revision (agentd ignores the failure when the run needs no credential).
UPDATE gateway_agent_forge
SET served_at = now(), updated_at = now()
WHERE org_id = @org_id AND agent_id = @agent_id AND state_revision = @state_revision
  AND served_at IS NULL;

-- name: DeleteGatewayAgentForge :exec
-- The agents of the org that no longer expect a forge credential.
DELETE FROM gateway_agent_forge
WHERE org_id = @org_id AND agent_id = ANY(@agent_ids::uuid[]);

-- name: GetGatewayAgentForge :one
SELECT state_revision, served_at, failure
FROM gateway_agent_forge
WHERE org_id = @org_id AND agent_id = @agent_id;
