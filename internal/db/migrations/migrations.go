// SPDX-License-Identifier: Apache-2.0

// Package migrations embeds the goose SQL migrations. File names are
// zero-padded (NNNNN_name.sql) so goose and sqlc read them in the same order.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

// FS holds every migration file.
//
//go:embed *.sql
var FS embed.FS

// Up applies every pending migration. Callers connect as the migration role,
// which owns the schema; the application roles only get DML rights.
func Up(ctx context.Context, db *sql.DB) ([]*goose.MigrationResult, error) {
	provider, err := goose.NewProvider(goose.DialectPostgres, db, FS)
	if err != nil {
		return nil, fmt.Errorf("goose provider: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return results, fmt.Errorf("migrate up: %w", err)
	}
	return results, nil
}
