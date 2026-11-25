-- Home checkpoint content digest (issue #1090, ADR 0119 D5, ADR 0032 amendment 2026-11-25).
--
-- The writer records the SHA-256 digest of the staged home tree when it completes a checkpoint.
-- The restore recomputes it from the restored files and refuses a checkpoint that differs, before
-- it sends a byte into the sandbox. The digest sits in Koinon's database, not only in the bucket,
-- so a write to the bucket cannot change the record too.
--
-- NULL means a checkpoint made before this column: the restore accepts it, because no digest was
-- recorded for it, and says so in the log.

-- +goose Up

ALTER TABLE home_checkpoints ADD COLUMN content_digest text
    CHECK (content_digest IS NULL OR content_digest ~ '^sha256:[0-9a-f]{64}$');
