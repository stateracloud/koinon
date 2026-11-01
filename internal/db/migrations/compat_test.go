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

// eraseCompat matches a DELETE or TRUNCATE of schema_compat. Emptying the table erases the
// declaration: the gate then reads min_reader_version as 0 and lets every older release through.
var eraseCompat = regexp.MustCompile(`(?i)\b(DELETE\s+FROM|TRUNCATE(\s+TABLE)?(\s+ONLY)?)\s+(\w+\.)?"?schema_compat\b`)

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
		if eraseCompat.Match(body) {
			t.Errorf("%s deletes or truncates schema_compat: the declaration may only be raised, with `UPDATE schema_compat SET min_reader_version = %d`", e.Name(), v)
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

// TestCompatEraseIsCaught keeps the DELETE and TRUNCATE check honest.
func TestCompatEraseIsCaught(t *testing.T) {
	for _, sql := range []string{
		"DELETE FROM schema_compat;", "delete  from public.schema_compat", "TRUNCATE schema_compat;",
		"TRUNCATE TABLE schema_compat", "truncate only schema_compat",
	} {
		if !eraseCompat.MatchString(sql) {
			t.Errorf("%q must be caught", sql)
		}
	}
	for _, sql := range []string{"UPDATE schema_compat SET min_reader_version = 46", "DELETE FROM other WHERE x = 'schema_compat'"} {
		if eraseCompat.MatchString(sql) {
			t.Errorf("%q must not be caught", sql)
		}
	}
}
