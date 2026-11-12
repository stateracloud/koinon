-- SPDX-License-Identifier: Apache-2.0
--
-- Whether the operator's per-org cap granted an agent's keep-warm request (#100).
--
-- agents.keep_warm (00044) is a request. The reconciler grants requests each pass, oldest agent first,
-- up to reconciler.keepWarmPerOrg. The API and the agent page must show the outcome, and only the
-- reconciler knows the cap, so it records its decision here and the API reads it. The column is
-- meaningful only while keep_warm is true; a new request starts not granted until the next pass.
-- +goose Up
ALTER TABLE agents
    ADD COLUMN keep_warm_granted boolean NOT NULL DEFAULT false;
