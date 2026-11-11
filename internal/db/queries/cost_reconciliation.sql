-- SPDX-License-Identifier: Apache-2.0
--
-- The daily reconciliation of the ledger against the direct providers' cost APIs (issue #47, ADR
-- 0056 amendment 2026-11-06). Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped), except the one listed in crossOrgQueries.

-- name: ListCostReportProviders :many
-- Every provider of every org with a cost API key, in a type that has a cost source; the
-- reconciliation asks each of them once a day.
SELECT org_id, id, type, url, cost_api_key, cost_scope::text AS cost_scope,
       cost_reconcile_since::timestamptz AS cost_reconcile_since
FROM providers
WHERE cost_api_key IS NOT NULL AND cost_scope IS NOT NULL AND removed_at IS NULL AND admin_disabled_at IS NULL
  AND type = ANY(@types::text[])
ORDER BY org_id, id;

-- name: SetProviderCostKey :one
-- Sets, replaces or (both NULL) removes a provider's cost API key and scope. Writing the key ends a
-- lost one. The scope can change alone (@set_key false keeps the envelope). A key stored where none
-- was starts cost_reconcile_since; a replaced key keeps it; a removed key clears it.
UPDATE providers
SET cost_api_key = CASE WHEN @set_key::boolean THEN sqlc.narg(cost_api_key) ELSE cost_api_key END,
    cost_reconcile_since = CASE
        WHEN NOT @set_key::boolean THEN cost_reconcile_since
        WHEN sqlc.narg(cost_api_key)::jsonb IS NULL THEN NULL
        WHEN cost_api_key IS NULL THEN now()
        ELSE cost_reconcile_since END,
    cost_api_key_lost_at = CASE WHEN @set_key::boolean THEN NULL ELSE cost_api_key_lost_at END,
    cost_scope = sqlc.narg(cost_scope)
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: ProviderLedgerForDay :one
-- The ledger's cost of one provider's entries in [@since, @until), with how many there were.
SELECT coalesce(sum(cost_usd), 0)::text AS cost_usd, count(*)::bigint AS entries
FROM spend_entries
WHERE org_id = @org_id AND provider_id = @provider_id
  AND occurred_at >= @since AND occurred_at < @until;

-- name: ListProviderCostReconciliations :many
-- A provider's reconciled days from @since, newest first.
SELECT * FROM provider_cost_reconciliations
WHERE org_id = @org_id AND provider_id = @provider_id AND day >= @since
ORDER BY day DESC
LIMIT @max_rows;

-- name: UpsertProviderCostReconciliation :one
-- Records (or, on a later pass, replaces) the reconciliation of one provider's day.
INSERT INTO provider_cost_reconciliations (
    org_id, provider_id, day, provider_type, ledger_usd, entries, provider_usd, diff_usd, status, error, checked_at
) VALUES (
    @org_id, @provider_id, @day, @provider_type, (sqlc.arg(ledger_usd)::text)::numeric, @entries,
    (sqlc.narg(provider_usd)::text)::numeric, (sqlc.narg(diff_usd)::text)::numeric, @status, sqlc.narg(error), @checked_at
)
ON CONFLICT (org_id, provider_id, day) DO UPDATE
SET provider_type = excluded.provider_type, ledger_usd = excluded.ledger_usd, entries = excluded.entries,
    provider_usd = excluded.provider_usd, diff_usd = excluded.diff_usd, status = excluded.status,
    error = excluded.error, checked_at = excluded.checked_at,
    attempts = provider_cost_reconciliations.attempts + 1
RETURNING *;
