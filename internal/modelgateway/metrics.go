// SPDX-License-Identifier: Apache-2.0

package modelgateway

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the model path's counters. Labels come from fixed sets only (ADR 0023): never an org,
// agent, key, model or run ID.
type Metrics struct {
	Requests *prometheus.CounterVec // by result
	Config   *prometheus.CounterVec // by reason
	KeyEcho  *prometheus.CounterVec // by answer: a response that carried the provider key (#1332)
}

// Results of a request, the label values of Requests.
const (
	resOK       = "ok"
	resRefused  = "refused"
	resUpstream = "upstream_error"
	resFailed   = "failed" // a stream cut or an error event
	resCanceled = "canceled"
	resBudget   = "budget_exceeded" // the key's spend reached its limit; the provider was not called
)

// Kinds of answer, the label values of KeyEcho.
const (
	answerSuccess = "success" // a 2xx answer
	answerError   = "error"   // a status of 400 or more
)

// NewMetrics registers the counters on reg (nil registers nothing).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_gateway_model_requests_total",
			Help: "Model requests by result.",
		}, []string{"result"}),
		Config: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_gateway_model_config_dropped_total",
			Help: "Model config entries left out of the applied config, by reason.",
		}, []string{"reason"}),
		KeyEcho: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_gateway_model_key_echo_total",
			Help: "Model responses whose headers or body carried the provider key back toward the agent, by answer.",
		}, []string{"answer"}),
	}
	for _, r := range []string{resOK, resRefused, resUpstream, resFailed, resCanceled, resBudget} {
		m.Requests.WithLabelValues(r)
	}
	for _, r := range []string{"base_url", "format", "price"} {
		m.Config.WithLabelValues(r)
	}
	for _, b := range []string{answerSuccess, answerError} {
		m.KeyEcho.WithLabelValues(b)
	}
	if reg != nil {
		reg.MustRegister(m.Requests, m.Config, m.KeyEcho)
	}
	return m
}

func (m *Metrics) request(result string) {
	if m != nil {
		m.Requests.WithLabelValues(result).Inc()
	}
}

func (m *Metrics) dropped(reason string) {
	if m != nil {
		m.Config.WithLabelValues(reason).Inc()
	}
}

func (m *Metrics) keyEcho(answer string) {
	if m != nil {
		m.KeyEcho.WithLabelValues(answer).Inc()
	}
}
