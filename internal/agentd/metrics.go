// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/runlimits"
	"github.com/stateracloud/koinon/internal/runoutcome"
)

// outcomes are the label values of koinon_agentd_runs_finished_total: the run states agentd moves
// a run into (succeeded, failed, canceled) and parked, where the run ends its turn on approvals.
// A control-plane cancel (cap, spend limit, a user) is made by koinon-api and not counted here.
var outcomes = []store.RunState{
	store.RunStateSucceeded, store.RunStateFailed, store.RunStateCanceled, store.RunStateParked,
}

// Metrics are koinon-agentd's Prometheus metrics. No label carries an org, agent, task or run ID
// (internal/metrics states the label rule).
type Metrics struct {
	Claimed     prometheus.Counter
	Finished    *prometheus.CounterVec
	Takeovers   prometheus.Counter
	SandboxWait prometheus.Histogram
	ModelWait   prometheus.Histogram
	Requeued    prometheus.Counter
	// SandboxWaitCap counts runs failed because their sandbox never became ready, by cause.
	SandboxWaitCap *prometheus.CounterVec
	// WrapUpNotices counts runs whose agent was told a run limit is near, by the limit (ADR 0081).
	WrapUpNotices *prometheus.CounterVec
	// TimeLimitStops counts runs this replica stopped at their time limit.
	TimeLimitStops prometheus.Counter
	// TraceSpansDropped counts run spans that were not exported, by reason (queue_full,
	// export_failed); TraceSpansExported those the listener accepted (ADR 0074).
	TraceSpansDropped  *prometheus.CounterVec
	TraceSpansExported prometheus.Counter
	// RunEventWriteFailures counts run events that could not be written after their tries, by class
	// (diagnostic: dropped, the run went on; business: the run was abandoned) and reason (ADR 0074,
	// amendment of 2026-10-31).
	RunEventWriteFailures *prometheus.CounterVec
	// TaintReports counts the leak guard feed's reports of finished tool calls, by the leg they set
	// (private_data, untrusted_input) and result (recorded, failed).
	TaintReports *prometheus.CounterVec
	// SweepRefused is how many retired home repositories the last repository sweep refused to
	// delete because their stored prefix is not their own (ADR 0085 decision 13).
	SweepRefused prometheus.Gauge
}

