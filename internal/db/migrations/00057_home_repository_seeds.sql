-- SPDX-License-Identifier: Apache-2.0
--
-- Seeding a new home repository after a compromise (ADR 0085 decision 14; #86). `koinon-api keys
-- new-repositories` starts a new repository epoch for each agent, with a password nobody has seen,
-- and asks for it to be seeded: koinon-agentd copies the agent's newest restorable snapshot from the
-- older epochs into the new repository, as a checkpoint of the new epoch. The old epoch is retired
-- with a deadline of now; the sweeper deletes it once the new epoch holds a checkpoint at least as
-- new as its newest (the guard of ListDueHomeRepositories), so history older than that snapshot is
-- dropped.
--
-- Expand-only (ADR 0085 decisions 5 and 6): new nullable columns and a constraint on them; every
-- row that exists satisfies it, and an older binary neither reads nor writes them (it never starts
-- an epoch with a seed).
--
-- seed_from_epoch: the epoch that was current when the new one started; set only for a seeded
--   epoch (reason key_compromised).
-- seed_after: when koinon-agentd may next try the seed (a claim moves it forward by its lease).
-- seed_attempts, seed_error: the tries so far and the last failure.
-- seeded_at: the new epoch holds the agent's newest home; seed_failed_at: given up after the tries,
--   the old epoch is kept (nothing newer holds the home) until an operator starts another epoch.

-- +goose Up

ALTER TABLE home_repository_epochs
    ADD COLUMN seed_from_epoch integer,
    ADD COLUMN seed_after      timestamptz,
    ADD COLUMN seed_attempts   integer NOT NULL DEFAULT 0 CHECK (seed_attempts >= 0),
    ADD COLUMN seed_error      text,
    ADD COLUMN seeded_at       timestamptz,
    ADD COLUMN seed_failed_at  timestamptz,
    ADD CONSTRAINT home_repository_epochs_seed CHECK (
        (seed_from_epoch IS NULL
            AND seed_after IS NULL AND seeded_at IS NULL AND seed_failed_at IS NULL AND seed_attempts = 0)
        OR (seed_from_epoch >= 0 AND seed_from_epoch < repository_epoch AND reason = 'key_compromised'
            AND seed_after IS NOT NULL AND (seeded_at IS NULL OR seed_failed_at IS NULL)));

-- The seeds koinon-agentd still has to do.
CREATE INDEX home_repository_epochs_seed_due ON home_repository_epochs (seed_after)
    WHERE seed_from_epoch IS NOT NULL AND seeded_at IS NULL AND seed_failed_at IS NULL;

