// SPDX-License-Identifier: Apache-2.0

package router

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Sources of a received span (koinon_api_trace_spans_received_total).
const (
	SourceAgentd = "agentd"
	SourceLedger = "ledger"
)

// Destination kinds, the second label of every export metric.
const (
	KindOrg      = "org"
	KindPlatform = "platform"
)

// Reasons a span is discarded before it reaches a destination queue.
const (
	discardIngestFull    = "ingest_full"   // the router's intake was full
	discardUnattributed  = "unattributed"  // no usable koinon.org or koinon.run, or a bad trace ID
	discardUnknownRun    = "unknown_run"   // no such run in that org
	discardUnknownTrace  = "unknown_trace" // the trace is not one of that run's turns
	discardUntraced      = "untraced"      // the run is not traced
	discardNoRoute       = "no_destination"
	discardLookup        = "lookup_failed" // the database could not answer; the span is dropped, not guessed
	discardShutdown      = "shutdown"
	discardInvalid       = "invalid"        // not a well-formed span
	discardEnqueueFailed = "enqueue_failed" // no live queue could take the span
	discardDuplicate     = "duplicate_root" // a root already held or finished
)

// Reasons a destination's span is dropped (koinon_api_trace_spans_dropped_total).
const (
	dropQueueFull    = "queue_full"
	dropExportFailed = "export_failed"
	dropRemoved      = "destination_removed"
	dropShutdown     = "shutdown"
	dropUnreadable   = "credentials_unreadable" // the destination's stored credentials cannot be opened
)

// Reasons a destination refuses spans (koinon_api_trace_spans_rejected_total).
const (
	rejectPartial = "partial_success" // the backend accepted the batch but rejected some spans
	rejectRefused = "batch_refused"   // the backend refused the batch with a non-retryable 4xx
)

// Outcomes of an export attempt (koinon_api_trace_export_duration_seconds).
const (
	outcomeOK        = "ok"
	outcomeRetryable = "retryable"
	outcomeRefused   = "refused"
)

// Metrics are the router's Prometheus metrics (ADR 0074, decision 10). Labels come from fixed sets:
// no org, destination, agent, run or task ID is ever a label (ADR 0023).
type Metrics struct {
	Received   *prometheus.CounterVec
	Discarded  *prometheus.CounterVec
	Exported   *prometheus.CounterVec
	Dropped    *prometheus.CounterVec
	Rejected   *prometheus.CounterVec
	ExportTime *prometheus.HistogramVec
	QueueDepth *prometheus.GaugeVec
	Redactions *prometheus.CounterVec
	HeldRoots  prometheus.Gauge
	// RequestsRejected counts requests the receiver refused whole, by reason.
	RequestsRejected *prometheus.CounterVec
}

// NewMetrics creates the metrics and registers them with reg (nil: not registered).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	dk := []string{"destination_kind", "backend"}
	m := &Metrics{
		Received: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_trace_spans_received_total", Help: "Spans received by the trace router, by source (agentd, ledger).",
		}, []string{"source"}),
		Discarded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_trace_spans_discarded_total", Help: "Spans the trace router discarded before any destination, by reason.",
		}, []string{"reason"}),
		Exported: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_trace_spans_exported_total", Help: "Spans a destination accepted.",
		}, dk),
		Dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_trace_spans_dropped_total", Help: "Spans dropped from a destination's queue, by reason.",
		}, append(dk[:2:2], "reason")),
		Rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_trace_spans_rejected_total", Help: "Spans a destination's backend refused, by reason.",
		}, append(dk[:2:2], "reason")),
		ExportTime: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "koinon_api_trace_export_duration_seconds", Help: "Duration of one export request, by outcome.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2.5, 10),
		}, append(dk[:2:2], "outcome")),
		QueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "koinon_api_trace_queue_depth", Help: "Spans waiting in destination queues, in total.",
		}, dk),
		Redactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koinon_api_trace_redactions_total", Help: "Values the scrubber replaced, by kind.",
		}, []string{"kind"}),
		HeldRoots: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "koinon_api_trace_held_roots", Help: "Turn root spans waiting for their model calls from the ledger.",
		}),
	}
	m.RequestsRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "koinon_api_trace_requests_rejected_total", Help: "Export requests the trace receiver refused whole, by reason (body_too_large, too_many_spans, structure, malformed, busy, memory).",
	}, []string{"reason"})
	for _, r := range []string{reqBodyTooLarge, reqTooManySpans, reqStructure, reqMalformed, reqBusy, reqMemory} {
		m.RequestsRejected.WithLabelValues(r)
	}
	for _, s := range []string{SourceAgentd, SourceLedger} {
		m.Received.WithLabelValues(s)
	}
	for _, r := range []string{discardIngestFull, discardUnattributed, discardUnknownRun, discardUnknownTrace, discardUntraced, discardNoRoute, discardLookup, discardShutdown, discardInvalid, discardEnqueueFailed, discardDuplicate} {
		m.Discarded.WithLabelValues(r)
	}
	if reg != nil {
		reg.MustRegister(m.Received, m.Discarded, m.Exported, m.Dropped, m.Rejected, m.ExportTime, m.QueueDepth, m.Redactions, m.HeldRoots, m.RequestsRejected)
	}
	return m
}

// Reasons the receiver refuses a request whole (koinon_api_trace_requests_rejected_total).
const (
	reqBodyTooLarge = "body_too_large"
	reqTooManySpans = "too_many_spans"
	reqStructure    = "structure"
	reqMalformed    = "malformed"
	reqBusy         = "busy"
	reqMemory       = "memory"
)
