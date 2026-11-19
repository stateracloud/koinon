// SPDX-License-Identifier: Apache-2.0

package gatewaykeys

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// APIReloadInterval is how often koinon-api re-reads the active public key. A key leaves `retiring` only
// after more than this, so no koinon-api pod still seals to it (ADR 0097 12.4).
const APIReloadInterval = 30 * time.Second

// DefaultRetireGrace is the wait between `retiring` and `retired`: a few reload intervals.
const DefaultRetireGrace = 10 * APIReloadInterval

// DefaultLoadWindow is how fresh a gateway's load report must be to count (the gateway refreshes it
// every [ReportInterval]).
const DefaultLoadWindow = 3 * ReportInterval

// Pool is what the key-admin commands need of the database: queries and a transaction.
type Pool interface {
	store.DBTX
	Beginner
}

// Admin is the key-admin Job's side of the lifecycle. Only its identity may write the Secret and the
// table (the chart's kek-guard rule and the database grants).
type Admin struct {
	DB      Pool
	Secrets SecretStore
	Wrap    secretstore.Wrappers
	Suite   string // "" is the default suite
	// Replicas is how many gateway processes must report a pending key loaded before it is activated.
	Replicas   int
	LoadWindow time.Duration
	Grace      time.Duration
	Now        func() time.Time
	Logf       func(format string, args ...any)
}

// ErrNotLoaded means a pending key is not yet loaded by every gateway process: activation waits.
var ErrNotLoaded = errors.New("gatewaykeys: not every gateway process has loaded the pending key")

// ErrForeignSecret means the Secret holds entries that no table row names. The Job never adopts a Secret
// it did not make.
var ErrForeignSecret = errors.New("gatewaykeys: the gateway key Secret holds entries that no gateway_recipient_keys row names; the Job does not adopt them (ADR 0097 12.3)")

func (a Admin) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
	}
}

func (a Admin) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a Admin) window() time.Duration {
	if a.LoadWindow > 0 {
		return a.LoadWindow
	}
	return DefaultLoadWindow
}

func (a Admin) grace() time.Duration {
	if a.Grace > 0 {
		return a.Grace
	}
	return DefaultRetireGrace
}

func find(rows []Row, st State) *Row {
	for i := range rows {
		if rows[i].State == st {
			return &rows[i]
		}
	}
	return nil
}

// check refuses a Secret entry no row names.
func (a Admin) check(ctx context.Context, rows []Row) error {
	entries, _, err := a.Secrets.Read(ctx)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, r := range rows {
		known[r.KeyID] = true
	}
	for id := range entries {
		if !known[id] {
			return fmt.Errorf("%w: %s", ErrForeignSecret, id)
		}
	}
	return nil
}

// Add makes a new key and stores it as `pending`: the first key (init) or the next one (rotate). It
// writes the row first and the wrapped private key second. A failure between the two leaves a pending row
// with no Secret entry: Add retires that row at once (or a rerun does) and makes a new key, so no private
// key is ever stored without a row. It returns the new key's ID. With a pending key whose wrapped key is
// stored it returns that key and makes nothing (the command is safe to rerun).
func (a Admin) Add(ctx context.Context) (string, error) {
	rows, err := List(ctx, a.DB)
	if err != nil {
		return "", err
	}
	if p := find(rows, Pending); p != nil {
		entries, _, err := a.Secrets.Read(ctx)
		if err != nil {
			return "", err
		}
		if _, ok := entries[p.KeyID]; ok {
			a.logf("gateway key %s is pending already", p.KeyID)
			return p.KeyID, nil
		}
		if ok, err := Move(ctx, a.DB, p.KeyID, Pending, Retired); err != nil || !ok {
			return "", errors.Join(err, errChanged)
		}
		a.logf("gateway key %s was pending with no wrapped private key; it is retired", p.KeyID)
	}
	if err := a.check(ctx, rows); err != nil {
		return "", err
	}
	suite := a.Suite
	if suite == "" {
		suite = secretstore.DefaultRecipientSuite()
	}
	k, err := secretstore.GenerateRecipientKey(suite)
	if err != nil {
		return "", err
	}
	stored, err := a.Wrap.WrapRecipientKey(ctx, k)
	if err != nil {
		return "", err
	}
	if err := Insert(ctx, a.DB, k.Public()); err != nil {
		return "", err
	}
	if err := a.Secrets.Put(ctx, k.ID(), stored); err != nil {
		// Best effort: a rerun retires the row too.
		if _, mErr := Move(ctx, a.DB, k.ID(), Pending, Retired); mErr != nil {
			a.logf("gateway key %s: %v", k.ID(), mErr)
		}
		return "", fmt.Errorf("gatewaykeys: store the wrapped key %s: %w", k.ID(), err)
	}
	a.logf("gateway key %s is pending (%s)", k.ID(), stored.Backend)
	return k.ID(), nil
}

