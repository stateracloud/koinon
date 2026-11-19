// SPDX-License-Identifier: Apache-2.0

package modelgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// maxRequestBody bounds a request body the gateway reads to rewrite it.
const maxRequestBody = 32 << 20

// generationHeader carries the provider's response ID (an OpenRouter generation ID).
const generationHeader = "X-Generation-Id"

// Handler serves agents on the route paths of every format ([gateway.RoutePaths]). An agent
// authenticates with its gateway key as a bearer token.
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(g.serve)
}

func (g *Gateway) refuse(w http.ResponseWriter, status int, text string) {
	g.o.Metrics.request(resRefused)
	w.Header().Set(gateway.DoNotRetryHeader, gateway.DoNotRetryValue)
	http.Error(w, text, status)
}

func keyInOtherHeader(h http.Header) bool {
	for name, vals := range h {
		if strings.EqualFold(name, "Authorization") {
			continue
		}
		for _, v := range vals {
			if strings.Contains(v, gateway.KeyPrefix) {
				return true
			}
		}
	}
	return false
}

func formatOfPath(p string) (modelformat.Format, bool) {
	for _, f := range modelformat.All() {
		r, err := gateway.RenderingOf(gateway.Model{Format: f})
		if err == nil && r.Path == p {
			return f, true
		}
	}
	return "", false
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	if g.draining() {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	query, _ := url.QueryUnescape(r.URL.RawQuery)
	if strings.Contains(r.URL.String(), gateway.KeyPrefix) || strings.Contains(query, gateway.KeyPrefix) ||
		strings.Contains(r.URL.Path, gateway.KeyPrefix) || keyInOtherHeader(r.Header) {
		g.refuse(w, http.StatusForbidden, "refused: a gateway key belongs in the Authorization header only")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		g.refuse(w, http.StatusUnauthorized, "refused: no gateway key")
		return
	}
	snap := g.snap.Load()
	key, ok := snap.authenticate(strings.TrimSpace(token))
	if !ok {
		g.refuse(w, http.StatusUnauthorized, "refused: unknown gateway key")
		return
	}
	format, ok := formatOfPath(r.URL.Path)
	if !ok {
		g.refuse(w, http.StatusNotFound, "refused: no such route")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		g.refuse(w, http.StatusMethodNotAllowed, "refused: POST only")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		g.refuse(w, http.StatusRequestEntityTooLarge, "refused: the request body is too large or unreadable")
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		g.refuse(w, http.StatusBadRequest, "refused: the body is not a JSON object")
		return
	}
	var model string
	_ = json.Unmarshal(fields["model"], &model)
	rt, ok := snap.resolve(key.Agent, model)
	// No translation (ADR 0069): a model is served on the route of its own format only.
	if !ok || rt.m.Format != format {
		g.refuse(w, http.StatusNotFound, "refused: model_not_found")
		return
	}
	rendering, _ := gateway.RenderingOf(rt.m)
	for _, f := range rendering.Reject {
		if _, present := fields[f]; present {
			g.refuse(w, http.StatusForbidden, "refused: the field "+f+" is not allowed")
			return
		}
	}
	var stream bool
	_ = json.Unmarshal(fields["stream"], &stream)
	out, err := rewriteBody(fields, rendering, rt.m, stream)
	if err != nil {
		g.refuse(w, http.StatusBadRequest, "refused: the body cannot be rewritten")
		return
	}
	secret, err := g.providerKey(rt.p)
	if err != nil {
		// Fail closed: the request is not sent without the provider's key. A gateway fault, not a refusal.
		g.o.Logger.Error("provider key unreadable", "provider", rt.p.Name)
		g.o.Metrics.request(resUpstream)
		http.Error(w, "provider key unavailable", http.StatusBadGateway)
		return
	}
	var maxTokens int64
	_ = json.Unmarshal(fields["max_tokens"], &maxTokens)
	if maxTokens == 0 {
		_ = json.Unmarshal(fields["max_completion_tokens"], &maxTokens)
	}
	c := &call{
		g: g, start: g.o.Now(),
		rec: gateway.UsageRecord{
			RequestID: uuid.NewString(), KeyID: key.ID, Agent: key.Agent, Time: g.o.Now(),
			Model: rt.m.Name, Format: format, Stream: stream, UpstreamKnown: true, Upstream: true,
			MaxTokens: maxTokens, RequestBytes: int64(len(body)), ResponseBytes: -1,
		},
	}
	ctx := r.Context()
	if g.o.Tracer != nil {
		ctx, c.span = g.o.Tracer.Start(ctx, "model.call", trace.WithSpanKind(trace.SpanKindClient))
		defer c.span.End()
	}
	base, _ := url.Parse(rt.p.BaseURL)
	target := base.JoinPath(strings.TrimPrefix(r.URL.Path, "/v1"))
	rp := &httputil.ReverseProxy{
		Transport:     g.o.HTTPClient.Transport,
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(g.o.Logger.Handler(), slog.LevelDebug),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL, pr.Out.Host = target, target.Host
			pr.Out.URL.RawQuery = ""
			pr.Out.Body, pr.Out.ContentLength = io.NopCloser(bytes.NewReader(out)), int64(len(out))
			pr.Out.Header = upstreamHeaders(r.Header, rt, rendering, secret)
		},
		ModifyResponse: func(resp *http.Response) error { return c.response(resp, rt, format, stream) },
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			c.transportError(w, req, err)
		},
	}
	rp.ServeHTTP(w, r.WithContext(ctx))
}

