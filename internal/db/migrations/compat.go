// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// The range of goose schema versions this build runs against (ADR 0085, item 8). koinon-api,
// koinon-agentd and koinon-reconciler read the version the database is at and stay not Ready,
// with a log line that says why, outside [MinSupported, MaxSupported]; see internal/schemagate.
//
//   - MinSupported is the oldest schema the code works against: raise it in the commit whose code
//     reads something a new migration adds. A schema below it means the migration Job has not run.
//   - MaxSupported is the newest schema this build tolerates. A migration that follows
//     expand/contract (nothing the older code reads is renamed, dropped or retyped) may raise it,
//     so that an app-only rollback within a minor line stays Ready. A migration that breaks the
//     previous release leaves it where it is: the older pods then refuse the new schema at start
//     instead of failing at their first query.
//
// TestSupportedRange keeps both inside what the embedded files can say, so adding a migration
// fails the test until the author has chosen.
const (
	MinSupported int64 = 44
	MaxSupported int64 = 44
)

// Latest returns the highest version among the embedded migration files.
func Latest() (int64, error) {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		return 0, err
	}
	var latest int64
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migration %q: version is not a number", e.Name())
		}
		latest = max(latest, v)
	}
	return latest, nil
}
