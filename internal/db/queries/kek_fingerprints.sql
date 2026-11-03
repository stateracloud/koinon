-- SPDX-License-Identifier: Apache-2.0
--
-- KEK fingerprints (#591, ADR 0085 decision 12): what each key-encryption key was when a process
-- first loaded it. No key material.

-- name: ListKEKFingerprints :many
SELECT * FROM kek_fingerprints
ORDER BY kek_id;

-- name: RecordKEKFingerprint :one
-- Inserts the KEK or, when it is known, keeps its fingerprint and first_seen and updates what wraps
-- it and (when given) its activation time. Returns the row as stored, so the caller sees a
-- fingerprint that differs from the one it holds.
INSERT INTO kek_fingerprints (kek_id, fingerprint, backend, key_id, activated_at)
VALUES (@kek_id, @fingerprint, @backend, @key_id, sqlc.narg(activated_at))
ON CONFLICT (kek_id) DO UPDATE
SET backend = EXCLUDED.backend,
    key_id = EXCLUDED.key_id,
    activated_at = COALESCE(EXCLUDED.activated_at, kek_fingerprints.activated_at)
RETURNING *;
