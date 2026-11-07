-- SPDX-License-Identifier: Apache-2.0
--
-- The platform price list and the price proposals (ADR 0098). None of these tables belongs to an org.

-- name: LockPlatformPrices :exec
-- Serializes writers of platform price versions and proposals' decisions: a transaction holds it to
-- the commit. Version numbers are max + 1, so two writers must not interleave.
SELECT pg_advisory_xact_lock(hashtext('koinon.platform_prices'));

-- name: GetPlatformPriceVersionAt :one
-- The platform price version valid at a time: the latest valid_from not after it.
SELECT * FROM platform_price_versions WHERE valid_from <= @at ORDER BY valid_from DESC, version DESC LIMIT 1;

-- name: GetPlatformPriceVersion :one
SELECT * FROM platform_price_versions WHERE version = @version;

-- name: LatestPlatformPriceVersion :one
-- The newest written version, valid or not yet; 0 when none was written.
SELECT COALESCE(MAX(version), 0)::bigint FROM platform_price_versions;

-- name: ListPlatformPriceVersions :many
SELECT * FROM platform_price_versions ORDER BY version DESC LIMIT @max_rows;

-- name: InsertPlatformPriceVersion :one
-- valid_from NULL means now; a value before created_at violates the no-backdating check. The clock
-- is read once, after the writer holds the platform lock, so versions are written in time order.
WITH t AS (SELECT clock_timestamp() AS ts)
INSERT INTO platform_price_versions (version, created_at, valid_from, actor, reason, rule_set_version, proposal_ids, restored_from)
SELECT (SELECT COALESCE(MAX(version), 0) + 1 FROM platform_price_versions), t.ts, COALESCE(sqlc.narg(valid_from)::timestamptz, t.ts),
       @actor, @reason, @rule_set_version, @proposal_ids::uuid[], sqlc.narg(restored_from)::bigint
FROM t
RETURNING *;

-- name: InsertPlatformPriceEntry :exec
INSERT INTO platform_price_entries (version, entry_id, entry, reviewed, provenance)
VALUES (@version, @entry_id, @entry, @reviewed, @provenance);

-- name: ListPlatformPriceEntries :many
SELECT * FROM platform_price_entries WHERE version = @version ORDER BY entry_id;

-- name: InsertPriceSourceFetch :one
INSERT INTO price_source_fetches (source, url, fetched_at, result, error, sha256, etag, commit_sha, model_count, trigger)
VALUES (@source, @url, @fetched_at, @result, @error, @sha256, @etag, @commit_sha, @model_count, @trigger)
RETURNING *;

-- name: GetLastGoodPriceSourceFetch :one
SELECT * FROM price_source_fetches WHERE source = @source AND result = 'ok' ORDER BY fetched_at DESC LIMIT 1;

-- name: ListPriceSourceFetches :many
SELECT * FROM price_source_fetches ORDER BY fetched_at DESC LIMIT @max_rows;

-- name: ObservePriceValue :one
-- Records that a fetch returned values_hash for the model. The same hash as before extends the run
-- of unchanged fetches; a different one starts a new run.
INSERT INTO price_source_observations (source, model, values_hash, first_seen, last_seen, fetches)
VALUES (@source, @model, @values_hash, @at, @at, 1)
ON CONFLICT (source, model) DO UPDATE SET
    first_seen  = CASE WHEN price_source_observations.values_hash = EXCLUDED.values_hash THEN price_source_observations.first_seen ELSE EXCLUDED.first_seen END,
    fetches     = CASE WHEN price_source_observations.values_hash = EXCLUDED.values_hash THEN price_source_observations.fetches + 1 ELSE 1 END,
    values_hash = EXCLUDED.values_hash,
    last_seen   = EXCLUDED.last_seen
RETURNING *;

-- name: UpsertPriceProposal :one
-- A fetch's proposal. A live open one with the same source, model and values is refreshed (its
-- class and the numbers can change as the stability and agreement facts do); a dismissed one is
-- remembered and returns no row.
INSERT INTO price_proposals (source, origin, provider_type, model, current_ref, current_entry, proposed_entry,
                             changes, provenance, agreeing_sources, class, failed_rules, values_hash)
VALUES (@source, @origin, @provider_type, @model, @current_ref, @current_entry, @proposed_entry,
        @changes, @provenance, @agreeing_sources, @class, @failed_rules, @values_hash)
