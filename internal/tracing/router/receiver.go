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
		if _, err := scanRequest(body, r.cfg.MaxRequestSpans); errors.Is(err, errBounds) {
			http.Error(w, "the request is larger than the router accepts", http.StatusRequestEntityTooLarge)
			return
		} else if err != nil {
			http.Error(w, "invalid ExportTraceServiceRequest", http.StatusBadRequest)
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

// The kinds of message the pre-scan descends into, and for each the fields that hold more of them.
// Everything else (strings, numbers, IDs) is skipped without being read.
type msgKind int

const (
	kRequest msgKind = iota
	kResourceSpans
	kResource
	kScopeSpans
	kScope
	kSpan
	kEvent
	kLink
	kKeyValue
	kAnyValue
	kArray
	kKVList
)

var childKinds = map[msgKind]map[protowire.Number]msgKind{
	kRequest:       {1: kResourceSpans},
	kResourceSpans: {1: kResource, 2: kScopeSpans},
	kResource:      {1: kKeyValue},
	kScopeSpans:    {1: kScope, 2: kSpan},
	kScope:         {3: kKeyValue},
	kSpan:          {9: kKeyValue, 11: kEvent, 13: kLink},
	kEvent:         {3: kKeyValue},
	kLink:          {5: kKeyValue},
	kKeyValue:      {2: kAnyValue},
	kAnyValue:      {5: kArray, 6: kKVList},
	kArray:         {1: kAnyValue},
	kKVList:        {1: kKeyValue},
}

// Bounds of a request's structure, in sub-messages. A few KB of gzip can name millions of empty
// attributes or resources, each of which decodes to a struct of tens of bytes, so the request is
// walked on the wire first and rejected before proto.Unmarshal allocates any of it.
const (
	maxRequestNodes    = 1 << 18 // sub-messages of every kind together: 256 spans of 1000 each
	maxRequestResource = 64      // ResourceSpans, and ScopeSpans in each
	maxNesting         = 16      // AnyValue inside array or kvlist inside AnyValue ...
)

// errBounds is a request that exceeds the structural bounds.
var errBounds = errors.New("request exceeds the structural bounds")

// scanRequest walks b on the wire without allocating, and returns the number of spans. It fails with
// errBounds when the request has more than maxSpans spans, more than maxRequestNodes sub-messages, more
// than maxRequestResource resources or scopes, or nests deeper than maxNesting; and with a parse error
// when it is not a well-formed message.
func scanRequest(b []byte, maxSpans int) (spans int, err error) {
	nodes := 0
	var walk func(kind msgKind, b []byte, depth int) error
	walk = func(kind msgKind, b []byte, depth int) error {
		if depth > maxNesting {
			return errBounds
		}
		kids := childKinds[kind]
		counts := map[msgKind]int{}
		for len(b) > 0 {
			n, typ, l := protowire.ConsumeTag(b)
			if l < 0 {
				return protowire.ParseError(l)
			}
			b = b[l:]
			vl := protowire.ConsumeFieldValue(n, typ, b)
			if vl < 0 {
				return protowire.ParseError(vl)
			}
			if child, ok := kids[n]; ok && typ == protowire.BytesType {
				payload, _ := protowire.ConsumeBytes(b)
				nodes++
				counts[child]++
				switch {
				case nodes > maxRequestNodes:
					return errBounds
				case child == kSpan:
					spans++
					if spans > maxSpans {
						return errBounds
					}
				case (child == kResourceSpans || child == kScopeSpans) && counts[child] > maxRequestResource:
					return errBounds
				}
				if err := walk(child, payload, depth+1); err != nil {
					return err
				}
			}
			b = b[vl:]
		}
		return nil
	}
	err = walk(kRequest, b, 0)
	return spans, err
}
