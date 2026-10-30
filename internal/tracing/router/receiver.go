// SPDX-License-Identifier: Apache-2.0

package router

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// TracesPath is where agentd's exporter posts (OTLP/HTTP).
const TracesPath = "/v1/traces"

// A request is bounded three ways (Config.MaxRequestBytes, MaxRequestSpans, MaxConcurrentRequests),
// because a few KB of gzip can name millions of empty spans: the compressed and the decompressed size
// are capped, the spans are counted from the wire format before anything is decoded, and only a few
// requests are handled at once. agentd sends batches of at most 256 spans.

// Handler serves POST /v1/traces. It decodes the request, hands the spans to the router without
// waiting for anything and answers at once. Spans the router has no room for are counted as rejected
// in the OTLP partial success, which the sender logs; retrying them would not help.
//
// Only OTLP/HTTP protobuf is accepted. OTLP JSON writes trace and span IDs as hex where proto3 JSON
// writes base64, so a generic decoder would silently mangle every ID; agentd sends protobuf.
//
// The handler does no authentication: it is mounted inside the internal OTLP listener's bearer-token
// middleware (internal/otlpauth), which covers every path of the listener.
func (r *Router) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		select {
		case r.reqs <- struct{}{}:
			defer func() { <-r.reqs }()
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests in progress", http.StatusServiceUnavailable)
			return
		}
		ct, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if ct != "application/x-protobuf" {
			http.Error(w, "only application/x-protobuf is supported", http.StatusUnsupportedMediaType)
			return
		}
		body, err := readBody(req, r.cfg.MaxRequestBytes)
		if err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errTooLarge) {
				code = http.StatusRequestEntityTooLarge
			}
			http.Error(w, err.Error(), code)
			return
		}
		if n, err := countSpans(body); err != nil {
			http.Error(w, "invalid ExportTraceServiceRequest", http.StatusBadRequest)
			return
		} else if n > r.cfg.MaxRequestSpans {
			http.Error(w, "too many spans in one request", http.StatusRequestEntityTooLarge)
			return
		}
		var in coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &in); err != nil {
			http.Error(w, "invalid ExportTraceServiceRequest", http.StatusBadRequest)
			return
		}
		var spans []*tracepb.Span
		for _, rs := range in.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				spans = append(spans, ss.GetSpans()...)
			}
		}
		resp := &coltracepb.ExportTraceServiceResponse{}
		if rejected := r.Submit(SourceAgentd, spans); rejected > 0 {
			resp.PartialSuccess = &coltracepb.ExportTracePartialSuccess{RejectedSpans: rejected, ErrorMessage: "the trace router is full"}
		}
		out, err := proto.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	})
}

// Mount returns a middleware that serves TracesPath with the router and passes every other request to
// the next handler. Pass it to ledger.Serve after the token check, so the token covers it.
func (r *Router) Mount() func(http.Handler) http.Handler {
	h := r.Handler()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == TracesPath {
				h.ServeHTTP(w, req)
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}

var errTooLarge = errors.New("body too large")

// readBody reads the request, gunzipping it if asked. Both the compressed and the decompressed size
// are limited by limit (decompressed: four times it).
func readBody(r *http.Request, limit int) ([]byte, error) {
	var src io.Reader = http.MaxBytesReader(nil, r.Body, int64(limit))
	maxOut := limit
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, errors.New("invalid gzip body")
		}
		defer func() { _ = zr.Close() }()
		maxOut = limit * 4
		src = io.LimitReader(zr, int64(maxOut)+1)
	default:
		return nil, errors.New("unsupported Content-Encoding")
	}
	b, err := io.ReadAll(src)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, errTooLarge
		}
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(b) > maxOut {
		return nil, errTooLarge
	}
	return b, nil
}

// Field numbers of the OTLP trace request: ExportTraceServiceRequest.resource_spans,
// ResourceSpans.scope_spans and ScopeSpans.spans.
const (
	fieldResourceSpans = 1
	fieldScopeSpans    = 2
	fieldSpans         = 2
)

// countSpans counts the spans of a request from the wire format, without decoding or allocating them.
func countSpans(b []byte) (int, error) {
	n := 0
	err := eachField(b, fieldResourceSpans, func(rs []byte) error {
		return eachField(rs, fieldScopeSpans, func(ss []byte) error {
			return eachField(ss, fieldSpans, func([]byte) error { n++; return nil })
		})
	})
	return n, err
}

// eachField calls f with the payload of every length-delimited field num of the message b.
func eachField(b []byte, num protowire.Number, f func([]byte) error) error {
	for len(b) > 0 {
		n, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return protowire.ParseError(l)
		}
		b = b[l:]
		l = protowire.ConsumeFieldValue(n, typ, b)
		if l < 0 {
			return protowire.ParseError(l)
		}
		if n == num && typ == protowire.BytesType {
			v, _ := protowire.ConsumeBytes(b)
			if err := f(v); err != nil {
				return err
			}
		}
		b = b[l:]
	}
	return nil
}
