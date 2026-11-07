// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/prices"
)

// The price metrics carry fixed label sets only (ADR 0023): never a model, org, user or run.
func TestMetricsLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	prices.NewMetrics(reg)
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"koinon_price_source_fetch_total": {"result", "source"},
		"koinon_price_proposals":          {"class", "state"},
		"koinon_price_auto_applied_total": {"source"},
	}
	for _, f := range fams {
		labels, ok := want[f.GetName()]
		if !ok {
			continue
		}
		delete(want, f.GetName())
		for _, m := range f.GetMetric() {
			var got []string
			for _, l := range m.GetLabel() {
				got = append(got, l.GetName())
			}
			if len(got) != len(labels) {
				t.Errorf("%s labels %v, want %v", f.GetName(), got, labels)
			}
		}
	}
	if len(want) != 0 {
		t.Errorf("metrics not exported (each series is pre-created at zero): %v", want)
	}
}
