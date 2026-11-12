// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // the "pgx" database/sql driver
	"github.com/pressly/goose/v3"

	"github.com/stateracloud/koinon/internal/checkpoint"
	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/db/store"
)

const (
	checkpointsSQL      = "home_checkpoints.sql"
	checkpointsExpected = "home_checkpoints.expected.json"
)

// checkpointsFixtureSQL is written for the schema at the recorded version: an org and two agents
// (a deleted org skips the owner rule, which is not what is under test) and one checkpoint row in
// every status. Two agents because at most one request per agent is open.
func checkpointsFixtureSQL(envelope []byte) string {
	return fmt.Sprintf(checkpointsFixtureFmt, strings.ReplaceAll(string(envelope), "'", "''"), checkpoint.Prefix(backupOrg, backupAgent))
}

const checkpointsFixtureFmt = `-- Written for the schema at the recorded version; loaded by internal/compat.
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
VALUES ('0198a000-0000-7000-8000-0000000000c1', '0198a000-0000-7000-8000-0000000000c2', '%[2]s', '%[1]s'::jsonb);
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
`

// checkpointsExpected is those rows as the check reads them back (to_jsonb, session time zone
// UTC). Only these keys are compared: a column a later migration adds is free.
var checkpointsExpectedRows = []map[string]any{
	{"id": "0198a000-0000-7000-8000-0000000000e1", "agent_id": "0198a000-0000-7000-8000-0000000000d1", "reason": "hourly", "status": "complete", "sandbox_id": "sbx-1", "generation": "gen-1", "epoch": 3, "snapshot_id": "snap-1", "size_bytes": 4096, "attempts": 1, "error": nil, "requested_at": "2026-01-02T03:04:05+00:00", "started_at": "2026-01-02T03:04:06+00:00", "finished_at": "2026-01-02T03:04:07+00:00"},
	{"id": "0198a000-0000-7000-8000-0000000000e2", "agent_id": "0198a000-0000-7000-8000-0000000000d1", "reason": "recreate", "status": "failed", "sandbox_id": "sbx-1", "generation": "gen-1", "epoch": 3, "snapshot_id": nil, "size_bytes": nil, "attempts": 5, "error": "storage unreachable", "requested_at": "2026-01-03T03:04:05+00:00", "started_at": "2026-01-03T03:04:06+00:00", "finished_at": "2026-01-03T03:04:07+00:00"},
	{"id": "0198a000-0000-7000-8000-0000000000e3", "agent_id": "0198a000-0000-7000-8000-0000000000d1", "reason": "stop", "status": "requested", "sandbox_id": "", "generation": "", "epoch": 0, "snapshot_id": nil, "size_bytes": nil, "attempts": 0, "error": nil, "requested_at": "2026-01-04T03:04:05+00:00", "started_at": nil, "finished_at": nil},
	{"id": "0198a000-0000-7000-8000-0000000000e4", "agent_id": "0198a000-0000-7000-8000-0000000000d2", "reason": "hourly", "status": "running", "sandbox_id": "sbx-2", "generation": "gen-2", "epoch": 1, "snapshot_id": nil, "size_bytes": nil, "attempts": 1, "error": nil, "requested_at": "2026-01-05T03:04:05+00:00", "started_at": "2026-01-05T03:04:06+00:00", "finished_at": nil},
}

// recordHomeCheckpoints writes the fixture for the schema version of this build. No database is
// needed to record: the SQL is plain and the check is what runs it.
func recordHomeCheckpoints(out string) error {
	env, err := sealedPassword()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, checkpointsSQL), []byte(checkpointsFixtureSQL(env)), 0o600); err != nil {
		return err
	}
	return writeJSON(filepath.Join(out, checkpointsExpected), checkpointsExpectedRows)
}

// CheckHomeCheckpoints loads the fixture into the empty database at dsn at the recorded schema
// version, migrates it to head, and requires every recorded row to read back unchanged: raw
// (to_jsonb, so a dropped or changed column shows) and through the generated store queries the
// services use. It also opens the backup agent's repository password the way the runtime does
// (checkpoint.Passwords.Get: the home_repositories row, then the envelope under the test KEK) and
// returns that repository for CheckHomeBackup.
func CheckHomeCheckpoints(ctx context.Context, dsn, dir string) (checkpoint.Repository, error) {
	if err := checkHomeCheckpoints(ctx, dsn, dir); err != nil {
		return checkpoint.Repository{}, err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return checkpoint.Repository{}, err
	}
	defer pool.Close()
	q := store.New(pool)
	if err := checkCheckpointQueries(ctx, q, dir); err != nil {
		return checkpoint.Repository{}, err
	}
	ring, err := Keyring()
	if err != nil {
		return checkpoint.Repository{}, err
	}
	repo, err := checkpoint.Passwords{Q: q, Keys: ring}.Get(ctx, uuid(backupOrg), uuid(backupAgent))
	if err != nil {
		return checkpoint.Repository{}, fmt.Errorf("the recorded repository password no longer opens: %w", err)
	}
	return repo, nil
}