// NewMetrics creates the metrics and registers them with reg (nil: not registered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Claimed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_agentd_runs_claimed_total", Help: "Runs claimed by this replica's workers (new claims, not takeovers).",
		}),
		Finished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_agentd_runs_finished_total", Help: "Runs ended or parked by this replica, by outcome (succeeded, failed, canceled, parked).",
		}, []string{"outcome"}),
		Takeovers: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_agentd_takeovers_total", Help: "Runs this replica took over after their owner's lease expired or was released.",
		}),
		Requeued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_agentd_runs_requeued_total", Help: "Runs sent back to the queue because their sandbox was not ready in time.",
		}),
		SandboxWaitCap: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_agentd_sandbox_wait_cap_total", Help: "Runs failed because their sandbox never became ready within the org's cap, by cause.",
		}, []string{"cause"}),
		WrapUpNotices: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_agentd_wrap_up_notices_total", Help: "Runs whose agent was told a run limit is near, by the limit.",
		}, []string{"limit"}),
		TimeLimitStops: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_agentd_time_limit_stops_total", Help: "Runs this replica stopped at their time limit (the api's sweep is the backstop and is not counted).",
		}),
		TraceSpansDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_agentd_trace_spans_dropped_total", Help: "Run spans dropped instead of exported, by reason (queue_full, export_failed, rejected, shutdown).",
		}, []string{"reason"}),
		TraceSpansExported: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_agentd_trace_spans_exported_total", Help: "Run spans the OTLP listener accepted.",
		}),
		RunEventWriteFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_agentd_run_event_write_failures_total", Help: "Run events that could not be written after their tries, by class (diagnostic: dropped and the run went on; business: the run was abandoned) and reason (timeout, conn, transient, other).",
		}, []string{"class", "reason"}),
		TaintReports: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_agentd_taint_reports_total", Help: "Finished tool calls of a sandbox reported to the MCP leak guard, by the leg they set (private_data, untrusted_input) and result (recorded; failed: the run was given up).",
		}, []string{"leg", "result"}),
		SweepRefused: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "koinon_agentd_home_repository_sweep_refused", Help: "Retired home repositories the last repository sweep did not delete because their stored prefix is not their own; anything above 0 needs a person (the log names each once).",
		}),
		SandboxWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "koinon_agentd_sandbox_wait_duration_seconds", Help: "Time a run waited for its agent's sandbox to be ready, whatever the result.",
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}),
		ModelWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "koinon_agentd_model_wait_duration_seconds", Help: "Time a run waited, before starting its harness, for the gateway to serve its model; observed only for runs that waited and then were served or timed out (not for a wait that ended in a cancel, a lost lease or a shutdown).",
			Buckets: []float64{1, 5, 10, 30, 60, 90, 120, 180, 300, 600},
		}),
	}
	for _, o := range outcomes {
		m.Finished.WithLabelValues(string(o))
	}
	for _, k := range runlimits.Kinds {
		m.WrapUpNotices.WithLabelValues(string(k))
	}
	for _, c := range execution.WaitCauses {
		m.SandboxWaitCap.WithLabelValues(string(c))
	}
	for _, r := range []string{dropQueueFull, dropExportFailed, dropRejected, dropShutdown} {
		m.TraceSpansDropped.WithLabelValues(r)
	}
	for _, c := range []runoutcome.EventClass{runoutcome.ClassBusiness, runoutcome.ClassDiagnostic} {
		for _, r := range runoutcome.WriteFailures {
			m.RunEventWriteFailures.WithLabelValues(c.String(), string(r))
		}
	}
	for _, l := range []string{legPrivate, legUntrusted} {
		for _, r := range []string{taintRecorded, taintFailed} {
			m.TaintReports.WithLabelValues(l, r)
		}
	}
	if reg != nil {
		reg.MustRegister(m.TaintReports, m.Claimed, m.Finished, m.Takeovers, m.SandboxWait, m.ModelWait, m.Requeued,
			m.SandboxWaitCap, m.WrapUpNotices, m.TimeLimitStops, m.TraceSpansDropped, m.TraceSpansExported,
			m.RunEventWriteFailures, m.SweepRefused)
	}
	return m
}

// outcomeMetrics counts what the run owner's verbs did (internal/runoutcome).
type outcomeMetrics struct{ m *Metrics }

func (o outcomeMetrics) Finished(state store.RunState) {
	o.m.Finished.WithLabelValues(string(state)).Inc()
}

func (o outcomeMetrics) Requeued() { o.m.Requeued.Inc() }

func (o outcomeMetrics) SandboxWaitCap(cause execution.SandboxWaitCause) {
	o.m.SandboxWaitCap.WithLabelValues(string(cause)).Inc()
}

func (o outcomeMetrics) EventWriteFailed(class runoutcome.EventClass, reason runoutcome.WriteFailure) {
	o.m.RunEventWriteFailures.WithLabelValues(class.String(), string(reason)).Inc()
}

// The labels of TaintReports.
const (
	legPrivate    = "private_data"
	legUntrusted  = "untrusted_input"
	taintRecorded = "recorded"
	taintFailed   = "failed"
)

// taintReport counts one report of the leak guard's feed.
func (m *Metrics) taintReport(legs mcpguard.Legs, ok bool) {
	result := taintRecorded
	if !ok {
		result = taintFailed
	}
	if legs.PrivateData {
		m.TaintReports.WithLabelValues(legPrivate, result).Inc()
	}
	if legs.UntrustedInput {
		m.TaintReports.WithLabelValues(legUntrusted, result).Inc()
	}
}
