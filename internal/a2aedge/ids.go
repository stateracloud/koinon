// SPDX-License-Identifier: Apache-2.0

package a2aedge

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func uuidString(id pgtype.UUID) string { return uuid.UUID(id.Bytes).String() }

// parseUUID parses a canonical UUID. Anything else is not an ID of this edge.
func parseUUID(s string) (pgtype.UUID, bool) {
	u, err := uuid.Parse(s)
	if err != nil || u.String() != s {
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: u, Valid: true}, true
}
