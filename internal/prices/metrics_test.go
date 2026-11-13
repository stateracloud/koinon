// SPDX-License-Identifier: Apache-2.0

package prices_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/pricefetch"
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
		"koinon_price_source_fetch_total":   {"result", "source"},
		"koinon_price_proposals":            {"class", "state"},
		"koinon_price_auto_applied_total":   {"source"},
		"koinon_price_unpriced_models":      {"provider_type"},
		"koinon_price_platform_bad_entries": nil,
	}
	seen := map[string]bool{}
	for _, f := range fams {
		if f.GetName() == "koinon_price_source_fetch_total" {
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "source" {
						seen[l.GetValue()] = true
					}
				}
			}
		}
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
	// Every adapter's source id is a pre-created label value, and nothing else is one.
	for _, src := range pricefetch.Sources {
		if !seen[src] {
			t.Errorf("koinon_price_source_fetch_total has no series for source %q", src)
		}
		delete(seen, src)
	}
	if len(seen) != 0 {
		t.Errorf("koinon_price_source_fetch_total has sources that are no adapter: %v", seen)
	}
	if len(want) != 0 {
		t.Errorf("metrics not exported (each series is pre-created at zero): %v", want)
	}
}
