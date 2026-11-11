// SPDX-License-Identifier: Apache-2.0

package agentd

// Run spans leave agentd through the OpenTelemetry Go SDK to koinon-api's internal OTLP listener
// (ADR 0074, decisions 1 and 7). Export never blocks a run: spans go into a bounded queue that drops
// when it is full, one worker exports batches with a deadline, and a batch that cannot be exported
// is dropped and counted. Nothing waits for the receiver.

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/stateracloud/koinon/internal/otlpauth"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/internal/tracing/semconv"
)

// TraceDecider decides whether a run is traced and at which content level. [tracedest.Service]
// provides both (see [TraceDeciderFrom]).
type TraceDecider interface {
	// DecideRun decides once whether the run is traced and stores it on the run.
	DecideRun(ctx context.Context, org, run pgtype.UUID) (bool, error)
	// ContentLevel is the highest content level any of the org's destinations may receive:
	// tracedest.LevelNone, LevelRedacted or LevelFull.
	ContentLevel(ctx context.Context, org pgtype.UUID) (string, error)
}

type settingsReader interface {
	DecideRun(ctx context.Context, org, run pgtype.UUID) (bool, error)
	GetSettings(ctx context.Context, org pgtype.UUID) (tracedest.Settings, error)
}

type decider struct{ s settingsReader }

// TraceDeciderFrom adapts the destination service. The org's own level, held to the platform
// ceiling, is the highest any of its destinations can get (the platform's copy is never above it),
// so it is what decides whether agentd sends content at all; the router applies each destination's
// own level.
func TraceDeciderFrom(s settingsReader) TraceDecider { return decider{s} }

func (d decider) DecideRun(ctx context.Context, org, run pgtype.UUID) (bool, error) {
	return d.s.DecideRun(ctx, org, run)
}

func (d decider) ContentLevel(ctx context.Context, org pgtype.UUID) (string, error) {
	st, err := d.s.GetSettings(ctx, org)
	if err != nil {
		return "", err
	}
	return tracedest.MinLevel(st.ContentLevel, st.Ceiling), nil
}

// Tracing turns on run spans (ADR 0074). Config.Tracing nil leaves them off.
type Tracing struct {
	// Decider decides per run; required.
	Decider TraceDecider
	// Endpoint is the base URL of koinon-api's internal OTLP listener; the exporter appends
	// /v1/traces. TokenFile is the file with the bearer token it requires for /v1/traces (agentd's own, not the gateway's), read on every export so a
	// rotated token applies. Both are required unless Exporter is set.
	Endpoint, TokenFile string
	// Exporter replaces the OTLP/HTTP exporter (tests).
	Exporter sdktrace.SpanExporter
	// QueueSize bounds the spans waiting for export; a full queue drops. Default 2048.
	QueueSize int
	// BatchSize is the most spans in one export. Default 256.
	BatchSize int
	// Interval is how long spans wait for a fuller batch. Default 2s.
	Interval time.Duration
	// BatchBytes bounds the estimated size of one export, so a batch of spans with large content stays
	// under the receiver's request cap (4 MiB). Default 3 MiB.
	BatchBytes int
	// ExportTimeout bounds one export, retries included. Default 5s.
	ExportTimeout time.Duration
	// ContentPreCap bounds each content attribute in memory, in bytes. The router truncates to the
	// destination's limit; this only keeps a 2 MB tool result from being held. Default 64 KiB.
	ContentPreCap int
}

func (t *Tracing) defaults() error {
	if t.Decider == nil {
		return errors.New("agentd: Tracing needs a Decider")
	}
	if t.Exporter == nil && (t.Endpoint == "" || t.TokenFile == "") {
		return errors.New("agentd: Tracing needs an Endpoint and a TokenFile")
	}
	setDefault(&t.QueueSize, 2048)
	setDefault(&t.BatchSize, 256)
	setDefault(&t.Interval, 2*time.Second)
	setDefault(&t.BatchBytes, 3<<20)
	setDefault(&t.ExportTimeout, 5*time.Second)
	setDefault(&t.ContentPreCap, 64<<10)
	return nil
}

// Reasons spans are dropped (koinon_agentd_trace_spans_dropped_total).
const (
	dropQueueFull    = "queue_full"
	dropExportFailed = "export_failed"
	dropRejected     = "rejected"
	dropShutdown     = "shutdown"
)

// traceSink owns the tracer provider of the pool.
type traceSink struct {
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer
	proc   *dropProcessor
	cap    int
}

