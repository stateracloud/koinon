-- SPDX-License-Identifier: Apache-2.0
--
-- A per-agent keep-warm setting (#315, decision by Andre 2026-10-31).
--
-- A sandbox starts on demand: activating an agent creates nothing, the first run, claimable work
-- or a pre-wake signal does. An org admin can ask for an agent's sandbox to stay up for fast
-- replies: agents.keep_warm. The platform operator caps the keep-warm sandboxes per org
-- (reconciler.keepWarmPerOrg), so the flag is a request: the reconciler grants it to the org's
-- oldest keep-warm agents up to the cap and the rest start on demand.

-- +goose Up

ALTER TABLE agents
    ADD COLUMN keep_warm boolean NOT NULL DEFAULT false;
