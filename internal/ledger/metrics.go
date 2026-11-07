// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Rejection reasons of koinon_api_ledger_records_rejected_total: a fixed set, never the free text
// a driver reports to the exporter (internal/metrics states the label rule).
const (
	RejectInvalidAgent  = "invalid_agent"      // the driver could not read the agent as org/agent-id
	RejectInvalidReq    = "invalid_request_id" // missing or over-long request ID
	RejectMissingKey    = "missing_key"        // no gateway key ID
	RejectNegativeToken = "negative_tokens"
	RejectInvalidCost   = "invalid_cost"
	rejectUnattributed  = "unattributed" // the org or agent does not exist
)

var rejectReasons = []string{RejectInvalidAgent, RejectInvalidReq, RejectMissingKey, RejectNegativeToken, RejectInvalidCost, rejectUnattributed}

// Metrics are the spend ledger's Prometheus metrics, served by koinon-api.
type Metrics struct {
	// Ingested counts new entries by principal ("agent" or "system": a fixed label set).
	Ingested  *prometheus.CounterVec
	Rejected  *prometheus.CounterVec
	IngestLag prometheus.Histogram
}

// Reject counts one rejected record of the given kind (a Reject* constant). It is safe on nil Metrics.
func (m *Metrics) Reject(kind string) {
	if m != nil {
		m.Rejected.WithLabelValues(kind).Inc()
	}
}

// NewMetrics creates the metrics and registers them with reg (nil: not registered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Ingested: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_ledger_records_ingested_total", Help: "Access-log records recorded as new spend entries, by principal (replays are not counted).",
		}, []string{"principal"}),
		Rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_ledger_records_rejected_total", Help: "Access-log records that looked charged but could not be attributed or were malformed, by reason.",
		}, []string{"reason"}),
		IngestLag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "koinon_api_ledger_ingest_lag_seconds", Help: "Time from a request's access-log record to its ingestion, for newly recorded entries.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 900, 3600},
		}),
	}
	m.Ingested.WithLabelValues(PrincipalAgent)
	m.Ingested.WithLabelValues(PrincipalSystem)
	for _, r := range rejectReasons {
		m.Rejected.WithLabelValues(r)
	}
	if reg != nil {
		reg.MustRegister(m.Ingested, m.Rejected, m.IngestLag)
	}
	return m
}
