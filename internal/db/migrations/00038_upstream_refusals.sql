-- SPDX-License-Identifier: Apache-2.0
--
-- What the provider said when it refused a model request (issues #241 and #246; ADR 0056 amendment
-- 2026-10-29). The gateway's access log says whether it called the provider (koinon.upstream) and,
-- for an error response, carries a redacted prefix of the body (koinon.error_body). The ledger now
-- records a request the provider refused with a 4xx even when the status is one the gateway also
-- uses (401, 403, 404, 429), charges it by the provider type's documented billing, and keeps the
-- status and a one-line excerpt here so that agentd can tell the person why a run failed from the
-- gateway's record instead of the harness's generic text (ADR 0030, amendment 2026-10-29).
-- Numbered after 00037_run_wrap_up_notice.

-- +goose Up

ALTER TABLE spend_entries
    -- The HTTP status the provider answered, set only when it is an error (4xx or 5xx). NULL when
    -- the request succeeded, the gateway answered itself, or the config that logged it could not
    -- say who answered.
    ADD COLUMN upstream_status smallint CHECK (upstream_status BETWEEN 400 AND 599),
    -- A one-line excerpt of the provider's error (package errbody): its message and reasons,
    -- redacted and capped, never the body.
    ADD COLUMN upstream_error  text CHECK (length(upstream_error) <= 2048),
    ADD CONSTRAINT spend_entries_upstream_error_status CHECK (upstream_error IS NULL OR upstream_status IS NOT NULL);

-- agentd reads the last refusal of a run when the harness reports an error without a status.
CREATE INDEX spend_entries_run_refused ON spend_entries (org_id, run_id, occurred_at)
    WHERE upstream_status IS NOT NULL;
