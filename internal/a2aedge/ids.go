// SPDX-License-Identifier: Apache-2.0

package a2aedge

import (
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
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

// taskID is the A2A task ID of turn n (counted from 1) of a thread: the thread's UUID, a dot and
// the turn number. The context ID is the UUID alone (ADR 0123 decision 2).
func taskID(thread pgtype.UUID, n int) a2a.TaskID {
	return a2a.TaskID(uuidString(thread) + "." + strconv.Itoa(n))
}

// parseTaskID splits a task ID of this edge into the thread and the turn. Anything else, such as a
// turn number with a sign or a leading zero, is not an ID of this edge.
func parseTaskID(s string) (pgtype.UUID, int, bool) {
	i := strings.LastIndexByte(s, '.')
	if i < 0 {
		return pgtype.UUID{}, 0, false
	}
	n, err := strconv.Atoi(s[i+1:])
	if err != nil || n < 1 || strconv.Itoa(n) != s[i+1:] {
		return pgtype.UUID{}, 0, false
	}
	id, ok := parseUUID(s[:i])
	return id, n, ok
}