ON CONFLICT (source, model, values_hash) WHERE state IN ('open', 'dismissed') DO UPDATE SET
    current_ref = EXCLUDED.current_ref, current_entry = EXCLUDED.current_entry, proposed_entry = EXCLUDED.proposed_entry,
    changes = EXCLUDED.changes, provenance = EXCLUDED.provenance, agreeing_sources = EXCLUDED.agreeing_sources,
    class = EXCLUDED.class, failed_rules = EXCLUDED.failed_rules, updated_at = now()
WHERE price_proposals.state = 'open'
RETURNING *;

-- name: SupersedeOpenPriceProposals :execrows
-- A fetch that returned other values for a model, or the same as the effective price, replaces the
-- source's open proposals for it.
UPDATE price_proposals SET state = 'superseded', updated_at = now()
WHERE source = @source AND model = @model AND origin = @origin AND state = 'open' AND values_hash <> @keep_hash;

-- name: GetPriceProposal :one
SELECT * FROM price_proposals WHERE id = @id;

-- name: ListPriceProposals :many
SELECT * FROM price_proposals
WHERE (@state::text = '' OR state = @state) AND (@class::text = '' OR class = @class)
ORDER BY created_at DESC, id DESC LIMIT @max_rows;

-- name: DecidePriceProposal :one
-- Accepts or dismisses an open proposal. No row: it was already decided.
UPDATE price_proposals
SET state = @state, decided_at = now(), updated_at = now(), decided_by = @decided_by, applied_version = sqlc.narg(applied_version)
WHERE id = @id AND state = 'open'
RETURNING *;

-- name: CountAutoAppliedPriceProposalsSince :one
SELECT count(*)::bigint FROM price_proposals WHERE decided_by = 'auto' AND state = 'accepted' AND decided_at >= @since;

-- name: CountPriceProposalsByClassState :many
SELECT class, state, count(*)::bigint AS n FROM price_proposals GROUP BY class, state;

-- name: ListPriceModelNames :many
-- The names of the enabled models of providers of one type, in every org (the fetch diffs a
-- source against the catalog's models). Names only: no org, provider or secret.
SELECT DISTINCT m.name
FROM models m
JOIN providers p ON p.org_id = m.org_id AND p.id = m.provider_id
WHERE p.type = @provider_type AND m.disabled_reason IS NULL AND p.disabled_reason IS NULL
ORDER BY m.name;

-- name: GetPriceRefreshSettings :one
SELECT * FROM price_refresh_settings WHERE singleton;

-- name: UpsertPriceRefreshSettings :exec
INSERT INTO price_refresh_settings (singleton, mode, interval, max_change_percent, max_auto_per_day)
VALUES (true, @mode, @interval, @max_change_percent, @max_auto_per_day)
ON CONFLICT (singleton) DO UPDATE SET mode = EXCLUDED.mode, interval = EXCLUDED.interval,
    max_change_percent = EXCLUDED.max_change_percent, max_auto_per_day = EXCLUDED.max_auto_per_day, updated_at = now();

-- name: GetPriceSourceObservation :one
-- The run of unchanged fetches of a source for a model (the stability rule).
SELECT * FROM price_source_observations WHERE source = @source AND model = @model;

-- name: ProviderTypeInUse :one
-- Whether any org has an enabled provider of this type (the refresh's auto mode). A yes or no only:
-- no org, provider or secret.
SELECT EXISTS (SELECT 1 FROM providers WHERE type = @provider_type AND disabled_reason IS NULL)::boolean AS in_use;

-- name: ListModelsOfProviderType :many
-- The enabled models of enabled providers of one type, with their org, for the unpriced-model count.
SELECT m.org_id, m.id, m.name
FROM models m
JOIN providers p ON p.org_id = m.org_id AND p.id = m.provider_id
WHERE p.type = @provider_type AND m.disabled_reason IS NULL AND p.disabled_reason IS NULL
ORDER BY m.org_id, m.name;

-- name: GetLastPriceSourceFetch :one
SELECT * FROM price_source_fetches WHERE source = @source ORDER BY fetched_at DESC LIMIT 1;

-- name: SetPriceProposalClass :exec
-- Moves an open proposal to another class with the rule that decided it (the daily cap).
UPDATE price_proposals SET class = @class, failed_rules = @failed_rules, updated_at = now() WHERE id = @id AND state = 'open';

-- name: ListOpenAutoPriceProposals :many
SELECT * FROM price_proposals WHERE state = 'open' AND class = 'auto' AND origin = 'fetch' ORDER BY created_at, id;
