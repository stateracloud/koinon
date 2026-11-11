// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// What this build requires of the schema (ADR 0085, item 8). koinon-api, koinon-agentd and
// koinon-reconciler read the schema at startup and every 15 s (internal/schemagate) and stay not
// Ready, with a log line that says why, unless both hold:
//
//   - the goose version is at least MinSupported, the oldest schema the code works against. Raise
//     it in the commit whose code reads something a new migration adds. A schema below it means the
//     migration Job has not run.
//   - the schema's min_reader_version (table schema_compat, written by migrations) is at most
//     Latest(), the newest migration this build embeds. The newer schema declares who can still
//     read it, so a release that shipped earlier needs no prediction: an expand migration leaves
//     min_reader_version alone and every older release in the minor line stays Ready (an image
//     rollback works); a breaking migration sets it to its own version in the same file.
//
// TestSupportedRange keeps MinSupported inside what the embedded files can say, and
// TestCompatDeclarations pins min_reader_version to the migrations that write it.
const MinSupported int64 = 70

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

// MustLatest is Latest for a binary's startup: the embedded files are fixed at build time, so an
// error means a broken build.
func MustLatest() int64 {
	v, err := Latest()
	if err != nil {
		panic(err)
	}
	return v
}
