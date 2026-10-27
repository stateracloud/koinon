-- SPDX-License-Identifier: Apache-2.0
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: ClaimDedupeKey :one
-- Records the key as seen. Returns no row when it was seen before: the caller then has no second
-- effect. A concurrent claim of the same key waits for the first transaction, so exactly one
-- claims it; if that one rolls back, the waiting one claims it.
INSERT INTO dedupe_records (org_id, source, key)
VALUES (@org_id, @source, @key)
ON CONFLICT (org_id, source, key) DO NOTHING
RETURNING *;

-- name: SetDedupeEffect :exec
-- Names what a claimed key produced, in the same transaction as the claim.
UPDATE dedupe_records
SET effect_kind = @effect_kind, effect_id = @effect_id
WHERE org_id = @org_id AND source = @source AND key = @key;

-- name: GetDedupeRecord :one
SELECT * FROM dedupe_records
WHERE org_id = @org_id AND source = @source AND key = @key;

-- name: DeleteDedupeRecordsBefore :execrows
-- Sweeps records older than the cutoff; sources keep redelivering for a bounded time only.
DELETE FROM dedupe_records
WHERE org_id = @org_id AND created_at < @before;
