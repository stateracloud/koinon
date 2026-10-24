// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func newUUID() pgtype.UUID { return pgtype.UUID{Bytes: uuid.Must(uuid.NewV7()), Valid: true} }

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
