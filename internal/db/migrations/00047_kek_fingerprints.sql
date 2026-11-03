-- SPDX-License-Identifier: Apache-2.0
--
-- KEK fingerprints (#591, ADR 0085 decision 12, part 2).
--
-- The key-encryption keys live in a Kubernetes Secret, outside the database. A restored database
-- paired with a restored or wrong KEK Secret opens nothing, and nothing says why. This table
-- records, per KEK ID, the fingerprint (secretstore.Fingerprint, which does not reveal the key) the
-- first time a process loaded it, so "keys status" can tell a KEK that differs from the one the
-- database was written with. It holds no key material.
--
-- Platform-wide, not org-owned: the KEKs belong to the install. A row is never deleted when a KEK
-- is retired; IDs are never reused, so the row stays the record of what that ID was.

-- +goose Up

CREATE TABLE kek_fingerprints (
    kek_id       text PRIMARY KEY CHECK (length(kek_id) BETWEEN 1 AND 128),
    fingerprint  text NOT NULL CHECK (length(fingerprint) BETWEEN 1 AND 128),
    -- What wraps the KEK now: the backend ("local", "openbao", "awskms") and the wrapping key's ID
    -- ('' for local). Updated when the KEK is wrapped by another key; the fingerprint never is.
    backend      text NOT NULL CHECK (length(backend) BETWEEN 1 AND 16),
    key_id       text NOT NULL DEFAULT '' CHECK (length(key_id) <= 512),
    first_seen   timestamptz NOT NULL DEFAULT now(),
    -- When the KEK became the active one, if known (from "keys activate" or the KEK Secret).
    activated_at timestamptz
);
