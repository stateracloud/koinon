-- SPDX-License-Identifier: Apache-2.0

-- name: GetOrgBySlug :one
SELECT * FROM orgs
WHERE slug = $1 AND deleted_at IS NULL;

-- name: ListOrgIDs :many
-- Every live org, for control-plane sweeps and workers that then work org by org. The run
-- claim loop of koinon-agentd uses ListOrgIDsWithRunWork instead.
SELECT id FROM orgs
WHERE deleted_at IS NULL
ORDER BY id;

-- name: ListOrgIDsWithRunWork :many
-- The live orgs where a worker could claim or take over a run right now: a run that is claimable
-- (ready_at set, ClaimNextRun) or whose owner's lease expired (TakeOverExpiredRun). agentd polls
-- this every few tens of milliseconds, so its cost follows the orgs with work, not every org that
-- ever existed (koinon issue 378). Both halves use partial indexes (runs_claimable, runs_leases).
-- A superset is fine: the claim queries re-check every condition.
SELECT o.id FROM orgs o
WHERE o.deleted_at IS NULL
  AND o.id IN (
    SELECT r.org_id FROM runs r WHERE r.ready_at IS NOT NULL
    UNION
    SELECT r.org_id FROM runs r
    WHERE r.state IN ('starting', 'running') AND r.lease_expires_at < now()
  )
ORDER BY o.id;

-- name: GetOrgSlug :one
-- The slug of the org the caller is acting in, for names derived from it (gateway targets).
SELECT slug FROM orgs
WHERE id = @org_id AND deleted_at IS NULL;

-- name: GetOrgDefaultModel :one
-- The model agents without a model setting run on (old issue 256); '' when the org has none.
SELECT coalesce(default_model, '')::text AS default_model FROM orgs
WHERE id = @org_id AND deleted_at IS NULL;

-- name: SetOrgDefaultModel :one
-- Sets or clears ('') the org's default model. The old value comes back for the audit event.
UPDATE orgs o
SET default_model = nullif(@default_model::text, '')
FROM (SELECT x.id, coalesce(x.default_model, '')::text AS old FROM orgs x WHERE x.id = @org_id AND x.deleted_at IS NULL FOR UPDATE) cur
WHERE o.id = cur.id
RETURNING cur.old::text AS old_default_model;

-- name: ClearOrgDefaultModelIfGone :one
-- Clears the org's default model when no model of that name is left (it was deleted); the old value
-- comes back for the audit event. No row: the default stays (or there was none).
UPDATE orgs o
SET default_model = NULL
FROM (SELECT x.id, x.default_model AS old FROM orgs x
      WHERE x.id = @org_id AND x.default_model IS NOT NULL AND x.deleted_at IS NULL FOR UPDATE) cur
WHERE o.id = cur.id
  AND NOT EXISTS (SELECT 1 FROM models m WHERE m.org_id = @org_id AND m.name = cur.old)
RETURNING cur.old::text AS old_default_model;

-- name: ListOrgsPendingDefaultModel :many
-- Orgs whose default model migration 00045 left for BackfillDefaults.
SELECT id FROM orgs
WHERE default_model_pending AND deleted_at IS NULL
ORDER BY id;

-- name: ClaimOrgDefaultModelBackfill :execrows
-- Takes an org's pending backfill; a concurrent replica waits on the row and then finds it taken.
UPDATE orgs SET default_model_pending = false
WHERE id = @org_id AND default_model_pending;

-- name: SetOrgDefaultModelIfUnset :execrows
-- The org's first model becomes its default; a default that is set stays.
UPDATE orgs
SET default_model = @default_model::text
WHERE id = @org_id AND default_model IS NULL AND deleted_at IS NULL;

-- name: ListOrgSummaries :many
-- The live orgs by slug after @after_slug, with member and agent counts, budget and spend since
-- @since, for the platform console (ADR 0126). Platform admins only. @only_org_id narrows the list
-- to one org (GetOrg); personal orgs are left out unless @include_personal or @only_org_id is set.
-- member_count applies the org's identity mode like ListEffectiveMemberships (ADR 0004).
SELECT o.id AS org_id, o.slug, o.display_name, o.kind, o.created_at,
       (SELECT count(DISTINCT om.user_id)
        FROM org_memberships om
        JOIN users u ON u.id = om.user_id
        WHERE om.org_id = o.id AND u.deleted_at IS NULL
          AND (o.identity_mode = 'hybrid'
               OR (o.identity_mode = 'platform' AND om.source = 'manual')
               OR (o.identity_mode = 'idp' AND (om.source = 'idp_group' OR om.role = 'owner'))))::bigint AS member_count,
       (SELECT count(*) FROM agents a WHERE a.org_id = o.id AND a.status <> 'archived')::bigint AS agent_count,
       coalesce(b.monthly_cap::text, '')::text AS monthly_cap,
       coalesce(b.platform_ceiling::text, '')::text AS platform_ceiling,
       coalesce((SELECT sum(s.cost_usd) FROM spend_entries s WHERE s.org_id = o.id AND s.occurred_at >= @since), 0)::text AS spent
FROM orgs o
LEFT JOIN org_budgets b ON b.org_id = o.id
WHERE o.deleted_at IS NULL
  AND (sqlc.narg(only_org_id)::uuid IS NULL OR o.id = sqlc.narg(only_org_id)::uuid)
  AND (@include_personal::bool OR sqlc.narg(only_org_id)::uuid IS NOT NULL OR o.kind = 'team')
  AND o.slug > @after_slug::text
ORDER BY o.slug
LIMIT @row_limit;

-- name: ListOrgRefs :many
-- The live orgs by slug after @after_slug, without counts, for the platform console's lists of org
-- resources (PlatformResourceService, ADR 0126). Platform admins only. @only_org_id narrows the list
-- to one org; personal orgs are left out unless @include_personal or @only_org_id is set.
SELECT o.id AS org_id, o.slug, o.display_name, o.kind
FROM orgs o
WHERE o.deleted_at IS NULL
  AND (sqlc.narg(only_org_id)::uuid IS NULL OR o.id = sqlc.narg(only_org_id)::uuid)
  AND (@include_personal::bool OR sqlc.narg(only_org_id)::uuid IS NOT NULL OR o.kind = 'team')
  AND o.slug > @after_slug::text
ORDER BY o.slug
LIMIT @row_limit;

-- name: ListOrgMembers :many
-- The users with an effective role in the org, by display name and then id, after the cursor
-- (@after_name, @after_id). The org's identity mode applies like ListEffectiveMemberships
-- (ADR 0004); the highest role wins.
SELECT u.id AS user_id, u.display_name, max(om.role)::org_role AS role
FROM org_memberships om
JOIN orgs o ON o.id = om.org_id
JOIN users u ON u.id = om.user_id
WHERE om.org_id = @org_id
  AND o.deleted_at IS NULL
  AND u.deleted_at IS NULL
  AND (o.identity_mode = 'hybrid'
       OR (o.identity_mode = 'platform' AND om.source = 'manual')
       OR (o.identity_mode = 'idp' AND (om.source = 'idp_group' OR om.role = 'owner')))
  AND (u.display_name, u.id) > (@after_name::text, @after_id::uuid)
GROUP BY u.id, u.display_name
ORDER BY u.display_name, u.id
LIMIT @row_limit;
