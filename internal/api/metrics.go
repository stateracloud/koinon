// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	connect "connectrpc.com/connect/v2"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the API's Prometheus metrics. Labels are the registered procedure name and the Connect
// code, both from fixed sets; never a user, org or request ID (internal/metrics states the rule).
type Metrics struct {
	RPCRequests *prometheus.CounterVec
	// QuestionsAsked counts the questions agents asked, by target kind (user, role, channel). The
	// ask_user tool increments it.
	QuestionsAsked *prometheus.CounterVec
}

// NewMetrics creates the metrics and registers them with reg (nil: not registered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RPCRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_rpc_requests_total",
			Help: "Connect RPCs finished, by procedure and Connect code (ok when the RPC succeeded).",
		}, []string{"procedure", "code"}),
		QuestionsAsked: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_questions_asked_total",
			Help: "Questions agents asked with ask_user, by target kind (user, role, channel).",
		}, []string{"target_kind"}),
	}
	for _, k := range []string{"user", "role", "channel"} {
		m.QuestionsAsked.WithLabelValues(k)
	}
	if reg != nil {
		reg.MustRegister(m.RPCRequests, m.QuestionsAsked)
	}
	return m
}

// Interceptor counts every RPC when it ends. It is the outermost interceptor, so RPCs the auth
// interceptor refuses are counted too. Unregistered procedures never reach it, which keeps the
// procedure label to the registered names.
func (m *Metrics) Interceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			err := next(ctx, spec, stream)
			code := "ok"
			if err != nil {
				code = connect.CodeOf(err).String()
			}
			m.RPCRequests.WithLabelValues(spec.Procedure, code).Inc()
			return err
		}
	}
}
