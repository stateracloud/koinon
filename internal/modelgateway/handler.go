// SPDX-License-Identifier: Apache-2.0

package modelgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/pricing"
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

// refuseActor answers an actor JWT that is not accepted. A 503 (no issuer keys) is not final.
func (g *Gateway) refuseActor(w http.ResponseWriter, r *gateway.ActorRefusal) {
	if r.Final {
		g.refuse(w, r.Status, r.Text)
		return
	}
	g.o.Metrics.request(resRefused)
	w.Header().Set("Retry-After", "5")
	http.Error(w, r.Text, r.Status)
}

// refuseBudget answers a key whose spend reached its limit: 429 budget_exceeded, marked final. The
// provider is not called. Retry-After is the time until the window ends, which a consumer with a
// cap reads as final; a limit without a window sends none (only a new limit resets it).
func (g *Gateway) refuseBudget(w http.ResponseWriter, format modelformat.Format, wait time.Duration) {
	g.o.Metrics.request(resBudget)
	h := w.Header()
	h.Set(gateway.DoNotRetryHeader, gateway.DoNotRetryValue)
	h.Set("Content-Type", "application/json")
	if wait > 0 {
		h.Set("Retry-After", strconv.FormatInt(int64((wait+time.Second-1)/time.Second), 10))
	}
	w.WriteHeader(http.StatusTooManyRequests)
	// The body has the error shape of the format, so the SDKs parse it.
	const msg = "budget_exceeded: the API key budget is exceeded"
	if format == modelformat.Anthropic {
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"budget_exceeded","message":"`+msg+`"}}`)
		return
	}
	_, _ = io.WriteString(w, `{"error":{"type":"budget_exceeded","code":"budget_exceeded","message":"`+msg+`"}}`)
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
	token = strings.TrimSpace(token)
	snap := g.snap.Load()
	var key gateway.Key
	if g.o.Actors != nil && !gateway.IsGatewayKey(token) {
		// A Substrate guest: atenet-egress put the actor JWT here (issue #1489). No fallback to a key. The
		// JWT never goes upstream: upstreamHeaders forwards no Authorization of the client.
		agent, refusal := gateway.IdentifyActor(g.o.Actors, token)
		if refusal != nil {
			g.o.Logger.Info("refused a model request: actor token", "reason", refusal.Reason, "status", refusal.Status)
			g.refuseActor(w, refusal)
			return
		}
		if key, ok = snap.byAgent[agent]; !ok {
			g.refuse(w, http.StatusForbidden, "refused: the agent has no gateway key")
			return
		}
	} else if key, ok = snap.authenticate(token); !ok {
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
	// The budget stop. It runs after the key and the model are known, so a key that is not loaded
	// yet is an unknown key (401), never a budget refusal.
	if refused, wait := g.overBudget(key.Agent); refused {
		g.refuseBudget(w, format, wait)
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
		g: g, start: g.o.Now(), agent: key.Agent, price: rt.price,
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
		ModifyResponse: func(resp *http.Response) error { return c.response(resp, rt, format, secret) },
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
	for _, name := range gateway.ForwardedHeaders {
		if name == "content-length" {
			continue // the proxy sets it from the rewritten body
		}
		if v := in.Values(name); len(v) > 0 {
			name = http.CanonicalHeaderKey(name)
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
	// agent and price are the agent and the model's price. The call is charged against the agent's
	// limit in force when it ends.
	agent gateway.AgentRef
	price pricing.Price
}

// overBudget reports whether the agent has reached the limit in force, and the wait until its
// window ends. The limit is read from the current snapshot: a request that read its snapshot before
// an apply must not be checked against a generation the apply retired. The apply publishes the new
// snapshot before a check can find the old generation retired, so one more read is enough. If an
// apply retires the generation again and again, the call is refused (fail closed) with a short wait.
func (g *Gateway) overBudget(a gateway.AgentRef) (bool, time.Duration) {
	for range 8 {
		l, ok := g.snap.Load().state.Limits[a]
		if !ok {
			return false, 0
		}
		if ok, wait, live := g.bud.check(a, l, g.o.Now()); live {
			return !ok, wait
		}
	}
	return true, time.Second
}

// charge adds the cost of the call to the agent's counter, under the limit in force now: a call
// that started under an older generation is charged to the current one, which the ledger computed
// before it saw this call. A call that reports its whole usage is priced on it. A 2xx call whose
// usage is missing or partial (a cut or canceled stream, or a provider that sends no usage) is
// charged a bound for what is missing: the request bytes at 4 bytes a token as input, and the
// request's max_tokens as output (0 when the request sets none: a gap, see the PR). A cut stream
// is charged at least that output bound. The next generation replaces the bound with the ledger's
// own figure. A non-2xx answer is not charged here (a gap, see the PR). A price that cannot be
// computed stops the key (fail closed).
func (c *call) charge() {
	cur, ok := c.g.snap.Load().state.Limits[c.agent]
	if !ok {
		return
	}
	u := pricing.Usage{
		Input: c.rec.InputTokens, CacheRead: c.rec.CacheReadTokens, CacheWrite: c.rec.CacheWriteTokens,
		Output: c.rec.OutputTokens, Reasoning: c.rec.ReasoningTokens,
	}
	if ok2xx := c.rec.Status >= 200 && c.rec.Status < 300; ok2xx {
		if !c.rec.InputKnown {
			u.Input = max(c.rec.RequestBytes, 0) / 4
		}
		if !c.rec.OutputKnown || c.rec.Error != "" {
			u.Output = max(u.Output, c.rec.MaxTokens)
		}
	} else if !c.rec.InputKnown && !c.rec.OutputKnown {
		return // an error answer or a refusal: nothing was generated
	}
	_, cost, err := c.price.Cost(u)
	if err != nil {
		c.g.o.Logger.Error("model call cannot be priced", "reason", "cost")
		cost = money.USD(math.MaxInt64)
	}
	c.g.bud.charge(c.agent, cur, cost, c.g.o.Now())
}

// emit finishes the record and sends it, once.
func (c *call) emit(result string, edit func(*gateway.UsageRecord)) {
	c.once.Do(func() {
		if edit != nil {
			edit(&c.rec)
		}
		c.g.o.Metrics.request(result)
		c.charge() // before the record leaves, so the agent's next call already sees the spend
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
func (c *call) response(resp *http.Response, rt route, format modelformat.Format, secret string) error {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// A redirect is never followed: it could lead the provider key to another host.
		_ = resp.Body.Close()
		return errRedirect
	}
	c.watchEcho(resp, rt, secret)
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

// watchEcho counts and logs a provider key that the upstream echoes back in a response header or body (#1332,
// ADR 0097 section 15), once per response. It detects only: the bytes reach the agent unchanged (an accepted
// risk for v0.2). The body watch streams with the body and never buffers it. A 1xx answer is not wrapped: a 101
// upgrade needs its body to stay an io.ReadWriteCloser.
func (c *call) watchEcho(resp *http.Response, rt route, secret string) {
	w := newEchoWatch(secret)
	if w == nil || resp.StatusCode < 200 {
		return
	}
	answer := answerError
	if resp.StatusCode < 300 {
		answer = answerSuccess
	}
	status := resp.StatusCode
	var once sync.Once
	report := func(where string) {
		once.Do(func() {
			c.g.o.Metrics.keyEcho(answer)
			// Never the key, the body or a part of them: the provider name, the status and where only.
			c.g.o.Logger.Warn("provider key echoed in a model response", "provider", rt.p.Name, "status", status,
				"in", where)
		})
	}
	for _, vs := range resp.Header {
		if slices.ContainsFunc(vs, func(v string) bool { return strings.Contains(v, secret) }) {
			report("header")
			break
		}
	}
	resp.Body = &echoBody{ReadCloser: resp.Body, watch: w, seen: func() { report("body") }}
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
