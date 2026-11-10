// SPDX-License-Identifier: Apache-2.0

package restorehold_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spf13/pflag"

	"github.com/stateracloud/koinon/internal/restorehold"
)

func TestFlagIsOffByDefault(t *testing.T) {
	var held bool
	fs := pflag.NewFlagSet("x", pflag.ContinueOnError)
	restorehold.Flag(fs, &held)
	if err := fs.Parse(nil); err != nil || held {
		t.Fatalf("no hold without the flag: held=%v err=%v", held, err)
	}
	if err := fs.Parse([]string{"--restore-hold"}); err != nil || !held {
		t.Fatalf("--restore-hold holds: held=%v err=%v", held, err)
	}
}

func TestMetricShowsTheHold(t *testing.T) {
	for held, want := range map[bool]float64{true: 1, false: 0} {
		reg := prometheus.NewRegistry()
		restorehold.Metric(reg, held)
		if got := testutil.ToFloat64(gaugeOf(t, reg)); got != want {
			t.Errorf("held=%v: koinon_restore_hold=%v, want %v", held, got, want)
		}
	}
}

func gaugeOf(t *testing.T, reg *prometheus.Registry) prometheus.Collector {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil || len(mfs) != 1 || mfs[0].GetName() != "koinon_restore_hold" {
		t.Fatalf("one koinon_restore_hold metric, got %v (%v)", mfs, err)
	}
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "copy"})
	g.Set(mfs[0].GetMetric()[0].GetGauge().GetValue())
	return g
}

// Wait blocks until the context ends and then returns nil: a hold never ends by itself and never
// does the component's work.
func TestWaitBlocksUntilTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- restorehold.Wait(ctx, slog.New(slog.DiscardHandler), "koinon-test") }()
	select {
	case err := <-done:
		t.Fatalf("Wait returned before the context ended: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the context ended")
	}
}
