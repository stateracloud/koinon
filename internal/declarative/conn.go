// SPDX-License-Identifier: Apache-2.0

package declarative

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/connections"
)

func connectionConfig(d DeclaredConnection) ([]byte, error) {
	return json.Marshal(connections.GitHubConfig{AppID: d.AppID, InstallationID: d.InstallationID, Site: d.Site})
}

func newConnID() (pgtype.UUID, error) {
	id, err := uuid.NewV7()
	return pgtype.UUID{Bytes: id, Valid: true}, err
}

func orgSlug(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID) (string, error) {
	var slug string
	err := pool.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, id).Scan(&slug)
	return slug, err
}
