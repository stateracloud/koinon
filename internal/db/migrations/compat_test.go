// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stateracloud/koinon/internal/db/migrations"
)

// TestSupportedRange fails when MinSupported cannot be satisfied by the embedded migrations
// (ADR 0085, item 8). Raise it when the code now needs a migration.
func TestSupportedRange(t *testing.T) {
	latest, err := migrations.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if migrations.MinSupported < 1 || migrations.MinSupported > latest {
		t.Fatalf("MinSupported %d must be between 1 and the newest migration, %d: this build could never be Ready", migrations.MinSupported, latest)
	}
}

var minReader = regexp.MustCompile(`(?i)min_reader_version`)

// TestCompatDeclarations records the compatibility decision a migration makes. Only a migration
// that breaks the previous release may write schema_compat.min_reader_version after 00045, and it
// must be an UPDATE that names its own version: an expand migration cannot lock the older
// releases out by accident.
func TestCompatDeclarations(t *testing.T) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	updateOwn := regexp.MustCompile(`(?i)UPDATE\s+schema_compat\s+SET\s+min_reader_version\s*=\s*(\d+)`)
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, _ := strconv.ParseInt(num, 10, 64)
		if v <= 45 {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if !minReader.Match(body) {
			continue
		}
		m := updateOwn.FindSubmatch(body)
		if m == nil || string(m[1]) != strconv.FormatInt(v, 10) {
			t.Errorf("%s writes min_reader_version: it must be `UPDATE schema_compat SET min_reader_version = %d` (its own version)", e.Name(), v)
		}
	}
}
