// SPDX-License-Identifier: Apache-2.0

package koinonapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// requireMCPV2Default is whether the migration Job refuses an upgrade while a stored MCP credential is
// still envelope v1 (ADR 0097 12.5 step 3). It is false until the release that drops the v1 read path of
// the reconciler and the gateway sets it to true. The flag --require-mcp-v2 turns it on before that.
const requireMCPV2Default = false

// ErrV1MCPCredentials means the upgrade preflight found MCP credentials that are not yet sealed to the
// gateway key.
var ErrV1MCPCredentials = errors.New("stored MCP credentials are still envelope v1")

// checkNoV1MCPCredentials is the upgrade preflight. It reads the envelope header of every stored MCP
// credential (never a value) and fails while any is not v2. A database with no mcp_servers table yet is a
// fresh install: nothing is stored, so nothing blocks.
func checkNoV1MCPCredentials(ctx context.Context, db *sql.DB) error {
	var n int64
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM mcp_servers WHERE credential IS NOT NULL AND (credential->>'fmt') IS DISTINCT FROM '2'`).Scan(&n)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("count the v1 MCP credentials: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w: %d credential(s). Run \"koinon-api keys seal-mcp --dry-run\", then \"keys seal-mcp\", and check that \"keys verify\" reports 0 in v1; no migration was applied", ErrV1MCPCredentials, n)
	}
	return nil
}
