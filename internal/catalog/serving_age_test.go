// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"testing"
	"time"

	"github.com/stateracloud/koinon/internal/db/store"
)

func TestServingOfSavedIsBoundedByAge(t *testing.T) {
	row := func(age time.Duration) store.ListProviderServingRow {
		return store.ListProviderServingRow{Models: 2, Served: 1, SavedAgeSeconds: age.Seconds()}
	}
	if got := servingOf(Provider{}, row(2*time.Minute)); got.State != ServingSaved {
		t.Fatalf("a fresh save is saved, got %+v", got)
	}
	cases := map[time.Duration]string{
		SavedGrace + time.Minute: "not served 11 min after saving; see the reconciler log",
		5 * time.Hour:            "not served 5 h after saving; see the reconciler log",
		72 * time.Hour:           "not served 3 days after saving; see the reconciler log",
	}
	for age, want := range cases {
		got := servingOf(Provider{}, row(age))
		if got.State != ServingFailed || got.Reason != ReasonUnknown || got.Detail != want {
			t.Fatalf("age %s: got %+v, want detail %q", age, got, want)
		}
	}
	served := store.ListProviderServingRow{Models: 2, Served: 2, SavedAgeSeconds: 3600}
	if got := servingOf(Provider{}, served); got.State != ServingServing {
		t.Fatalf("a served provider stays serving, got %+v", got)
	}
}
