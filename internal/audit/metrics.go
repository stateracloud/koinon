// SPDX-License-Identifier: Apache-2.0

package audit

import "github.com/prometheus/client_golang/prometheus"

// The chain label values of koinon_audit_writes_total.
const (
	ChainOrg     = "org"
	ChainInstall = "install"
)

// writes counts audit writes (ADR 0100, Metrics). The labels are fixed: the chain kind and the
// result, never an org, user, key or actor ID (ADR 0023). A write inside a transaction that rolls
// back later still counts as ok: the metric counts inserts, not commits.
var writes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "koinon_audit_writes_total",
	Help: "Audit events written, by chain (org, install) and result (ok, error).",
}, []string{"chain", "result"})

func countWrite(chain string, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	writes.WithLabelValues(chain, result).Inc()
}

// RegisterMetrics registers koinon_audit_writes_total on reg (nil: none) with every series at zero.
// The counter is shared by the process, as Record and RecordInstall are package functions; register
// it once per registry.
func RegisterMetrics(reg prometheus.Registerer) {
	for _, c := range []string{ChainOrg, ChainInstall} {
		for _, r := range []string{"ok", "error"} {
			writes.WithLabelValues(c, r)
		}
	}
	if reg != nil {
		reg.MustRegister(writes)
	}
}

// RegisterInstallWriterMetric registers koinon_install_audit_writer_ok (InstallWriterCheck) on reg.
// Only koinon-api writes the install chain, so only koinon-api registers it: a reconciler or agentd
// series would read 0 and fire the alert of docs/ops/secret-keys.md.
func RegisterInstallWriterMetric(reg prometheus.Registerer) { reg.MustRegister(installWriterOK) }
