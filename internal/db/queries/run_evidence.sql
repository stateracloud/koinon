-- SPDX-License-Identifier: Apache-2.0
--
-- Lab run evidence (issue #926): the leak guard's decision records and what an export reads. Every
-- query takes @org_id and filters on it (TestQueriesAreOrgScoped).

-- name: InsertLeakGuardDecision :exec
-- One decision of the MCP leak guard. The agent's running run is read in the same statement, so a
-- decision costs one round trip. No argument, payload or matched text is stored.
INSERT INTO leak_guard_decisions (
    org_id, agent_id, generation, run_id, server, tool, sink_class, action, reason, reasons,
    match_count, match_length, match_encoding, payload_bytes, untrusted_input, private_data)
VALUES (
    @org_id, @agent_id, @generation,
    (SELECT r.id FROM runs r WHERE r.org_id = @org_id AND r.agent_id = @agent_id AND r.state = 'running'),
    @server, @tool, @sink_class, @action, @reason, @reasons::text[],
    @match_count, @match_length, @match_encoding, @payload_bytes, @untrusted_input, @private_data);

-- name: ListRunEvidenceDecisions :many
SELECT * FROM leak_guard_decisions
WHERE org_id = @org_id AND run_id = @run_id
ORDER BY created_at, id;

-- name: SumRunEvidenceOutbound :many
-- The bytes that left the org for each external sink: the payload of every external call the guard
-- passed, per run, per home generation and per MCP server. A call refused, or
-- parked for a person's approval (action 'approve'), sent nothing at that moment.
SELECT generation, server, sink_class, count(*)::bigint AS calls, sum(payload_bytes)::bigint AS payload_bytes
FROM leak_guard_decisions
WHERE org_id = @org_id AND run_id = @run_id AND action = 'pass'
  AND sink_class IN ('external-low', 'external-high')
GROUP BY generation, server, sink_class
ORDER BY generation, server, sink_class;

-- name: GetRunEvidenceRun :one
SELECT * FROM runs WHERE org_id = @org_id AND id = @run_id;

-- name: ListRunEvidenceTurns :many
SELECT * FROM run_turns WHERE org_id = @org_id AND run_id = @run_id ORDER BY seq;

-- name: ListRunEvidenceEvents :many
-- Pages of the run's events in write order.
SELECT * FROM run_events
WHERE org_id = @org_id AND run_id = @run_id AND seq > @after_seq
ORDER BY seq
LIMIT @page_size;

-- name: GetRunEvidenceSandbox :one
SELECT * FROM agent_sandboxes WHERE org_id = @org_id AND agent_id = @agent_id;

-- name: SetAgentSandboxEnforcement :exec
-- The network enforcement mode observed in the sandbox's policy (ADR 0045): enforce, audit, none or
-- unknown. Kept apart from RecordAgentSandbox, which many paths call without an observed sandbox.
UPDATE agent_sandboxes
SET enforcement = @enforcement
WHERE org_id = @org_id AND agent_id = @agent_id AND enforcement IS DISTINCT FROM @enforcement;

-- name: SumGenerationEvidenceOutbound :many
-- The same sums for one home generation of an agent, over every run that was in it.
SELECT server, sink_class, count(*)::bigint AS calls, sum(payload_bytes)::bigint AS payload_bytes
FROM leak_guard_decisions
WHERE org_id = @org_id AND agent_id = @agent_id AND generation = @generation
  AND action = 'pass' AND sink_class IN ('external-low', 'external-high')
GROUP BY server, sink_class
ORDER BY server, sink_class;
