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
	errKindStateRevision   = "state_revision"   // storing a revision of the gateway State (the State stream) failed
	errKindSandbox         = "sandbox"          // converging an agent's sandbox, its provider record or its harness profile failed
)

// unmarkedKinds are the label values of koinon_openshell_unmarked_objects and
// koinon_openshell_unmarked_objects_waiting_for_agent.
var unmarkedKinds = []string{"sandbox", "provider", "profile"}

// sandboxStates are the label values of koinon_reconciler_sandboxes: the enum of the
// agent_sandboxes.state column.
var sandboxStates = []store.SandboxState{
	store.SandboxStatePending, store.SandboxStateStarting, store.SandboxStateReady, store.SandboxStateStopping,
	store.SandboxStateStopped, store.SandboxStateError, store.SandboxStateDeleting, store.SandboxStateDeleted,
	store.SandboxStateRetained,
}

// Metrics are the reconciler's Prometheus metrics (docs site, https://koinon.stateracloud.com/docs/operate/metrics/).
type Metrics struct {
	PassDuration prometheus.Histogram
	Passes       *prometheus.CounterVec
	PassErrors   *prometheus.CounterVec
	GatewayApply prometheus.Histogram
	// StateRevisions counts the revisions of the gateway State stored (ADR 0097 decision 2.1). No
	// labels: a revision, an org, an agent or a key is never a label (ADR 0023).
	StateRevisions prometheus.Counter
	// GatewayConfigRejected is 1 while the last Apply was refused whole by the gateway.
	GatewayConfigRejected prometheus.Gauge
	Sandboxes             *prometheus.GaugeVec
	AgentsHeldBack        prometheus.Gauge
	// UnmarkedObjects counts the OpenShell objects in this install's workspaces without the install
	// mark and without a known owner, from the gateway at most once per resync interval, by kind: every
	// reason of the unmarked list except waits-for-agent. UnmarkedWaiting counts the waits-for-agent
	// ones from the same list (issue 511). UnmarkedStale is 1 until a list has succeeded completely
	// and after one has failed (both gauges then keep the last good values).
	UnmarkedObjects *prometheus.GaugeVec
	UnmarkedWaiting *prometheus.GaugeVec
	UnmarkedStale   prometheus.Gauge
	SandboxRepairs  prometheus.Counter
	// SandboxCreationsHeld counts creations deferred by an org's creation rate (issue #27). No org label (ADR 0023).
	SandboxCreationsHeld prometheus.Counter
	AgentsUnhealthy      prometheus.Counter
	// ObserverErrors counts failures of the sandbox observer (ADR 0078), by reason: forbidden,
	// not_found or other.
	ObserverErrors *prometheus.CounterVec
	// LimitRangeErrors counts failures of the sandbox LimitRange writer (#76), by reason: forbidden,
	// not_found or other.
	LimitRangeErrors *prometheus.CounterVec
	// NetworkPolicyErrors counts failures of the sandbox default-deny NetworkPolicy writer (#993), by
	// reason: forbidden, not_found or other.
	NetworkPolicyErrors *prometheus.CounterVec
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
		StateRevisions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_reconciler_state_revisions_total", Help: "Revisions of the gateway State stored for the State stream (a revision is stored only when the state changed).",
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
		UnmarkedObjects: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "koinon_openshell_unmarked_objects", Help: "OpenShell sandboxes, providers and profiles in this install's workspaces without its install mark and without a known owner, from the gateway at most once per resync interval, by kind (sandbox, provider or profile): every line of koinon-reconciler unmarked list except waits-for-agent. Read with koinon_openshell_unmarked_objects_stale: 0 for every kind and stale 0 means no object without a known owner is left; waits-for-agent objects are counted apart and do not block (ADR 0106).",
		}, []string{"kind"}),
		UnmarkedWaiting: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "koinon_openshell_unmarked_objects_waiting_for_agent", Help: "OpenShell objects in this install's workspaces without its install mark that wait for their agent's status to change (the waits-for-agent lines of koinon-reconciler unmarked list: a draft agent's stopped sandbox and the providers it attaches, an archived agent's retained sandbox), by kind, from the same list as koinon_openshell_unmarked_objects. They do not block the drill gate (ADR 0106).",
		}, []string{"kind"}),
		UnmarkedStale: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "koinon_openshell_unmarked_objects_stale", Help: "1 until the list behind koinon_openshell_unmarked_objects and koinon_openshell_unmarked_objects_waiting_for_agent has succeeded completely, after a list that failed (the object gauges then show the last good values), and when ownership is off (no install mark: nothing can be claimed, so nothing is safe); 0 after a good count.",
		}),
		SandboxRepairs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_reconciler_sandbox_repairs_total", Help: "Stuck, failed or completed sandboxes deleted to be recreated (ADR 0054).",
		}),
		SandboxCreationsHeld: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_reconciler_sandbox_creations_held_total", Help: "Reconciler passes that deferred a sandbox creation or start (counted per pass, not per sandbox) because the org was at its sandbox creations per minute (issue #27, ADR 0066).",
		}),
		AgentsUnhealthy: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "koinon_reconciler_agents_marked_unhealthy_total", Help: "Agents marked unhealthy because their sandbox kept failing.",
		}),
		ObserverErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_reconciler_sandbox_observer_errors_total",
			Help: "Failed reads or RoleBindings of the sandbox observer (ADR 0078), by reason (forbidden, not_found or other).",
		}, []string{"reason"}),
		LimitRangeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_reconciler_sandbox_limit_range_errors_total",
			Help: "Failed Applies of the sandbox LimitRange (#76), by reason (forbidden, not_found or other).",
		}, []string{"reason"}),
		NetworkPolicyErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_reconciler_sandbox_network_policy_errors_total",
			Help: "Failed Applies of the sandbox default-deny NetworkPolicy (#993), by reason (forbidden, not_found or other).",
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
		errKindProviderKeys, errKindGatewayApply, errKindGatewayRejected, errKindGatewayConfig, errKindStateRevision, errKindSandbox,
	} {
		m.PassErrors.WithLabelValues(k)
	}
	for _, k := range observerReasons {
		m.ObserverErrors.WithLabelValues(k)
		m.LimitRangeErrors.WithLabelValues(k)
		m.NetworkPolicyErrors.WithLabelValues(k)
	}
	for _, k := range unmarkedKinds {
		m.UnmarkedObjects.WithLabelValues(k)
		m.UnmarkedWaiting.WithLabelValues(k)
	}
	m.UnmarkedStale.Set(1) // nothing has been counted yet
	for _, s := range sandboxStates {
		m.Sandboxes.WithLabelValues(string(s))
	}
	if reg != nil {
		reg.MustRegister(m.PassDuration, m.Passes, m.PassErrors, m.GatewayApply, m.StateRevisions, m.GatewayConfigRejected, m.Sandboxes, m.AgentsHeldBack, m.UnmarkedObjects, m.UnmarkedWaiting, m.UnmarkedStale,
			m.SandboxRepairs, m.SandboxCreationsHeld, m.AgentsUnhealthy, m.ObserverErrors, m.LimitRangeErrors, m.NetworkPolicyErrors, m.GatewayIDMismatch)
	}
	return m
}
