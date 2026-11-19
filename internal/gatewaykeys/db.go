// SPDX-License-Identifier: Apache-2.0

// Package gatewaykeys is the lifecycle of the gateway's recipient key (ADR 0097 decision 12, #1151): the
// public rows (table gateway_recipient_keys), the wrapped private keys (a Secret that only the gateway
// mounts), the key-admin commands that make them (init, rotate, activate, retire), the gateway's load of
// its own keys, and koinon-api's load of the active public key. Nothing here opens a credential: that is
// slice 3.
package gatewaykeys

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// State is the state of a key (ADR 0097 12.4).
type State string

// The states of a key, in the order a key moves through them.
const (
	Pending  State = "pending"
	Active   State = "active"
	Retiring State = "retiring"
	Retired  State = "retired"
)

// Row is one public key of the table.
type Row struct {
	KeyID       string
	Recipient   string
	Public      []byte
	State       State
	CreatedAt   time.Time
	ActivatedAt *time.Time
	RetiringAt  *time.Time
	RetiredAt   *time.Time
}

// PublicKey returns the key of the row. It refuses a key whose ID is not the hash of its public bytes.
func (r Row) PublicKey() (secretstore.RecipientPublicKey, error) {
	p, err := secretstore.NewRecipientPublicKey(r.Recipient, r.Public)
	if err != nil {
		return secretstore.RecipientPublicKey{}, fmt.Errorf("gateway key %q: %w", r.KeyID, err)
	}
	if err := p.CheckID(r.KeyID); err != nil {
		return secretstore.RecipientPublicKey{}, err
	}
	return p, nil
}

// ErrNotMigrated means the table does not exist: the migrations have not run.
var ErrNotMigrated = errors.New("gatewaykeys: the database has no gateway_recipient_keys table (migration 00097)")

func undefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// List returns every row, oldest first.
func List(ctx context.Context, db store.DBTX) ([]Row, error) {
	rows, err := db.Query(ctx, `SELECT key_id, recipient, public_key, state, created_at, activated_at, retiring_at, retired_at
		FROM gateway_recipient_keys ORDER BY created_at, key_id`)
	if undefinedTable(err) {
		return nil, ErrNotMigrated
	}
	if err != nil {
		return nil, fmt.Errorf("gatewaykeys: list the keys: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var st string
		if err := rows.Scan(&r.KeyID, &r.Recipient, &r.Public, &st, &r.CreatedAt, &r.ActivatedAt, &r.RetiringAt, &r.RetiredAt); err != nil {
			return nil, fmt.Errorf("gatewaykeys: read a key row: %w", err)
		}
		r.State = State(st)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		if undefinedTable(err) {
			return nil, ErrNotMigrated
		}
		return nil, fmt.Errorf("gatewaykeys: list the keys: %w", err)
	}
	return out, nil
}

// Insert adds a public key in the state pending. Only the key-admin role may.
func Insert(ctx context.Context, db store.DBTX, p secretstore.RecipientPublicKey) error {
	_, err := db.Exec(ctx, `INSERT INTO gateway_recipient_keys (key_id, recipient, public_key, state) VALUES ($1, $2, $3, 'pending')`,
		p.ID(), p.Suite(), p.Bytes())
	if undefinedTable(err) {
		return ErrNotMigrated
	}
	if err != nil {
		return fmt.Errorf("gatewaykeys: insert key %s: %w", p.ID(), err)
	}
	return nil
}

// Move changes the state of a key from one state to the next and reports whether the row was in from.
// The database trigger refuses any other move.
func Move(ctx context.Context, db store.DBTX, keyID string, from, to State) (bool, error) {
	tag, err := db.Exec(ctx, `UPDATE gateway_recipient_keys SET state = $3::text,
			activated_at = CASE WHEN $3::text = 'active' THEN now() ELSE activated_at END,
			retiring_at = CASE WHEN $3::text = 'retiring' THEN now() ELSE retiring_at END,
			retired_at = CASE WHEN $3::text = 'retired' THEN now() ELSE retired_at END
		WHERE key_id = $1 AND state = $2::text`, keyID, string(from), string(to))
	if err != nil {
		return false, fmt.Errorf("gatewaykeys: move key %s from %s to %s: %w", keyID, from, to, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Beginner is what Activate needs of a pool or a connection.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// InUse counts the stored MCP credentials sealed to the key. The key-admin role cannot read the sealed
// column: the count comes from a function that runs with the owner's right.
func InUse(ctx context.Context, db store.DBTX, keyID string) (int64, error) {
	var n int64
	if err := db.QueryRow(ctx, `SELECT gateway_recipient_key_in_use($1)`, keyID).Scan(&n); err != nil {
		return 0, fmt.Errorf("gatewaykeys: count the credentials sealed to %s: %w", keyID, err)
	}
	return n, nil
}

// ReportLoaded is the gateway's report that this process (copyID) holds the private key of keyID.
func ReportLoaded(ctx context.Context, db store.DBTX, keyID, copyID string) error {
	_, err := db.Exec(ctx, `INSERT INTO gateway_recipient_key_loads (key_id, copy_id) VALUES ($1, $2)
		ON CONFLICT (key_id, copy_id) DO UPDATE SET loaded_at = now()`, keyID, copyID)
	if err != nil {
		return fmt.Errorf("gatewaykeys: report key %s loaded: %w", keyID, err)
	}
	return nil
}

// LoadedBy returns the processes that reported keyID loaded within the last window.
func LoadedBy(ctx context.Context, db store.DBTX, keyID string, window time.Duration) ([]string, error) {
	rows, err := db.Query(ctx, `SELECT copy_id FROM gateway_recipient_key_loads
		WHERE key_id = $1 AND loaded_at > now() - $2::interval ORDER BY copy_id`, keyID, fmt.Sprintf("%d seconds", int(window.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("gatewaykeys: read the load reports of %s: %w", keyID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
