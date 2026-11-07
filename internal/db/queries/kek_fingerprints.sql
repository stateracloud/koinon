-- SPDX-License-Identifier: Apache-2.0
--
-- KEK fingerprints (old issue 591, ADR 0085 decision 12): what each key-encryption key was when a process
-- first loaded it. No key material.

-- name: ListKEKFingerprints :many
SELECT * FROM kek_fingerprints
ORDER BY kek_id;

-- name: RecordKEKFingerprint :one
-- Inserts the KEK or, when it is known, keeps its fingerprint and first_seen. What wraps it and its
-- activation time are updated only when the fingerprint matches: a different key under a known ID
-- (a wrong backup) must not rewrite the record. Returns the row as stored, so the caller sees a
-- fingerprint that differs from the one it holds.
INSERT INTO kek_fingerprints (kek_id, fingerprint, backend, key_id, activated_at)
VALUES (@kek_id, @fingerprint, @backend, @key_id, sqlc.narg(activated_at))
ON CONFLICT (kek_id) DO UPDATE
SET backend = CASE WHEN kek_fingerprints.fingerprint = EXCLUDED.fingerprint
                   THEN EXCLUDED.backend ELSE kek_fingerprints.backend END,
    key_id = CASE WHEN kek_fingerprints.fingerprint = EXCLUDED.fingerprint
                  THEN EXCLUDED.key_id ELSE kek_fingerprints.key_id END,
    activated_at = CASE WHEN kek_fingerprints.fingerprint = EXCLUDED.fingerprint
                        THEN COALESCE(EXCLUDED.activated_at, kek_fingerprints.activated_at)
                        ELSE kek_fingerprints.activated_at END
RETURNING *;