// Activate moves the pending key to `active` and the old active key to `retiring`, in one transaction,
// once every gateway process reported the pending key loaded. It returns ErrNotLoaded before that, and
// ErrNoPending when there is no pending key.
func (a Admin) Activate(ctx context.Context) (string, error) {
	rows, err := List(ctx, a.DB)
	if err != nil {
		return "", err
	}
	p := find(rows, Pending)
	if p == nil {
		return "", ErrNoPending
	}
	if a.Replicas > 0 {
		by, err := LoadedBy(ctx, a.DB, p.KeyID, a.window())
		if err != nil {
			return "", err
		}
		if len(by) < a.Replicas {
			return p.KeyID, fmt.Errorf("%w: %s is loaded by %d of %d", ErrNotLoaded, p.KeyID, len(by), a.Replicas)
		}
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if old := find(rows, Active); old != nil {
		if ok, err := Move(ctx, tx, old.KeyID, Active, Retiring); err != nil || !ok {
			return "", errors.Join(err, errChanged)
		}
	}
	if ok, err := Move(ctx, tx, p.KeyID, Pending, Active); err != nil || !ok {
		return "", errors.Join(err, errChanged)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	a.logf("gateway key %s is active", p.KeyID)
	return p.KeyID, nil
}

// ErrNoPending means there is no key to activate.
var ErrNoPending = errors.New("gatewaykeys: no pending key")

var errChanged = errors.New("gatewaykeys: the key table changed under the command; run it again")

// Retire finishes every `retiring` key whose wait is over and that no stored credential names: it marks
// the row `retired` and deletes the wrapped private key. It also deletes the Secret entry of a row that
// is already `retired` (a rerun after a failure between the two steps). It returns the retired IDs and
// the keys it kept, with the reason.
func (a Admin) Retire(ctx context.Context) (retired []string, kept map[string]string, err error) {
	rows, err := List(ctx, a.DB)
	if err != nil {
		return nil, nil, err
	}
	kept = map[string]string{}
	for _, r := range rows {
		switch r.State {
		case Retired:
			if err := a.Secrets.Delete(ctx, r.KeyID); err != nil {
				return retired, kept, err
			}
		case Retiring:
			if r.RetiringAt == nil || a.now().Sub(*r.RetiringAt) < a.grace() {
				kept[r.KeyID] = "the wait after it left active is not over"
				continue
			}
			reason, err := a.retireRow(ctx, r.KeyID)
			if err != nil {
				return retired, kept, err
			}
			if reason != "" {
				kept[r.KeyID] = reason
				continue
			}
			if err := a.Secrets.Delete(ctx, r.KeyID); err != nil {
				return retired, kept, err
			}
			retired = append(retired, r.KeyID)
		}
	}
	return retired, kept, nil
}

// retireRow moves one `retiring` row to `retired` in a transaction that holds the row lock while it counts
// the credentials sealed to the key. The credential trigger takes a shared lock on the same row, so a
// credential cannot be sealed to the key between the count and the move. It returns the reason to keep
// the key, or "" when it retired the row.
func (a Admin) retireRow(ctx context.Context, keyID string) (string, error) {
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM gateway_recipient_keys WHERE key_id = $1 FOR UPDATE`, keyID).Scan(&state); err != nil {
		return "", fmt.Errorf("gatewaykeys: lock key %s: %w", keyID, err)
	}
	if state != string(Retiring) {
		return "its state changed", nil
	}
	n, err := InUse(ctx, tx, keyID)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return fmt.Sprintf("%d stored credentials are still sealed to it (re-seal first)", n), nil
	}
	if ok, err := Move(ctx, tx, keyID, Retiring, Retired); err != nil {
		return "", err
	} else if !ok {
		return "its state changed", nil
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return "", nil
}
