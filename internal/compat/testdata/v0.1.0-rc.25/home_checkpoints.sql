-- Written for the schema at the recorded version; loaded by internal/compat.
INSERT INTO orgs (id, slug, display_name, kind, deleted_at)
VALUES ('0198a000-0000-7000-8000-0000000000d0', 'compat', 'Compat', 'personal', '2026-01-01T00:00:00Z');
INSERT INTO agents (org_id, id, display_name, harness) VALUES
  ('0198a000-0000-7000-8000-0000000000d0', '0198a000-0000-7000-8000-0000000000d1', 'one', 'opencode'),
  ('0198a000-0000-7000-8000-0000000000d0', '0198a000-0000-7000-8000-0000000000d2', 'two', 'opencode');
-- The agent whose home the kopia backup holds: its repository row carries the password envelope.
INSERT INTO orgs (id, slug, display_name, kind, deleted_at)
VALUES ('0198a000-0000-7000-8000-0000000000c1', 'compat-backup', 'Compat backup', 'personal', '2026-01-01T00:00:00Z');
INSERT INTO agents (org_id, id, display_name, harness)
VALUES ('0198a000-0000-7000-8000-0000000000c1', '0198a000-0000-7000-8000-0000000000c2', 'backup', 'opencode');
INSERT INTO home_repositories (org_id, agent_id, prefix, password_envelope)
VALUES ('0198a000-0000-7000-8000-0000000000c1', '0198a000-0000-7000-8000-0000000000c2', 'orgs/0198a000-0000-7000-8000-0000000000c1/homes/0198a000-0000-7000-8000-0000000000c2/', '{"fmt":1,"alg":"AES-256-GCM","kek_id":"compat","wrapped_dek":"gXIIXwl8gtR/5Cc7Mrji/vslB/jodfuABjXj8YrngMHEY9k5V3A12vwUc8oai0T2","dek_nonce":"UEH7L2pSrn4w+7nq","nonce":"5ysdyafRMmppuMkH","ciphertext":"y1hJ3PYhpebKugfWhRPmHC4q6EGYVvxk9VmBG/dDf215Z/fHQo6n2x9OJPDbkeA="}'::jsonb);
INSERT INTO home_checkpoints
  (org_id, id, agent_id, reason, status, sandbox_id, generation, epoch, snapshot_id, size_bytes, attempts, error, requested_at, started_at, finished_at)
VALUES
  ('0198a000-0000-7000-8000-0000000000d0', '0198a000-0000-7000-8000-0000000000e1', '0198a000-0000-7000-8000-0000000000d1',
   'hourly', 'complete', 'sbx-1', 'gen-1', 3, 'snap-1', 4096, 1, NULL,
   '2026-01-02T03:04:05Z', '2026-01-02T03:04:06Z', '2026-01-02T03:04:07Z'),
  ('0198a000-0000-7000-8000-0000000000d0', '0198a000-0000-7000-8000-0000000000e2', '0198a000-0000-7000-8000-0000000000d1',
   'recreate', 'failed', 'sbx-1', 'gen-1', 3, NULL, NULL, 5, 'storage unreachable',
   '2026-01-03T03:04:05Z', '2026-01-03T03:04:06Z', '2026-01-03T03:04:07Z'),
  ('0198a000-0000-7000-8000-0000000000d0', '0198a000-0000-7000-8000-0000000000e3', '0198a000-0000-7000-8000-0000000000d1',
   'stop', 'requested', '', '', 0, NULL, NULL, 0, NULL,
   '2026-01-04T03:04:05Z', NULL, NULL),
  ('0198a000-0000-7000-8000-0000000000d0', '0198a000-0000-7000-8000-0000000000e4', '0198a000-0000-7000-8000-0000000000d2',
   'hourly', 'running', 'sbx-2', 'gen-2', 1, NULL, NULL, 1, NULL,
   '2026-01-05T03:04:05Z', '2026-01-05T03:04:06Z', NULL);
