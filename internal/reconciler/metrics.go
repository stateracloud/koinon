// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/db/store"
)

// Error kinds of koinon_reconciler_pass_errors_total: a fixed set, never an error message or a name
// (internal/metrics states the label rule).
const (
	errKindDatabase        = "database"         // listing orgs, agents or providers failed
	errKindAgentPrepare    = "agent_prepare"    // an agent's key or desired state could not be prepared
	errKindProvider        = "provider"         // an org's model provider could not be rendered
	errKindMCP             = "mcp"              // an org MCP server could not be rendered or applied
	errKindSpendLimit      = "spend_limit"      // an agent's budget could not be computed
	errKindProviderKeys    = "provider_keys"    // delivering or pruning provider key files failed
	errKindGatewayApply    = "gateway_apply"    // the gateway was unreachable or the reload did not complete
	errKindGatewayRejected = "gateway_rejected" // the gateway rejected objects or keys while serving the rest
	errKindGatewayConfig   = "gateway_config"   // the gateway refused the whole config and kept the previous one (standalone mode)
	errKindSandbox         = "sandbox"          // converging an agent's sandbox failed
)

// sandboxStates are the label values of koinon_reconciler_sandboxes: the enum of the
// agent_sandboxes.state column.
var sandboxStates = []store.SandboxState{
	store.SandboxStatePending, store.SandboxStateStarting, store.SandboxStateReady, store.SandboxStateStopping,
	store.SandboxStateStopped, store.SandboxStateError, store.SandboxStateDeleting, store.SandboxStateDeleted,
	store.SandboxStateRetained,
}

// Metrics are the reconciler's Prometheus metrics (docs/site guide "Metrics").
type Metrics struct {
	PassDuration prometheus.Histogram
	Passes       *prometheus.CounterVec
	PassErrors   *prometheus.CounterVec
	GatewayApply prometheus.Histogram
	// GatewayConfigRejected is 1 while the last Apply was refused whole by the gateway.
	GatewayConfigRejected prometheus.Gauge
	Sandboxes             *prometheus.GaugeVec
	AgentsHeldBack        prometheus.Gauge
	SandboxRepairs        prometheus.Counter
	AgentsUnhealthy       prometheus.Counter
	// ObserverErrors counts failures of the sandbox observer (ADR 0078), by reason: forbidden,
	// not_found or other.
	ObserverErrors *prometheus.CounterVec
	// GatewayIDMismatch is 1 while the OpenShell gateway's gateway_id differs from openshell.gatewayId,
	// which leaves the sandbox admission policies matching none of its namespaces (old issue 475).
	GatewayIDMismatch prometheus.Gauge
}

// NewMetrics creates the metrics and registers them with reg (nil: not registered, for tests that
// do not look at them).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		PassDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "koinon_reconciler_pass_duration_seconds", Help: "Duration of one reconcile pass.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}),
		Passes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_reconciler_passes_total", Help: "Reconcile passes, by result (ok or error).",
		}, []string{"result"}),
		PassErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_reconciler_pass_errors_total", Help: "Errors in reconcile passes, by kind.",
		}, []string{"kind"}),
		GatewayApply: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "koinon_reconciler_gateway_apply_duration_seconds", Help: "Duration of applying the gateway state, including the wait for the reload.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}),
		GatewayConfigRejected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "koinon_reconciler_gateway_config_rejected", Help: "1 while the gateway refused the whole config of the last gateway apply and kept serving the previous one, else 0. The reason is in the reconciler log.",
		}),
		Sandboxes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "koinon_reconciler_sandboxes", Help: "Agent sandboxes by recorded state, at the end of the last pass.",
		}, []string{"state"}),
		AgentsHeldBack: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "koinon_reconciler_agents_waiting_for_gateway_key", Help: "Agents whose sandbox work was held back in the last pass because the gateway does not serve their new key yet.",
		}),
		SandboxRepairs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_reconciler_sandbox_repairs_total", Help: "Stuck, failed or completed sandboxes deleted to be recreated (ADR 0054).",
		}),
		AgentsUnhealthy: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_reconciler_agents_marked_unhealthy_total", Help: "Agents marked unhealthy because their sandbox kept failing.",
		}),
		ObserverErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_reconciler_sandbox_observer_errors_total",
			Help: "Failed reads or RoleBindings of the sandbox observer (ADR 0078), by reason (forbidden, not_found or other).",
		}, []string{"reason"}),
		GatewayIDMismatch: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "koinon_reconciler_openshell_gateway_id_mismatch",
			Help: "1 while the OpenShell gateway's gateway_id differs from openshell.gatewayId, so the sandbox admission policies match none of its namespaces and fail open, else 0.",
		}),
	}
	for _, k := range []string{"ok", "error"} {
		m.Passes.WithLabelValues(k)
	}
	for _, k := range []string{
		errKindDatabase, errKindAgentPrepare, errKindProvider, errKindMCP, errKindSpendLimit,
		errKindProviderKeys, errKindGatewayApply, errKindGatewayRejected, errKindGatewayConfig, errKindSandbox,
	} {
		m.PassErrors.WithLabelValues(k)
	}
	for _, k := range observerReasons {
		m.ObserverErrors.WithLabelValues(k)
	}
	for _, s := range sandboxStates {
		m.Sandboxes.WithLabelValues(string(s))
	}
	if reg != nil {
		reg.MustRegister(m.PassDuration, m.Passes, m.PassErrors, m.GatewayApply, m.GatewayConfigRejected, m.Sandboxes, m.AgentsHeldBack,
			m.SandboxRepairs, m.AgentsUnhealthy, m.ObserverErrors, m.GatewayIDMismatch)
	}
	return m
}