// upstreamHeaders builds the upstream request headers: the allowlisted client headers, the
// rendering's own, and the provider credential. Nothing else of the client's request leaves.
func upstreamHeaders(in http.Header, rt route, rendering gateway.Rendering, secret string) http.Header {
	h := http.Header{}
	for _, name := range []string{"Accept", "Content-Type", "User-Agent"} {
		if v := in.Values(name); len(v) > 0 {
			h[name] = append([]string(nil), v...)
		}
	}
	for _, x := range rendering.RequestHeaders(rt.p, rt.m) {
		h.Set(x.Name, x.Value)
	}
	if secret != "" {
		h.Set("Authorization", "Bearer "+secret)
	}
	// An accept-encoding is never forwarded: the tap needs the plain bytes.
	return h
}

// rewriteBody applies the rendering's body rules, natively: the CEL of the agentgateway driver
// expresses the same rules (gateway.Rendering.Body).
func rewriteBody(f map[string]json.RawMessage, r gateway.Rendering, m gateway.Model, stream bool) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(f)+1)
	for k, v := range f {
		out[k] = v
	}
	for _, bf := range r.Body {
		switch bf.Field {
		case "stream_options":
			if stream {
				out[bf.Field] = json.RawMessage(`{"include_usage":true}`)
			} else {
				delete(out, bf.Field)
			}
		case "tools":
			if raw, ok := out[bf.Field]; ok {
				kept, err := keepClientTools(raw)
				if err != nil {
					return nil, err
				}
				out[bf.Field] = kept
			}
		}
	}
	if m.Guard {
		for k, v := range gateway.GuardOverrides() {
			b, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			out[k] = b
		}
	}
	return json.Marshal(out)
}

// keepClientTools keeps the tools the agent's harness runs itself: the ones without a type or of type
// "custom". Everything else is a tool the provider runs.
func keepClientTools(raw json.RawMessage) (json.RawMessage, error) {
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, err
	}
	kept := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		var probe struct {
			Type *string `json:"type"`
		}
		if err := json.Unmarshal(t, &probe); err != nil {
			return nil, err
		}
		if probe.Type == nil || *probe.Type == "custom" {
			kept = append(kept, t)
		}
	}
	return json.Marshal(kept)
}

// call is the state of one proxied request.
type call struct {
	g     *Gateway
	start time.Time
	span  trace.Span
	rec   gateway.UsageRecord
	once  sync.Once
}

