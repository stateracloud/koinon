// SPDX-License-Identifier: Apache-2.0

package prices

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/pricefetch"
)

// Fetch results, the values of the result label of koinon_price_source_fetch_total.
const (
	FetchOK      = "ok"
	FetchFailed  = "failed"
	FetchRefused = "refused"
)

var (
	proposalClasses = []string{string(pricefetch.ClassAuto), string(pricefetch.ClassNeedsReview), string(pricefetch.ClassConfirm), string(pricefetch.ClassRefused)}
	proposalStates  = []string{"open", "accepted", "dismissed", "superseded"}
)

// Metrics are the price-refresh metrics (ADR 0098 decision 5). Their label sets are fixed: the
// adapters, the results, the classes and the states, never a model, org, user or run ID (ADR 0023).
type Metrics struct {
	Fetches     *prometheus.CounterVec
	Proposals   *prometheus.GaugeVec
	AutoApplied *prometheus.CounterVec
	// Unpriced counts the enabled catalog models no price covers, by provider type (the types a
	// source prices: a fixed set).
	Unpriced *prometheus.GaugeVec
}

// NewMetrics registers the metrics on reg (nil: none registered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Fetches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_price_source_fetch_total", Help: "Fetches of a public price source, by source and result (ok, failed, refused).",
		}, []string{"source", "result"}),
		Proposals: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "koinon_price_proposals", Help: "Price proposals by class and state.",
		}, []string{"class", "state"}),
		AutoApplied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_price_auto_applied_total", Help: "Price proposals applied by the bounded auto-apply, by source.",
		}, []string{"source"}),
		Unpriced: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "koinon_price_unpriced_models", Help: "Enabled catalog models that no org, platform or default price covers, by provider type.",
		}, []string{"provider_type"}),
	}
	for _, pt := range pricefetch.PricedProviderTypes() {
		m.Unpriced.WithLabelValues(pt)
	}
	for _, s := range pricefetch.Sources {
		for _, r := range []string{FetchOK, FetchFailed, FetchRefused} {
			m.Fetches.WithLabelValues(s, r)
		}
		m.AutoApplied.WithLabelValues(s)
	}
	for _, c := range proposalClasses {
		for _, st := range proposalStates {
			m.Proposals.WithLabelValues(c, st)
		}
	}
	if reg != nil {
		reg.MustRegister(m.Fetches, m.Proposals, m.AutoApplied, m.Unpriced)
	}
	return m
}

// refreshProposals sets the proposal gauges from the table.
func (m *Metrics) refreshProposals(ctx context.Context, q *store.Queries) error {
	if m == nil {
		return nil
	}
	rows, err := q.CountPriceProposalsByClassState(ctx)
	if err != nil {
		return err
	}
	m.Proposals.Reset()
	for _, c := range proposalClasses {
		for _, st := range proposalStates {
			m.Proposals.WithLabelValues(c, st)
		}
	}
	for _, r := range rows {
		m.Proposals.WithLabelValues(r.Class, r.State).Set(float64(r.N))
	}
	return nil
}

func (m *Metrics) unpriced(providerType string, n int) {
	if m != nil {
		m.Unpriced.WithLabelValues(providerType).Set(float64(n))
	}
}

func (m *Metrics) fetched(source, result string) {
	if m != nil {
		m.Fetches.WithLabelValues(source, result).Inc()
	}
}
