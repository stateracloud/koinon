// SPDX-License-Identifier: Apache-2.0

package keyscli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stateracloud/koinon/internal/secretstore"
)

// ErrVerifyFailed is returned by Verify when at least one stored value does not open.
var ErrVerifyFailed = errors.New("some stored values do not open with the key set")

// Verify opens every sealed value with the key set and reports how many open. It is read-only: it
// takes no row lock, writes no row and no audit event, and the plaintext is cleared at once. It is
// the check after a restore (docs: Back up and restore): the database and the keys come from two
// backups, and a pair that do not match shows as values that do not open, which this finds before a
// run needs one. It reads the key set and the rows it is given; it never wraps a key, so a KMS that
// is down shows as failures of the kind "unknown_kek" or "open", not as a changed row.
//
// It returns ErrVerifyFailed (joined with nothing else) when a value does not open, after it has
// printed one line per failing value: the table, the org and the row, and why. Nothing printed is
// secret. A value marked lost (keys forget) has no envelope and is not counted.
func (t *Tool) Verify(ctx context.Context) error {
	ks, err := t.Store.Load(ctx)
	if err != nil {
		return err
	}
	defer func() {
		for _, k := range ks.Keys {
			clear(k)
		}
	}()
	ring, err := t.ring(ks)
	if err != nil {
		return err
	}
	opened, failed := 0, 0
	kinds := map[string]int{}
	for _, c := range Columns {
		ids, err := t.rowIDs(ctx, c)
		if err != nil {
			return fmt.Errorf("list %s.%s: %w", c.Table, c.Column, err)
		}
		for _, r := range ids {
			err := t.verifyOne(ctx, c, r, ring)
			if err == nil {
				opened++
				continue
			}
			if errors.Is(err, pgx.ErrNoRows) {
				continue // deleted meanwhile
			}
			failed++
			kind := failureKind(err)
			kinds[kind]++
			_, _ = fmt.Fprintf(t.Out, "FAIL %s.%s org %s row %s: %s\n", c.Table, c.Column, r.org, r.id, kind)
		}
	}
	_, _ = fmt.Fprintf(t.Out, "%d value(s) opened, %d failed.\n", opened, failed)
	if failed > 0 {
		for _, k := range []string{"unknown_kek", "open", "envelope", "database"} {
			if kinds[k] > 0 {
				_, _ = fmt.Fprintf(t.Out, "  %s: %d (%s)\n", k, kinds[k], verifyHint(k))
			}
		}
		return ErrVerifyFailed
	}
	return nil
}

// verifyHint says what a failure kind usually means after a restore.
func verifyHint(kind string) string {
	switch kind {
	case "unknown_kek":
		return "the key set lacks the KEK that sealed the value: restore the key backup that holds it, then run keys status"
	case "open":
		return "the KEK is there but the value does not open: the keys and the database come from different installs or the value changed"
	case "envelope":
		return "the stored value is not a sealed envelope this build reads"
	}
	return "the database could not be read"
}

// verifyOne reads one value without locking it and opens it.
func (t *Tool) verifyOne(ctx context.Context, c Column, r rowRef, ring *secretstore.Keyring) error {
	var raw []byte
	var err error
	if c.singleton() {
		err = t.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE singleton`, c.Column, c.Table)).Scan(&raw)
	} else {
		err = t.Pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT %[2]s FROM %[1]s WHERE org_id = $1 AND %[3]s = $2`, c.Table, c.Column, c.rowKey()), r.org, r.id).Scan(&raw)
	}
	if err != nil {
		return err
	}
	if raw == nil {
		return pgx.ErrNoRows // marked lost meanwhile
	}
	var env secretstore.Envelope
	if err := json.Unmarshal(raw, &env); err != nil || secretstore.UnsupportedFormat(env) {
		return errUnreadable
	}
	plain, err := ring.Open(secretstore.Binding{OrgID: r.org, Table: c.Table, RowID: r.id, Column: c.Column}, env)
	if err != nil {
		return err
	}
	clear(plain)
	return nil
}