// emit finishes the record and sends it, once.
func (c *call) emit(result string, edit func(*gateway.UsageRecord)) {
	c.once.Do(func() {
		if edit != nil {
			edit(&c.rec)
		}
		c.g.o.Metrics.request(result)
		if c.span != nil {
			c.span.SetAttributes(attribute.String("gen_ai.request.model", c.rec.Model),
				attribute.String("koinon.format", string(c.rec.Format)), attribute.Bool("koinon.stream", c.rec.Stream),
				attribute.Int64("http.response.status_code", c.rec.Status), attribute.String("koinon.result", result))
			if result != resOK {
				c.span.SetStatus(codes.Error, result)
			}
		}
		if c.g.o.Sink != nil {
			c.g.o.Sink(c.rec)
		}
	})
}

// response runs on the upstream's answer, before its headers go to the client.
func (c *call) response(resp *http.Response, rt route, format modelformat.Format, stream bool) error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// A redirect is never followed: it could lead the provider key to another host.
		_ = resp.Body.Close()
		return errRedirect
	}
	c.rec.Status = int64(resp.StatusCode)
	c.rec.ResponseID = resp.Header.Get(generationHeader)
	if resp.StatusCode == http.StatusForbidden {
		resp.Header.Set(gateway.DoNotRetryHeader, gateway.DoNotRetryValue)
	}
	resp.Header.Del("Set-Cookie")
	if resp.StatusCode >= 400 || resp.StatusCode < 200 {
		// An error body carries no usage. It still passes through, and the record keeps the status.
		c.emit(resUpstream, nil)
		return nil
	}
	var t tap
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		c.rec.Stream = true
		t = newSSETap(format)
	} else {
		t = newJSONTap(format)
	}
	resp.Body = &tapBody{rc: resp.Body, tap: t, finish: func(cn counts, complete bool, readErr error, n int64) {
		c.finishBody(resp.Request.Context(), cn, complete, readErr, n)
	}}
	return nil
}

func (c *call) finishBody(ctx context.Context, cn counts, complete bool, readErr error, n int64) {
	result := resOK
	edit := func(r *gateway.UsageRecord) {
		r.ResponseBytes = n
		if cn.inKnown {
			r.InputTokens, r.InputKnown = cn.in, true
			r.CacheReadTokens, r.CacheWriteTokens = cn.cacheRead, cn.cacheWrite
		}
		if cn.outKnown {
			r.OutputTokens, r.OutputKnown = cn.out, true
			if cn.reasoningKnown {
				r.ReasoningTokens, r.ReasoningKnown = cn.reasoning, true
			}
		}
		if !complete || readErr != nil {
			if ctx.Err() != nil || errors.Is(readErr, errClosedEarly) || errors.Is(readErr, context.Canceled) {
				r.Error = failCanceled
			} else {
				r.Error = failUpstream
			}
		}
	}
	if !complete || readErr != nil {
		result = resFailed
		if ctx.Err() != nil || errors.Is(readErr, errClosedEarly) || errors.Is(readErr, context.Canceled) {
			result = resCanceled
		}
	}
	c.emit(result, edit)
}

var errRedirect = errors.New("modelgateway: the upstream answered with a redirect")

// transportError answers a request the upstream never answered.
func (c *call) transportError(w http.ResponseWriter, req *http.Request, err error) {
	canceled := req.Context().Err() != nil
	var op *net.OpError
	dial := errors.As(err, &op) && op.Op == "dial"
	c.emit(map[bool]string{true: resCanceled, false: resUpstream}[canceled], func(r *gateway.UsageRecord) {
		r.Status = http.StatusBadGateway
		r.Upstream = !dial && !errors.Is(err, errRedirect)
		r.Error = failUpstream
		if canceled {
			r.Error, r.Status = failCanceled, 0
		}
	})
	if canceled {
		return
	}
	// A gateway failure is not a refusal: no do-not-retry marker (gateway.DoNotRetry).
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(w, "upstream unavailable\n")
}
