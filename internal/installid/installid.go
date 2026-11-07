// SPDX-License-Identifier: Apache-2.0

// Package installid reads the install ID (old issue 615): the UUID a migration writes once into the
// database (table install_identity) and nothing changes afterwards. KMS-wrapped KEKs are bound to
// it (secretstore stored format 2), so a KEK Secret only unwraps next to the database it belongs to.
//
// Why the database: the KEKs protect what the database holds, so the ID is born with that data and
// travels with it, through a backup and restore, a helm reinstall that keeps the database, or a move
// to another namespace or cluster. A KEK Secret restored from another day still matches, since the
// row never changes; a KEK Secret next to another install's database does not.
//
// It also keeps the install's "bound only" marker (table kek_bound_only): once set, unbound KEKs
// (format 1) are refused, so an old KEK Secret backup stops opening. See [BoundOnlySource].
package installid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// ErrNotMigrated means the database has no install ID or no bound-only marker table yet: the
// migrations that create them have not run.
var ErrNotMigrated = errors.New("installid: the database is not migrated yet (no install_identity or kek_bound_only)")

func undefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// Read returns the install ID of the database.
func Read(ctx context.Context, db store.DBTX) (string, error) {
	id, err := store.New(db).GetInstallID(ctx)
	if undefinedTable(err) {
		return "", ErrNotMigrated
	}
	if err != nil {
		return "", fmt.Errorf("installid: read the install ID: %w", err)
	}
	s := id.String()
	if !secretstore.ValidInstallID(s) {
		return "", errors.New("installid: the database holds no valid install ID")
	}
	return s, nil
}

// Source returns the install ID source of a process: it reads the database when first asked and
// keeps the answer, since the row never changes. A failed read is not kept, so a process that
// started before the migration reads it on a later load of the key set.
func Source(db store.DBTX) secretstore.InstallIDSource {
	var (
		mu sync.Mutex
		id string
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if id != "" {
			return id, nil
		}
		got, err := Read(ctx, db)
		if err != nil {
			return "", err
		}
		id = got
		return id, nil
	}
}

// ReadBoundOnly returns the bound-only marker of the database (not set: no row).
func ReadBoundOnly(ctx context.Context, db store.DBTX) (secretstore.BoundOnly, error) {
	row, err := store.New(db).GetKEKBoundOnly(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return secretstore.BoundOnly{}, nil
	case undefinedTable(err):
		return secretstore.BoundOnly{}, ErrNotMigrated
	case err != nil:
		return secretstore.BoundOnly{}, fmt.Errorf("installid: read the bound-only marker: %w", err)
	}
	return secretstore.BoundOnly{Set: true, At: row.SetAt.Time, How: row.SetBy}, nil
}

// SetBoundOnly sets the bound-only marker, unless it is set already (the first setting stays), and
// returns it. Nothing in Koinon clears it.
func SetBoundOnly(ctx context.Context, db store.DBTX, how string) (secretstore.BoundOnly, error) {
	err := store.New(db).SetKEKBoundOnly(ctx, how)
	if undefinedTable(err) {
		return secretstore.BoundOnly{}, ErrNotMigrated
	}
	if err != nil {
		return secretstore.BoundOnly{}, fmt.Errorf("installid: set the bound-only marker: %w", err)
	}
	return ReadBoundOnly(ctx, db)
}

// BoundOnlySource returns the bound-only marker source of a process. While the marker is not set it
// reads the database every time it is asked, which is on every load of a key set that holds an
// unbound KMS KEK (at start and on each reload, about once a minute): a process that started before
// the marker was set refuses unbound KEKs from its next load on. A set marker is kept, since it is
// never cleared. A failed read is not kept and refuses the unbound KEK (fail closed).
func BoundOnlySource(db store.DBTX) secretstore.BoundOnlySource {
	var (
		mu  sync.Mutex
		set secretstore.BoundOnly
	)
	return func(ctx context.Context) (secretstore.BoundOnly, error) {
		mu.Lock()
		defer mu.Unlock()
		if set.Set {
			return set, nil
		}
		b, err := ReadBoundOnly(ctx, db)
		if err != nil {
			return secretstore.BoundOnly{}, err
		}
		if b.Set {
			set = b
		}
		return b, nil
	}
}

// Marker is the bound-only marker of one database, for the key-admin Job and keys status.
type Marker struct{ DB store.DBTX }

// BoundOnly reads the marker.
func (m Marker) BoundOnly(ctx context.Context) (secretstore.BoundOnly, error) {
	return ReadBoundOnly(ctx, m.DB)
}

// SetBoundOnly sets the marker (see [SetBoundOnly]).
func (m Marker) SetBoundOnly(ctx context.Context, how string) (secretstore.BoundOnly, error) {
	return SetBoundOnly(ctx, m.DB, how)
}

// Wait reads the install ID, waiting while the database is not migrated yet (on a new install the
// key-admin Job and the first migrations run side by side), until ctx ends: both the install ID and
// the bound-only marker table must be there, since the Job may set the marker. It retries every
// interval and logs the wait. When ctx ends, the error wraps ctx's error and the last read's.
func Wait(ctx context.Context, db store.DBTX, interval time.Duration) (string, error) {
	for {
		id, err := Read(ctx, db)
		if err == nil {
			_, err = ReadBoundOnly(ctx, db)
		}
		switch {
		case err == nil:
			return id, nil
		case ctx.Err() != nil:
			// The read may have failed because ctx ended in it: say so, whatever the read saw.
			return "", fmt.Errorf("%w: %w", err, ctx.Err())
		case !errors.Is(err, ErrNotMigrated):
			return "", err
		}
		slog.Info("waiting for the migrations that create the install ID and the bound-only marker")
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w: %w", err, ctx.Err())
		case <-time.After(interval):
		}
	}
}
