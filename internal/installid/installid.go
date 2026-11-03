// SPDX-License-Identifier: Apache-2.0

// Package installid reads the install ID (#615): the UUID a migration writes once into the
// database (table install_identity) and nothing changes afterwards. KMS-wrapped KEKs are bound to
// it (secretstore stored format 2), so a KEK Secret only unwraps next to the database it belongs to.
//
// Why the database: the KEKs protect what the database holds, so the ID is born with that data and
// travels with it, through a backup and restore, a helm reinstall that keeps the database, or a move
// to another namespace or cluster. A KEK Secret restored from another day still matches, since the
// row never changes; a KEK Secret next to another install's database does not.
package installid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/secretstore"
)

// ErrNotMigrated means the database has no install ID yet: the migration that creates it has not
// run.
var ErrNotMigrated = errors.New("installid: the database has no install ID yet (the migration that creates it has not run)")

// Read returns the install ID of the database.
func Read(ctx context.Context, db store.DBTX) (string, error) {
	id, err := store.New(db).GetInstallID(ctx)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" { // undefined_table
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

// Wait reads the install ID, waiting while the database is not migrated yet (on a new install the
// key-admin Job and the first migration run side by side), until ctx ends. It retries every
// interval and logs the wait. When ctx ends, the error wraps ctx's error and the last read's.
func Wait(ctx context.Context, db store.DBTX, interval time.Duration) (string, error) {
	for {
		id, err := Read(ctx, db)
		switch {
		case err == nil:
			return id, nil
		case ctx.Err() != nil:
			// The read may have failed because ctx ended in it: say so, whatever the read saw.
			return "", fmt.Errorf("%w: %w", err, ctx.Err())
		case !errors.Is(err, ErrNotMigrated):
			return "", err
		}
		slog.Info("waiting for the migration that creates the install ID")
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w: %w", err, ctx.Err())
		case <-time.After(interval):
		}
	}
}
