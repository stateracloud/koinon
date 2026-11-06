// SPDX-License-Identifier: Apache-2.0

package router

import (
	"bytes"
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
		refuse := func(reason string, code int, msg string) {
			r.metrics.RequestsRejected.WithLabelValues(reason).Inc()
			if code == http.StatusServiceUnavailable {
				w.Header().Set("Retry-After", "1")
			}
			http.Error(w, msg, code)
		}
		select {
		case r.reqs <- struct{}{}:
			defer func() { <-r.reqs }()
		default:
			refuse(reqBusy, http.StatusServiceUnavailable, "too many requests in progress")
			return
		}
		ct, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if ct != "application/x-protobuf" {
			http.Error(w, "only application/x-protobuf is supported", http.StatusUnsupportedMediaType)
			return
		}
		// The bytes this request holds are charged to RequestBytes as they are known: the encoded body
		// first, then the decompressed body and what decoding it allocates. A request that does not fit
		// waits for none: it is refused with Retry-After, or, if it could never fit, as too large.
		var held int64
		defer func() { r.reqBytes.Add(-held) }()
		charge := func(n int64) bool {
			if r.reqBytes.Add(n) > int64(r.cfg.RequestBytes) {
				r.reqBytes.Add(-n)
				return false
			}
			held += n
			return true
		}
		encoded := req.ContentLength
		if encoded <= 0 || encoded > int64(r.cfg.MaxRequestBytes) {
			encoded = int64(r.cfg.MaxRequestBytes)
		}
		if !charge(encoded) {
			if encoded > int64(r.cfg.RequestBytes) {
				refuse(reqMemory, http.StatusRequestEntityTooLarge, "the request is too large to read within the router's memory budget; send smaller batches")
			} else {
				refuse(reqMemory, http.StatusServiceUnavailable, "the trace router is handling as much as it can; retry")
			}
			return
		}
		// A gzip body's buffer is pre-sized at most to half of what is left of the request budget.
		body, err := readBody(req, r.cfg.MaxRequestBytes, int(max((int64(r.cfg.RequestBytes)-r.reqBytes.Load())/2, 0)))
		if err != nil {
			if errors.Is(err, errTooLarge) {
				refuse(reqBodyTooLarge, http.StatusRequestEntityTooLarge,
					fmt.Sprintf("the request is larger than %d bytes (encoded) or %d bytes (decompressed); send smaller batches", r.cfg.MaxRequestBytes, 4*r.cfg.MaxRequestBytes))
			} else {
				refuse(reqMalformed, http.StatusBadRequest, err.Error())
			}
			return
		}
		if _, err := scanRequest(body, r.cfg.MaxRequestSpans); err != nil {
			switch {
			case errors.Is(err, errTooManySpans):
				refuse(reqTooManySpans, http.StatusRequestEntityTooLarge, fmt.Sprintf("more than %d spans in one request; send smaller batches", r.cfg.MaxRequestSpans))
			case errors.Is(err, errBounds):
				refuse(reqStructure, http.StatusRequestEntityTooLarge, err.Error())
			default:
				refuse(reqMalformed, http.StatusBadRequest, "invalid ExportTraceServiceRequest")
			}
			return
		}
		// Decoding allocates a few times the encoded size; the decompressed body is held too.
		if need := int64(len(body)) * 4; !charge(need) {
			if need > int64(r.cfg.RequestBytes) {
				refuse(reqMemory, http.StatusRequestEntityTooLarge, "the request is too large to decode within the router's memory budget; send smaller batches")
			} else {
				refuse(reqMemory, http.StatusServiceUnavailable, "the trace router is handling as much as it can; retry")
			}
			return
		}
		var in coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &in); err != nil {
			refuse(reqMalformed, http.StatusBadRequest, "invalid ExportTraceServiceRequest")
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
// the next handler. Pass it to agentgateway.Serve after the token check, so the token covers it.
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

// readBody reads the request, gunzipping it if asked. The encoded size is limited by limit and the
// decompressed size by four times it (16 MiB at the default; what the request budget allows in
// practice is less: see Config.RequestBytes).
func readBody(r *http.Request, limit, maxHint int) ([]byte, error) {
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
	// The buffer is sized from Content-Length instead of growing by doubling, which would allocate about
	// twice the body: exactly the length plus bytes.MinRead, so reading the end does not grow it. A gzip
	// body is pre-sized for eight times its encoded length at most, never more than the decompressed
	// ceiling or maxHint, and grows beyond that only as the data really is that large.
	hint := 64 << 10
	if cl := int(min(max(r.ContentLength, 0), int64(limit))); cl > 0 {
		hint = cl + bytes.MinRead
		if maxOut > limit {
			hint = max(min(cl*8, maxOut, maxHint), 64<<10)
		}
	}
	buf := bytes.NewBuffer(make([]byte, 0, hint))
	_, err := buf.ReadFrom(src)
	b := buf.Bytes()
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

// errBounds is a request that exceeds the structural bounds; errTooManySpans is the one that has more
// spans than allowed.
var (
	errBounds       = errors.New("the request exceeds the structural bounds the router accepts (sub-messages, resources or nesting)")
	errTooManySpans = errors.New("too many spans")
)

// scanRequest walks b on the wire without allocating, and returns the number of spans. It fails with
// errBounds when the request has more than maxSpans spans, more than maxRequestNodes sub-messages, more
// than maxRequestResource resources or scopes, or nests deeper than maxNesting (errTooManySpans for the
// span cap); and with a parse error
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
						return errTooManySpans
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
