// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
)

// errDB is a database that fails every statement.
type errDB struct{}

var errDatabaseDown = errors.New("database unavailable")

func (errDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errDatabaseDown
}

func (errDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errDatabaseDown }

func (errDB) QueryRow(context.Context, string, ...any) pgx.Row { return errRow{} }

type errRow struct{}

func (errRow) Scan(...any) error { return errDatabaseDown }

// listingBackend lists one unmarked sandbox and has an install mark; nothing else of the backend is
// called by observeUnmarked.
type listingBackend struct{ execution.Backend }

func (listingBackend) Owner() string { return "koinon-test-mark" }

func (listingBackend) ListUnmarked(context.Context) ([]execution.UnmarkedObject, error) {
	return []execution.UnmarkedObject{{Workspace: "ws", Kind: "sandbox", Name: "sb"}}, nil
}

// A failed database read keeps both gauges at their last values and sets stale, as a failed gateway
// list does (owner_test.go): the gate never reads 0 from a list it could not classify.
func TestObserveUnmarkedDatabaseFailure(t *testing.T) {
	m := NewMetrics(nil)
	r := &Reconciler{backend: listingBackend{}, q: store.New(errDB{}), m: m, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for i, k := range unmarkedKinds {
		m.UnmarkedObjects.WithLabelValues(k).Set(float64(i + 1))
		m.UnmarkedWaiting.WithLabelValues(k).Set(float64(i + 10))
	}
	m.UnmarkedStale.Set(0)

	r.observeUnmarked(context.Background())

	if got := testutil.ToFloat64(m.UnmarkedStale); got != 1 {
		t.Errorf("stale = %v, want 1", got)
	}
	for i, k := range unmarkedKinds {
		if got := testutil.ToFloat64(m.UnmarkedObjects.WithLabelValues(k)); got != float64(i+1) {
			t.Errorf("unmarked %s = %v, want the last value %d", k, got, i+1)
		}
		if got := testutil.ToFloat64(m.UnmarkedWaiting.WithLabelValues(k)); got != float64(i+10) {
			t.Errorf("waiting %s = %v, want the last value %d", k, got, i+10)
		}
	}
}