func newTraceSink(cfg *Tracing, m *Metrics, log *slog.Logger) (*traceSink, error) {
	exp := cfg.Exporter
	if exp == nil {
		var err error
		if exp, err = otlpExporter(cfg); err != nil {
			return nil, err
		}
	}
	proc := newDropProcessor(exp, cfg, m, log)
	res := resource.NewWithAttributes(semconv.SchemaURL,
		attribute.String("service.name", semconv.ServiceName),
		attribute.String("service.version", project.Version))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(proc), sdktrace.WithResource(res),
		sdktrace.WithIDGenerator(idGen{}), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	return &traceSink{
		tp: tp, proc: proc, cap: cfg.ContentPreCap,
		tracer: tp.Tracer(semconv.ScopeName, trace.WithSchemaURL(semconv.SchemaURL)),
	}, nil
}

// shutdown flushes and stops the worker, for at most ctx.
func (s *traceSink) shutdown(ctx context.Context) { _ = s.tp.Shutdown(ctx) }

func otlpExporter(cfg *Tracing) (sdktrace.SpanExporter, error) {
	base := strings.TrimSuffix(cfg.Endpoint, "/")
	tr := &tokenTransport{file: cfg.TokenFile, next: http.DefaultTransport}
	exp, err := otlptracehttp.New(context.Background(),
		otlptracehttp.WithEndpointURL(base+"/v1/traces"),
		otlptracehttp.WithHTTPClient(&http.Client{Transport: tr}),
		otlptracehttp.WithTimeout(cfg.ExportTimeout),
		otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{
			Enabled: true, InitialInterval: 500 * time.Millisecond, MaxInterval: 2 * time.Second, MaxElapsedTime: cfg.ExportTimeout,
		}),
	)
	if err != nil {
		return nil, err
	}
	return otlpSpanExporter{SpanExporter: exp, tr: tr}, nil
}

// tokenTransport sends the listener's bearer token, read from its file on every request, and notes
// the spans the listener rejected in a partial success (the exporter reports those only as an
// error without a count).
type tokenTransport struct {
	file     string
	next     http.RoundTripper
	rejected atomic.Int64
}

