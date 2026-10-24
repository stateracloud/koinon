-- SPDX-License-Identifier: Apache-2.0
--
-- Folds the per-model rates of PV1 into dated org price entries (plan item W4, ADR 0056
-- amendment 2026-10-24). After this the rates layer is gone from pricing precedence: org prices,
-- then the default list.
--
-- Each model with usable rates becomes one price_overrides row for its provider type and its
-- exact name, valid from the model's creation (the rates applied to every request since the
-- model existed, so a ledger entry ingested late is priced as it would have been). The models
-- columns stay, unread, for one release (expand, then contract) so a rolling upgrade does not
-- break the old pods; a later migration drops them.
--
-- Not carried over, because SQL cannot read the default list: a cache write rate, a reasoning
-- rate, tiers and the tokenizer (bytes, 3.0 bytes per token). max_output is not stored on a
-- model; the entry gets 131072, a ceiling for an aborted response's estimate. Admins can
-- tighten all of these by adding a later version of the price.

-- +goose Up

-- +goose StatementBegin
DO $$
DECLARE
    m record;
    skipped int := 0;
BEGIN
    FOR m IN
        SELECT mo.org_id, mo.id, mo.name, mo.input_rate, mo.output_rate, mo.cache_read_rate, mo.created_at, p.type AS provider_type
        FROM models mo
        JOIN providers p ON p.org_id = mo.org_id AND p.id = mo.provider_id
        WHERE mo.input_rate IS NOT NULL
    LOOP
        -- A rate the stricter validation (money.ParseRate: at most six whole and six fraction
        -- digits) would refuse is not carried: one unusable entry would stop pricing for the org.
        IF m.input_rate !~ '^[0-9]{1,6}(\.[0-9]{1,6})?$'
           OR m.output_rate !~ '^[0-9]{1,6}(\.[0-9]{1,6})?$'
           OR (m.cache_read_rate IS NOT NULL AND m.cache_read_rate !~ '^[0-9]{1,6}(\.[0-9]{1,6})?$')
           OR m.provider_type !~ '^[a-z][a-z0-9-]{0,62}$' THEN
            skipped := skipped + 1;
            RAISE NOTICE 'model % of org % has rates that cannot be priced; not carried over', m.name, m.org_id;
            CONTINUE;
        END IF;
        INSERT INTO price_overrides (org_id, name, entry, valid_from, created_by)
        VALUES (
            m.org_id,
            'model-' || left(regexp_replace(lower(m.name), '[^a-z0-9._-]+', '-', 'g'), 100) || '-' || left(replace(m.id::text, '-', ''), 8),
            jsonb_build_object(
                'provider', m.provider_type,
                'match', jsonb_build_object('glob', m.name),
                'validFrom', to_char(m.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'),
                'prices', jsonb_strip_nulls(jsonb_build_object('input', m.input_rate, 'output', m.output_rate, 'cacheRead', m.cache_read_rate)),
                'tokenizer', 'bytes',
                'ratio', '3.0',
                'maxOutput', CASE WHEN m.input_rate::numeric = 0 AND m.output_rate::numeric = 0 AND coalesce(m.cache_read_rate::numeric, 0) = 0 THEN 0 ELSE 131072 END,
                'source', jsonb_build_object('url', '', 'accessed', ''),
                'note', 'Migrated from the rates set on the model; max_output is a ceiling, set a later version to tighten it.'
            ),
            m.created_at,
            'migration 00014'
        )
        ON CONFLICT DO NOTHING;
    END LOOP;
END
$$;
-- +goose StatementEnd

COMMENT ON COLUMN models.input_rate IS 'Deprecated and unread since migration 00014: prices are price_overrides rows. Dropped later.';
COMMENT ON COLUMN models.output_rate IS 'Deprecated and unread since migration 00014. Dropped later.';
COMMENT ON COLUMN models.cache_read_rate IS 'Deprecated and unread since migration 00014. Dropped later.';
