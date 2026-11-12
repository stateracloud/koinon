-- SPDX-License-Identifier: Apache-2.0
--
-- Templates name their harnesses in one block (ADR 0112, #610): spec.flavors {allowed, default}
-- moves into spec.harness, beside the harness settings that block already holds. The stored copy
-- of each template version is rewritten in place, so a version synced before this change reads
-- like one synced after it. content_hash is the hash of the template file and is left as it is:
-- the next sync stores the renamed files as new versions.

-- +goose Up
UPDATE templates
SET spec = jsonb_set(
    spec #- '{spec,flavors}',
    '{spec,harness}',
    COALESCE(spec -> 'spec' -> 'harness', '{}'::jsonb) || (spec -> 'spec' -> 'flavors')
)
WHERE spec -> 'spec' ? 'flavors';
