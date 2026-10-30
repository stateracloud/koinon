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
	"google.golang.org/protobuf/proto"
)

// TracesPath is where agentd's exporter posts (OTLP/HTTP).
const TracesPath = "/v1/traces"

// maxBody bounds one export request, compressed or not. agentd batches at most 256 spans of at most
// 64 KiB content each; the bound leaves room for that and no more.
const maxBody = 32 << 20

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
		ct, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if ct != "application/x-protobuf" {
			http.Error(w, "only application/x-protobuf is supported", http.StatusUnsupportedMediaType)
			return
		}
		body, err := readBody(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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

func readBody(r *http.Request) ([]byte, error) {
	var src io.Reader = http.MaxBytesReader(nil, r.Body, maxBody)
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(src)
		if err != nil {
			return nil, errors.New("invalid gzip body")
		}
		defer func() { _ = zr.Close() }()
		src = io.LimitReader(zr, maxBody+1)
	default:
		return nil, errors.New("unsupported Content-Encoding")
	}
	b, err := io.ReadAll(src)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(b) > maxBody {
		return nil, errors.New("body too large")
	}
	return b, nil
}
