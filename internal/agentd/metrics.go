// SPDX-License-Identifier: Apache-2.0

package agentd

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/db/store"
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
	Requeued    prometheus.Counter
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
		SandboxWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "koinon_agentd_sandbox_wait_duration_seconds", Help: "Time a run waited for its agent's sandbox to be ready, whatever the result.",
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}),
	}
	for _, o := range outcomes {
		m.Finished.WithLabelValues(string(o))
	}
	if reg != nil {
		reg.MustRegister(m.Claimed, m.Finished, m.Takeovers, m.SandboxWait, m.Requeued)
	}
	return m
}
