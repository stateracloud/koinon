// SPDX-License-Identifier: Apache-2.0

// Package metrics gives each Koinon component its Prometheus registry and the listener that serves
// it (ADR 0023, amendment 2026-10-26).
//
// Label rule, for every metric in every component: label values come from small fixed sets
// (a state, an outcome, an error kind, an RPC procedure and code). Never an org, agent, user,
// ticket or run ID, a name or a free-text message: that would make the series count grow with the
// data and leak tenant names to whoever scrapes. The /metrics endpoint has no authentication; the
// chart's NetworkPolicy decides who can reach it.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// DefaultPort is the port every component serves /metrics on (each is its own pod).
const DefaultPort = ":9090"

// NewRegistry returns a registry of its own (not the process-wide default) with the Go runtime and
// process collectors.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// Handler serves only /metrics, from reg.
func Handler(reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	return mux
}

// Serve serves reg on addr until ctx ends. An empty addr disables the listener.
func Serve(ctx context.Context, addr string, reg *prometheus.Registry, log *slog.Logger) {
	if addr == "" {
		return
	}
	srv := &http.Server{Addr: addr, Handler: Handler(reg), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener failed", "addr", addr, "err", err)
		}
	}()
}
