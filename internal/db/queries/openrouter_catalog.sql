-- SPDX-License-Identifier: Apache-2.0
-- The per-install OpenRouter model catalog (ADR 0070 amendment of 2026-11-11, #543). Install-wide
-- tables: no org column, nothing here reads an org's rows.

-- name: GetOpenRouterCatalog :one
SELECT * FROM openrouter_catalog WHERE singleton;

-- name: UpsertOpenRouterCatalog :exec
INSERT INTO openrouter_catalog (singleton, source, fetched_at, model_count, sha256)
VALUES (true, @source, @fetched_at, @model_count, @sha256)
ON CONFLICT (singleton) DO UPDATE
SET source = EXCLUDED.source, fetched_at = EXCLUDED.fetched_at, model_count = EXCLUDED.model_count, sha256 = EXCLUDED.sha256;

-- name: UpsertOpenRouterModel :exec
-- Display fields apply at once. The fetched limits are stored but are not effective (see
-- openrouter_limit_values). The endpoints columns are left as they are.
INSERT INTO openrouter_models (model, display, context_length, max_output, source, fetched_at)
VALUES (@model, @display, @context_length, @max_output, @source, @fetched_at)
ON CONFLICT (model) DO UPDATE
SET display = EXCLUDED.display, context_length = EXCLUDED.context_length, max_output = EXCLUDED.max_output,
    source = EXCLUDED.source, fetched_at = EXCLUDED.fetched_at;

-- name: GetOpenRouterModel :one
SELECT * FROM openrouter_models WHERE model = @model;

-- name: ListOpenRouterModels :many
SELECT * FROM openrouter_models ORDER BY model;

-- name: SetOpenRouterModelEndpoints :execrows
UPDATE openrouter_models
SET endpoints = @endpoints, endpoints_source = @endpoints_source, endpoints_fetched_at = @endpoints_fetched_at
WHERE model = @model;

-- name: ListOpenRouterLimitValues :many
SELECT * FROM openrouter_limit_values ORDER BY model, field;

-- name: GetOpenRouterLimitValue :one
SELECT * FROM openrouter_limit_values WHERE model = @model AND field = @field;

-- name: UpsertOpenRouterLimitValue :exec
INSERT INTO openrouter_limit_values (model, field, value, accepted_by, proposal_id)
VALUES (@model, @field, @value, @accepted_by, @proposal_id)
ON CONFLICT (model, field) DO UPDATE
SET value = EXCLUDED.value, accepted_at = now(), accepted_by = EXCLUDED.accepted_by, proposal_id = EXCLUDED.proposal_id;

-- name: FindLiveOpenRouterLimitProposal :one
SELECT * FROM openrouter_limit_proposals
WHERE model = @model AND field = @field AND proposed_value = @proposed_value AND state IN ('open', 'dismissed');

-- name: InsertOpenRouterLimitProposal :one
INSERT INTO openrouter_limit_proposals (model, field, current_value, proposed_value, source)
VALUES (@model, @field, @current_value, @proposed_value, @source)
RETURNING *;

-- name: RefreshOpenRouterLimitProposal :exec
UPDATE openrouter_limit_proposals SET current_value = @current_value, source = @source WHERE id = @id AND state = 'open';

-- name: SupersedeOpenRouterLimitProposals :execrows
-- Closes the open proposals of a model and field whose value is not the one now proposed (the fetch
-- moved on), or all of them when keep is 0 (the fetched value is the accepted one).
UPDATE openrouter_limit_proposals SET state = 'superseded', decided_at = now(), decided_by = 'system'
WHERE model = @model AND field = @field AND state = 'open' AND proposed_value <> @keep;

-- name: ListOpenRouterLimitProposals :many
SELECT * FROM openrouter_limit_proposals
WHERE (@state::text = '' OR state = @state::text)
ORDER BY created_at DESC, id DESC
LIMIT @max_rows;

-- name: GetOpenRouterLimitProposalForUpdate :one
SELECT * FROM openrouter_limit_proposals WHERE id = @id FOR UPDATE;

-- name: DecideOpenRouterLimitProposal :exec
UPDATE openrouter_limit_proposals SET state = @state, decided_at = now(), decided_by = @decided_by WHERE id = @id AND state = 'open';
