// SPDX-License-Identifier: Apache-2.0

package declarative

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/gitea"
)

// connectionConfig is the non-secret config of a declared connection. prev is the stored config
// of the connection being updated, if any: a Gitea token that did not change keeps the date it
// was first entered, so the age warning is not reset by applying the same Secret again.
func connectionConfig(d DeclaredConnection, prev []byte, now time.Time) ([]byte, error) {
	if d.Connector == gitea.HostID {
		var p *gitea.Config
		if len(prev) > 0 {
			if c, err := gitea.ParseConfig(prev); err == nil {
				p = &c
			}
		}
		return json.Marshal(gitea.NewConfig(d.Site, d.ReadToken, d.WriteToken, p, now))
	}
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
