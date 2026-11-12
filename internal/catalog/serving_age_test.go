// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stateracloud/koinon/internal/db/store"
)

func TestServingOfSavedIsBoundedByAge(t *testing.T) {
	now := time.Now()
	row := func(age time.Duration) store.ListProviderServingRow {
		return store.ListProviderServingRow{Models: 2, Served: 1, ChangedAt: pgtype.Timestamptz{Time: now.Add(-age), Valid: true}}
	}
	if got := servingOf(Provider{}, row(2*time.Minute), now); got.State != ServingSaved {
		t.Fatalf("a fresh save is saved, got %+v", got)
	}
	got := servingOf(Provider{}, row(SavedGrace+time.Minute), now)
	if got.State != ServingFailed || got.Reason != ReasonUnknown || got.Detail != "not served 11 min after saving; see the reconciler log" {
		t.Fatalf("an old unserved save is failed, got %+v", got)
	}
	if got := servingOf(Provider{}, store.ListProviderServingRow{Models: 2, Served: 2, ChangedAt: row(time.Hour).ChangedAt}, now); got.State != ServingServing {
		t.Fatalf("a served provider stays serving, got %+v", got)
	}
}
