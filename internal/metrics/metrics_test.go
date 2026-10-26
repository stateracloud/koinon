// SPDX-License-Identifier: Apache-2.0

package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/api"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/metrics"
	"github.com/stateracloud/koinon/internal/reconciler"
)

func get(t *testing.T, srv *httptest.Server, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func contains(t *testing.T, body string, names ...string) {
	t.Helper()
	for _, n := range names {
		if !containsLine(body, n) {
			t.Errorf("/metrics lacks %s", n)
		}
	}
}

func containsLine(body, name string) bool { return strings.Contains("\n"+body, "\n"+name) }

// Each component's registry serves its metrics on /metrics, and only there.
func TestScrape(t *testing.T) {
	tests := []struct {
		component string
		register  func(prometheus.Registerer)
		names     []string
	}{
		{"reconciler", func(r prometheus.Registerer) { reconciler.NewMetrics(r) }, []string{
			"koinon_reconciler_passes_total{result=\"ok\"}", "koinon_reconciler_pass_errors_total{kind=\"gateway_apply\"}",
			"koinon_reconciler_pass_duration_seconds_count", "koinon_reconciler_gateway_apply_duration_seconds_count",
			"koinon_reconciler_sandboxes{state=\"ready\"}", "koinon_reconciler_agents_waiting_for_gateway_key",
			"koinon_reconciler_sandbox_repairs_total", "koinon_reconciler_agents_marked_unhealthy_total",
		}},
		{"agentd", func(r prometheus.Registerer) { agentd.NewMetrics(r) }, []string{
			"koinon_agentd_runs_claimed_total", "koinon_agentd_runs_finished_total{outcome=\"parked\"}",
			"koinon_agentd_takeovers_total", "koinon_agentd_sandbox_wait_duration_seconds_count",
		}},
		{"api", func(r prometheus.Registerer) {
			ledger.NewMetrics(r)
			api.NewMetrics(r).RPCRequests.WithLabelValues("/x.v1.S/M", "ok").Inc()
		}, []string{
			"koinon_api_ledger_records_ingested_total", "koinon_api_ledger_records_rejected_total{reason=\"unattributed\"}",
			"koinon_api_ledger_ingest_lag_seconds_count", "koinon_api_rpc_requests_total{code=\"ok\",procedure=\"/x.v1.S/M\"}",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.component, func(t *testing.T) {
			reg := metrics.NewRegistry()
			tc.register(reg)
			srv := httptest.NewServer(metrics.Handler(reg))
			defer srv.Close()

			code, body := get(t, srv, http.MethodGet, "/metrics")
			if code != http.StatusOK {
				t.Fatalf("GET /metrics = %d", code)
			}
			contains(t, body, append(tc.names, "go_goroutines", "process_open_fds")...)
			if code, _ := get(t, srv, http.MethodGet, "/"); code != http.StatusNotFound {
				t.Errorf("GET / = %d, want 404", code)
			}
			if code, _ := get(t, srv, http.MethodPost, "/metrics"); code == http.StatusOK {
				t.Errorf("POST /metrics = %d, want an error", code)
			}
		})
	}
}
