// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"testing"

	"github.com/stateracloud/koinon/internal/db/migrations"
)

// TestSupportedRange fails when a migration is added without a decision on the range the binaries
// declare (ADR 0085, item 8): raise MaxSupported when the migration leaves the previous release
// working (expand/contract), and MinSupported when the code now needs it.
func TestSupportedRange(t *testing.T) {
	latest, err := migrations.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if migrations.MinSupported < 1 || migrations.MinSupported > migrations.MaxSupported {
		t.Fatalf("MinSupported %d, MaxSupported %d: need 1 <= min <= max", migrations.MinSupported, migrations.MaxSupported)
	}
	if migrations.MinSupported > latest {
		t.Fatalf("MinSupported %d is above the newest migration, %d: this build could never be Ready", migrations.MinSupported, latest)
	}
	if migrations.MaxSupported < latest {
		t.Fatalf("MaxSupported %d is below the newest migration, %d: a fresh install would not be Ready; raise it (and MinSupported if the code needs the migration)", migrations.MaxSupported, latest)
	}
}
