// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/sinkclass"
)

// Every label of the guard's metrics has a fixed set of values (ADR 0023): decision, reason, sink
// class and phase. No org, agent, run, tool or server name.
var (
	actions = []Action{Pass, Refuse, Approve, NeedsDecision}
	reasons = []string{
		ReasonInternal, ReasonClean, ReasonBothLegs, ReasonTaint, ReasonByteCap, ReasonURLRule, ReasonCheck,
		ReasonError, ReasonAmbiguous, ReasonIdentity, ReasonNoRun, ReasonExempt, ReasonAskTarget, ReasonFeedStalled,
		ReasonReplyUnseen,
	}
	// classUnknown labels a decision taken before the class was known (identity, errors).
	classUnknown = "unknown"
)

// Metrics are the guard's Prometheus metrics, served by koinon-api.
type Metrics struct {
	Decisions *prometheus.CounterVec
	Duration  prometheus.Histogram
	// Evictions counts sessions whose taint store dropped fingerprints (a miss may then be text it
	// once held; the decision is not changed).
	Evictions prometheus.Counter
	// Legs counts responses that set a leg, by leg.
	Legs *prometheus.CounterVec
	// Unseen counts replies the guard passed without reading (#254): a tools/call that never ended in
	// a result it saw (an error reply or a cut call), or a reply of a method it has no reading for. No
	// label; the session they set both legs on is in the guard's warning.
	Unseen prometheus.Counter
}

// NewMetrics creates the metrics and registers them with reg (nil: not registered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_mcp_guard_decisions_total", Help: "Tool calls the MCP leak guard decided, by decision, reason and sink class.",
		}, []string{"decision", "reason", "sink_class"}),
		Duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "koinon_api_mcp_guard_check_seconds", Help: "Time the MCP leak guard took to decide a tool call.",
			Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 8},
		}),
		Evictions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_api_mcp_guard_taint_evictions_total", Help: "Times a session's taint store dropped its oldest fingerprints to stay within its bound.",
		}),
		Legs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_mcp_guard_legs_total", Help: "Tool results and feed reports that set a session leg, by leg.",
		}, []string{"leg"}),
		Unseen: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_api_mcp_guard_unseen_replies_total",
			Help: "Replies the MCP leak guard passed without reading (a tool call with no result it saw, or a reply of a method it cannot read), so their session holds both legs.",
		}),
	}
	for _, a := range actions {
		for _, r := range reasons {
			for _, c := range append([]string{classUnknown}, classNames()...) {
				m.Decisions.WithLabelValues(string(a), r, c)
			}
		}
	}
	m.Legs.WithLabelValues("untrusted_input")
	m.Legs.WithLabelValues("private_data")
	if reg != nil {
		reg.MustRegister(m.Decisions, m.Duration, m.Evictions, m.Legs, m.Unseen)
	}
	return m
}

func classNames() []string {
	out := make([]string, 0, len(sinkclass.Classes))
	for _, c := range sinkclass.Classes {
		out = append(out, string(c))
	}
	return out
}

func (m *Metrics) decided(v Verdict, class string, took time.Duration) {
	if m == nil {
		return
	}
	if class == "" {
		class = classUnknown
	}
	m.Decisions.WithLabelValues(string(v.Action), v.Reason, class).Inc()
	m.Duration.Observe(took.Seconds())
}

func (m *Metrics) evicted() {
	if m != nil {
		m.Evictions.Inc()
	}
}

func (m *Metrics) unseen(n int64) {
	if m != nil {
		m.Unseen.Add(float64(n))
	}
}

func (m *Metrics) legs(l Legs) {
	if m == nil {
		return
	}
	if l.UntrustedInput {
		m.Legs.WithLabelValues("untrusted_input").Inc()
	}
	if l.PrivateData {
		m.Legs.WithLabelValues("private_data").Inc()
	}
}

// Collectors are the metrics to register.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.Decisions, m.Duration, m.Evictions, m.Legs, m.Unseen}
}