func (t *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tok, err := otlpauth.ReadToken(t.file)
	if err != nil {
		return nil, fmt.Errorf("the OTLP token: %w", err)
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := t.next.RoundTrip(r)
	if err != nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if rerr == nil {
		var out coltracepb.ExportTraceServiceResponse
		if proto.Unmarshal(body, &out) == nil {
			t.rejected.Add(out.GetPartialSuccess().GetRejectedSpans())
		}
	}
	return resp, nil
}

// otlpSpanExporter is the OTLP/HTTP exporter and the count of spans its last export had rejected.
type otlpSpanExporter struct {
	sdktrace.SpanExporter
	tr *tokenTransport
}

// takeRejected returns, and resets, the spans rejected since the last call.
func (e otlpSpanExporter) takeRejected() int64 { return e.tr.rejected.Swap(0) }

// dropProcessor is a span processor with a bounded queue that drops instead of waiting.
type dropProcessor struct {
	exp      sdktrace.SpanExporter
	m        *Metrics
	ch       chan sdktrace.ReadOnlySpan
	flushCh  chan chan struct{}
	stop     chan struct{}
	done     chan struct{}
	batch    int
	maxBytes int
	every    time.Duration
	timeout  time.Duration
	log      *slog.Logger
	// mu orders OnEnd against Shutdown: no span is queued once closed is set, so the worker's last
	// drain sees every span that was accepted.
	mu      sync.RWMutex
	closed  bool
	once    sync.Once
	lastLog time.Time
}

// rejecter is implemented by exporters that know how many spans the receiver rejected.
type rejecter interface{ takeRejected() int64 }

func newDropProcessor(exp sdktrace.SpanExporter, cfg *Tracing, m *Metrics, log *slog.Logger) *dropProcessor {
	p := &dropProcessor{
		exp: exp, m: m, log: log, ch: make(chan sdktrace.ReadOnlySpan, cfg.QueueSize), flushCh: make(chan chan struct{}),
		stop: make(chan struct{}), done: make(chan struct{}),
		batch: cfg.BatchSize, maxBytes: cfg.BatchBytes, every: cfg.Interval, timeout: cfg.ExportTimeout,
	}
	go p.loop()
	return p
}

func (p *dropProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (p *dropProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		p.m.TraceSpansDropped.WithLabelValues(dropShutdown).Inc()
		return
	}
	select {
	case p.ch <- s:
	default:
		p.m.TraceSpansDropped.WithLabelValues(dropQueueFull).Inc()
	}
}

func (p *dropProcessor) loop() {
	defer close(p.done)
	t := time.NewTicker(p.every)
	defer t.Stop()
	var buf []sdktrace.ReadOnlySpan
	flush := func() {
		for len(buf) > 0 {
			// A batch ends at BatchSize spans or BatchBytes of estimated size, whichever comes first;
			// a span larger than the byte bound goes alone.
			n, size := 0, 0
			for n < len(buf) && n < p.batch {
				sz := spanSize(buf[n])
				if n > 0 && size+sz > p.maxBytes {
					break
				}
				size += sz
				n++
			}
			p.export(buf[:n])
			buf = buf[n:]
		}
		buf = nil
	}
	drain := func() {
		for {
			select {
			case s := <-p.ch:
				buf = append(buf, s)
			default:
				return
			}
		}
	}
	for {
		select {
		case s := <-p.ch:
			buf = append(buf, s)
			if len(buf) >= p.batch {
				flush()
			}
		case <-t.C:
			flush()
		case ack := <-p.flushCh:
			drain()
			flush()
			close(ack)
		case <-p.stop:
			drain()
			flush()
			return
		}
	}
}

// spanSize estimates the size of a span on the wire: its text, which is what makes a span large.
func spanSize(s sdktrace.ReadOnlySpan) int {
	n := 256 + len(s.Name())
	for _, kv := range s.Attributes() {
		n += len(kv.Key) + 16
		switch kv.Value.Type() {
		case attribute.STRING:
			n += len(kv.Value.AsString())
		case attribute.STRINGSLICE:
			for _, v := range kv.Value.AsStringSlice() {
				n += len(v) + 4
			}
		}
	}
	for _, e := range s.Events() {
		n += 64 + len(e.Name) + 32*len(e.Attributes)
	}
	return n + 64*len(s.Links())
}

func (p *dropProcessor) export(spans []sdktrace.ReadOnlySpan) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	err := p.exp.ExportSpans(ctx, spans)
	var rejected int64
	if r, ok := p.exp.(rejecter); ok {
		rejected = min(r.takeRejected(), int64(len(spans)))
	}
	switch {
	case err != nil && rejected == 0:
		p.m.TraceSpansDropped.WithLabelValues(dropExportFailed).Add(float64(len(spans)))
		p.warn("exporting run spans failed; the batch is dropped", err, len(spans))
	case rejected > 0:
		// A partial success: the listener took the rest.
		p.m.TraceSpansDropped.WithLabelValues(dropRejected).Add(float64(rejected))
		p.m.TraceSpansExported.Add(float64(int64(len(spans)) - rejected))
		p.warn("the OTLP listener rejected run spans", err, int(rejected))
	default:
		p.m.TraceSpansExported.Add(float64(len(spans)))
	}
}

// warn logs at most once every 30 seconds. The error is the exporter's: it names the URL and the
// status, never the token.
func (p *dropProcessor) warn(msg string, err error, spans int) {
	if p.log == nil || time.Since(p.lastLog) < 30*time.Second {
		return
	}
	p.lastLog = time.Now()
	p.log.Warn(msg, "spans", spans, "err", err)
}

func (p *dropProcessor) ForceFlush(ctx context.Context) error {
	ack := make(chan struct{})
	select {
	case p.flushCh <- ack:
	case <-p.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *dropProcessor) Shutdown(ctx context.Context) error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.stop)
	})
	select {
	case <-p.done:
		return p.exp.Shutdown(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// idGen makes random IDs, except for a turn's root span: Koinon assigns the trace ID (it is stored
// on the run_turns row) and derives the root span's ID from it, so a later turn, a takeover or the
// router can name the root without having seen it (ADR 0074, decision 2).
type idGen struct{}

type rootIDsKey struct{}

type rootIDs struct {
	trace trace.TraceID
	span  trace.SpanID
}

func (idGen) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if r, ok := ctx.Value(rootIDsKey{}).(rootIDs); ok {
		return r.trace, r.span
	}
	var t trace.TraceID
	for t == (trace.TraceID{}) {
		_, _ = rand.Read(t[:])
	}
	return t, randSpanID()
}

func (idGen) NewSpanID(context.Context, trace.TraceID) trace.SpanID { return randSpanID() }

func randSpanID() trace.SpanID {
	var s trace.SpanID
	for s == (trace.SpanID{}) {
		_, _ = rand.Read(s[:])
	}
	return s
}

// RootSpanID is the span ID of the root span of the turn with this trace ID: the first 8 bytes of
// SHA-256("koinon.turn.root" + trace ID). The router uses it to place spans under the turn.
func RootSpanID(traceID [16]byte) [8]byte { return semconv.RootSpanID(traceID) }