// checkCheckpointQueries reads the recorded rows through the store queries.
func checkCheckpointQueries(ctx context.Context, q *store.Queries, dir string) error {
	var want []map[string]any
	b, err := os.ReadFile(filepath.Join(dir, checkpointsExpected)) //nolint:gosec // G304: path inside the recorded release folder
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &want); err != nil {
		return fmt.Errorf("%s: %w", checkpointsExpected, err)
	}
	org := uuid("0198a000-0000-7000-8000-0000000000d0")
	for _, w := range want {
		id := fmt.Sprint(w["id"])
		c, err := q.GetHomeCheckpoint(ctx, store.GetHomeCheckpointParams{OrgID: org, ID: uuid(id)})
		if err != nil {
			return fmt.Errorf("GetHomeCheckpoint %s: %w", id, err)
		}
		got := map[string]any{
			"agent_id": c.AgentID.String(), "reason": string(c.Reason), "status": string(c.Status),
			"sandbox_id": c.SandboxID, "generation": c.Generation, "epoch": c.Epoch, "attempts": c.Attempts,
			"snapshot_id": textOrNil(c.SnapshotID.String, c.SnapshotID.Valid), "error": textOrNil(c.Error.String, c.Error.Valid),
		}
		if c.SizeBytes.Valid {
			got["size_bytes"] = c.SizeBytes.Int64
		} else {
			got["size_bytes"] = nil
		}
		for k, g := range got {
			wb, _ := json.Marshal(w[k])
			gb, _ := json.Marshal(g)
			if string(wb) != string(gb) {
				return fmt.Errorf("GetHomeCheckpoint %s field %s: got %s, recorded %s", id, k, gb, wb)
			}
		}
	}
	// The recreate gate's read: the newest complete checkpoint of a sandbox.
	latest, err := q.LatestCompleteHomeCheckpointOfSandbox(ctx, store.LatestCompleteHomeCheckpointOfSandboxParams{
		OrgID: org, AgentID: uuid("0198a000-0000-7000-8000-0000000000d1"), SandboxID: "sbx-1",
	})
	if err != nil {
		return fmt.Errorf("LatestCompleteHomeCheckpointOfSandbox: %w", err)
	}
	if latest.ID.String() != "0198a000-0000-7000-8000-0000000000e1" || latest.SnapshotID.String != "snap-1" {
		return fmt.Errorf("LatestCompleteHomeCheckpointOfSandbox returned %s (snapshot %q), recorded e1 (snap-1)", latest.ID.String(), latest.SnapshotID.String)
	}
	return nil
}

func textOrNil(s string, valid bool) any {
	if !valid {
		return nil
	}
	return s
}

func checkHomeCheckpoints(ctx context.Context, dsn, dir string) error {
	m, err := ReadManifest(dir)
	if err != nil {
		return err
	}
	head, err := migrations.Latest()
	if err != nil {
		return err
	}
	if m.SchemaVersion > head {
		return fmt.Errorf("the folder was recorded at schema %d, newer than this build's %d", m.SchemaVersion, head)
	}
	stmts, err := os.ReadFile(filepath.Join(dir, checkpointsSQL)) //nolint:gosec // G304: path inside the recorded release folder
	if err != nil {
		return err
	}
	var want []map[string]any
	b, err := os.ReadFile(filepath.Join(dir, checkpointsExpected)) //nolint:gosec // G304: path inside the recorded release folder
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &want); err != nil {
		return fmt.Errorf("%s: %w", checkpointsExpected, err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return err
	}
	if _, err := provider.UpTo(ctx, m.SchemaVersion); err != nil {
		return fmt.Errorf("migrating to the recorded schema %d: %w", m.SchemaVersion, err)
	}
	if _, err := db.ExecContext(ctx, string(stmts)); err != nil {
		return fmt.Errorf("loading %s at schema %d: %w", checkpointsSQL, m.SchemaVersion, err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrating from schema %d to head: %w", m.SchemaVersion, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL TIME ZONE 'UTC'`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT to_jsonb(c) FROM home_checkpoints c ORDER BY id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	got := map[string]map[string]any{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var r map[string]any
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		got[fmt.Sprint(r["id"])] = r
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, w := range want {
		id := fmt.Sprint(w["id"])
		r, ok := got[id]
		if !ok {
			return fmt.Errorf("checkpoint %s is gone after migrating to head", id)
		}
		for k, wv := range w {
			// Both sides went through encoding/json, so numbers are float64 on both.
			wb, _ := json.Marshal(wv)
			gb, _ := json.Marshal(r[k])
			if string(wb) != string(gb) {
				return fmt.Errorf("checkpoint %s column %s: got %s, recorded %s", id, k, gb, wb)
			}
		}
	}
	return nil
}
