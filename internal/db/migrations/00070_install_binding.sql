-- SPDX-License-Identifier: Apache-2.0
--
-- Install binding (ADR 0110 decisions 0 and 3, issue 516): the database records which install it
-- belongs to, so a restored copy can tell that it is one and refuse the source's API tokens and
-- sessions. The install mark itself never enters the database; only its hash does (the same hash,
-- execution.OwnerID, that labels OpenShell objects).
--
-- Expand only (ADR 0085): one new table and two nullable columns.
-- +goose Up

-- One row per database. mark is NULL for an unbound row, which a held start writes when it finds no
-- row: the state is then "copy" and stays so after the release until adopt-restore. The outward
-- target columns hold the targets of the install that last ran unheld here (decision 0); the runtime
-- guards of decision 4 compare against them.
CREATE TABLE install_binding (
    singleton           boolean     PRIMARY KEY DEFAULT true CHECK (singleton),
    mark                text        CHECK (mark IS NULL OR length(mark) > 0),
    public_url          text,
    oidc_redirect_url   text,
    checkpoint_endpoint text,
    checkpoint_bucket   text,
    held_since          timestamptz,
    released_at         timestamptz,
    released_by         text,
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- The hash of the mark of the install that issued the row. NULL: issued before this migration, or by
-- a process that has no mark (a development run). Authentication refuses a row whose mark is not the
-- process's own (decision 3); a NULL counts as the process's own only in state "same".
ALTER TABLE api_tokens ADD COLUMN install_mark text;
ALTER TABLE sessions ADD COLUMN install_mark text;
