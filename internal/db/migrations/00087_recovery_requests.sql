-- SPDX-License-Identifier: Apache-2.0
--
-- recovery_requests (issue #512, ADR 0110 decision 6): one row per PlatformAdminRecovery request the
-- reconciler carried out. The nonce is the primary key, so a re-applied manifest (a GitOps sync, a
-- copy-paste) finds its row and does nothing. cr_uid names the request object that used the nonce: a
-- retry of the same object (its status write failed) reads sealed_result back instead of acting twice.
-- sealed_result is the age ciphertext sealed to the requester's key; the secret itself is never stored.
-- The action column has no CHECK: a later action (adopt-restore) must not need a contract migration.
-- A new table, so this is an expand migration and legal on a patch line (ADR 0085).

-- +goose Up

CREATE TABLE recovery_requests (
    nonce         text        PRIMARY KEY CHECK (nonce ~ '^[A-Za-z0-9_-]{16,64}$'),
    cr_uid        text        NOT NULL,
    action        text        NOT NULL,
    requester     text        NOT NULL,
    sealed_result text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now()
);
