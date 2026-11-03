-- SPDX-License-Identifier: Apache-2.0
--
-- The leak guard's state (ADR 0084, ADR 0091; #349): sessions per home generation with their legs,
-- the taint store of fingerprints, and the budgets of a run. Every query takes @org_id and filters
-- on it (TestQueriesAreOrgScoped).

-- name: EnsureLeakGuardSession :one
-- The session of one home generation, created with @fp_key the first time. A concurrent first call
-- keeps the key the first insert wrote.
INSERT INTO leak_guard_sessions (org_id, agent_id, generation, fp_key)
VALUES (@org_id, @agent_id, @generation, @fp_key)
ON CONFLICT (org_id, agent_id, generation) DO UPDATE SET org_id = leak_guard_sessions.org_id
RETURNING *;

-- name: GetLeakGuardSession :one
SELECT * FROM leak_guard_sessions
WHERE org_id = @org_id AND agent_id = @agent_id AND generation = @generation;

-- name: AddLeakGuardLegs :one
-- The legs only grow: a leg already set stays set whatever @untrusted_input and @private_data say.
UPDATE leak_guard_sessions
SET untrusted_input = untrusted_input OR @untrusted_input::boolean,
    private_data    = private_data OR @private_data::boolean,
    updated_at      = now()
WHERE org_id = @org_id AND agent_id = @agent_id AND generation = @generation
RETURNING *;

-- name: MarkLeakGuardEvicted :exec
UPDATE leak_guard_sessions
SET evicted = true, updated_at = now()
WHERE org_id = @org_id AND agent_id = @agent_id AND generation = @generation;

-- name: AddLeakGuardFingerprints :execrows
-- Adding a fingerprint again makes it new, so the eviction keeps what the session saw last.
INSERT INTO leak_guard_fingerprints (org_id, agent_id, generation, fp, added_at)
SELECT @org_id, @agent_id, @generation, f, now()
FROM unnest(@fps::bigint[]) AS f
ON CONFLICT (org_id, agent_id, generation, fp) DO UPDATE SET added_at = EXCLUDED.added_at;

-- name: CountLeakGuardFingerprints :one
SELECT count(*) FROM leak_guard_fingerprints
WHERE org_id = @org_id AND agent_id = @agent_id AND generation = @generation;

-- name: EvictLeakGuardFingerprints :execrows
-- Drops the @n oldest fingerprints of a session.
DELETE FROM leak_guard_fingerprints d
WHERE d.org_id = @org_id AND d.agent_id = @agent_id AND d.generation = @generation
  AND d.fp IN (
    SELECT o.fp FROM leak_guard_fingerprints o
    WHERE o.org_id = @org_id AND o.agent_id = @agent_id AND o.generation = @generation
    ORDER BY o.added_at, o.fp
    LIMIT @n);

-- name: LookupLeakGuardFingerprints :many
-- The fingerprints of @fps the session holds.
SELECT DISTINCT fp FROM leak_guard_fingerprints
WHERE org_id = @org_id AND agent_id = @agent_id AND generation = @generation
  AND fp = ANY (@fps::bigint[]);

-- name: DeleteLeakGuardSession :exec
DELETE FROM leak_guard_sessions
WHERE org_id = @org_id AND agent_id = @agent_id AND generation = @generation;

-- name: LockLeakGuardRun :one
-- The budgets of a run, locked for the rest of the transaction so two replicas charging one run at
-- once count both calls.
INSERT INTO leak_guard_runs (org_id, run_id)
VALUES (@org_id, @run_id)
ON CONFLICT (org_id, run_id) DO UPDATE SET org_id = leak_guard_runs.org_id
RETURNING *;

-- name: SaveLeakGuardRun :exec
UPDATE leak_guard_runs
SET capped_bytes = @capped_bytes, url_state = @url_state, updated_at = now()
WHERE org_id = @org_id AND run_id = @run_id;
