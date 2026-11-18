// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/gateway"
)

// guardTransport wraps an upstream transport so the guard sees every raw JSON-RPC message of the
// connection (ADR 0097 decision 5). It is the pattern of the SDK's LoggingTransport, with a verdict.
type guardTransport struct {
	inner   mcp.Transport
	guard   Guard
	target  string
	org     string
	cred    string        // the credential kind, for Message.Credential
	timeout time.Duration // bounds the guard on each inbound message (Options.CallTimeout)
	log     *slog.Logger
	metrics *Metrics
	agent   gateway.AgentRef

	conn atomic.Pointer[guardConn] // the connection Connect made, for the liveness check
}

// dead reports that the connection is known to be lost, so a call is not sent on it.
func (t *guardTransport) dead() bool {
	c := t.conn.Load()
	return c != nil && c.lost.Load()
}

// Connect implements mcp.Transport.
func (t *guardTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	inner, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	c := &guardConn{
		inner: inner, guard: t.guard, target: t.target, org: t.org, cred: t.cred, agent: t.agent,
		timeout: t.timeout, log: t.log, metrics: t.metrics,
		in: make(chan readResult, 16), done: make(chan struct{}), pending: map[string]pendingCall{}, owed: map[string]*owedEntry{},
		workers: make(chan struct{}, maxInboundWorkers), notes: make(chan jsonrpc.Message, maxQueuedNotes),
	}
	t.conn.Store(c)
	// The connect context may end when Connect returns; the pump lives as long as the connection.
	c.life, c.stop = context.WithCancel(context.WithoutCancel(ctx))
	go c.pump()
	go c.noteLoop()
	return c, nil
}

type readResult struct {
	msg jsonrpc.Message
	err error
}

// guardConn is the guarded connection. A goroutine reads the inner connection, so refusals can
// answer a call without a read from the inner connection.
type guardConn struct {
	inner   mcp.Connection
	guard   Guard
	target  string
	org     string
	cred    string
	timeout time.Duration
	log     *slog.Logger
	metrics *Metrics
	agent   gateway.AgentRef

	life context.Context //nolint:containedctx // the lifetime of the connection, not of a call
	stop context.CancelFunc

	in   chan readResult
	done chan struct{}
	once sync.Once

	lost    atomic.Bool   // the read side failed or the connection closed
	workers chan struct{} // admission for the goroutines that handle responses and server requests
	notes   chan jsonrpc.Message
	wg      sync.WaitGroup // the workers in flight, so a read error never overtakes a response

	mu      sync.Mutex
	pending map[string]pendingCall // request ID -> the call the gateway sent
	owed    map[string]*owedEntry  // server requests the SDK has not yet answered
}

// owedEntry is one server request the SDK has not yet answered. Its timer and its reply hold the
// pointer, so a timer of an old request cannot remove a newer entry that reuses the ID.
type owedEntry struct {
	writing bool // the reply is being written
}

// pendingCall is a call the gateway sent: its method and the context of the call, which its
// response is judged under.
type pendingCall struct {
	method string
	ctx    context.Context //nolint:containedctx // the call's own context, kept until its response
}

const (
	maxInboundWorkers = 64
	maxQueuedNotes    = 64
	// unsolicitedTimeout bounds the guard on an inbound message when the connection has no call limit
	// (a connection built without Options.CallTimeout).
	unsolicitedTimeout = 30 * time.Second
	// lateResponseGrace is how long a response to a call that gave up is still judged.
	lateResponseGrace = 30 * time.Second
)

// limit is the time the guard has for one inbound message or one reply: the call limit of the gateway
// (Options.CallTimeout), because the guard may hold a message for a person's approval.
func (c *guardConn) limit() time.Duration {
	if c.timeout > 0 {
		return c.timeout
	}
	return unsolicitedTimeout
}

// SessionID implements mcp.Connection.
func (c *guardConn) SessionID() string { return c.inner.SessionID() }

// Read implements mcp.Connection.
func (c *guardConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case r := <-c.in:
		return r.msg, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("mcpgateway: upstream connection closed")
	}
}

// Close implements mcp.Connection.
func (c *guardConn) Close() error {
	c.once.Do(func() { c.lost.Store(true); close(c.done); c.stop() })
	return c.inner.Close()
}

func idKey(id jsonrpc.ID) string {
	b, _ := json.Marshal(id.Raw())
	return string(b)
}

func (c *guardConn) deliver(r readResult) {
	select {
	case c.in <- r:
	case <-c.done:
	}
}

// errorResponse is the JSON-RPC error that stands in for a refused result.
func errorResponse(id jsonrpc.ID, code int64, text string) *jsonrpc.Response {
	return &jsonrpc.Response{ID: id, Error: &jsonrpc.Error{Code: code, Message: text}}
}

// refusedCode is the JSON-RPC error code of a message the gateway refuses. It lies in the
// implementation-defined server range.
const refusedCode = -32001

func (c *guardConn) message(dir Direction, kind Kind, method string, msg jsonrpc.Message) Message {
	raw, _ := jsonrpc.EncodeMessage(msg)
	return Message{Target: c.target, Org: c.org, Credential: c.cred, Agent: c.agent, Dir: dir, Kind: kind, Method: method, Raw: raw}
}

func (c *guardConn) violation(ctx context.Context, m Message, v Violation) {
	if r, ok := c.guard.(ViolationReporter); ok {
		r.Violation(ctx, m, v)
	}
}

// Write implements mcp.Connection: the gateway sends msg to the target.
func (c *guardConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	switch m := msg.(type) {
	case *jsonrpc.Request:
		kind := KindNotification
		if m.ID.IsValid() {
			kind = KindRequest
		}
		v := Verdict{}
		gm := c.message(ToUpstream, kind, m.Method, m)
		switch {
		case !slices.Contains(UpstreamRequests, m.Method):
			v = Verdict{Refuse: true, Reason: "refused: the method " + m.Method + " is not allowed"}
			// The guard still sees the message it refuses on this side.
			_ = c.guard.Inspect(ctx, gm)
		default:
			v = c.guard.Inspect(ctx, gm)
		}
		if v.Refuse {
			if kind == KindRequest {
				c.deliver(readResult{msg: errorResponse(m.ID, refusedCode, v.Reason)})
			}
			return nil
		}
		c.track(ctx, m, kind)
		return c.inner.Write(ctx, msg)
	case *jsonrpc.Response:
		kind := KindResult
		if m.Error != nil {
			kind = KindError
		}
		// The request keeps its place in the bound until the reply write ends. The timer cannot
		// release it while the write runs.
		ok := idKey(m.ID)
		c.mu.Lock()
		held := c.owed[ok]
		if held != nil {
			held.writing = true
		}
		c.mu.Unlock()
		if held != nil {
			// Only a request that was owed leaves the bound, and only its own entry: a late reply to a
			// request the timer already released must not remove a newer request that reuses the ID.
			defer func() {
				c.mu.Lock()
				if c.owed[ok] == held {
					delete(c.owed, ok)
				}
				c.mu.Unlock()
			}()
		}
		// The SDK writes the reply with a context that has no deadline: a target that stalls the
		// reply must not hold the connection.
		wctx, cancel := context.WithTimeout(ctx, c.limit())
		defer cancel()
		if v := c.guard.Inspect(wctx, c.message(ToUpstream, kind, "", m)); v.Refuse {
			return c.inner.Write(wctx, errorResponse(m.ID, refusedCode, v.Reason))
		}
		return c.inner.Write(wctx, msg)
	}
	return c.inner.Write(ctx, msg)
}

// track keeps the pending map right: a call adds its ID, a cancellation removes it, so a call that
// the target never answers does not stay in the map.
func (c *guardConn) track(ctx context.Context, m *jsonrpc.Request, kind Kind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case kind == KindRequest:
		k := idKey(m.ID)
		c.pending[k] = pendingCall{method: m.Method, ctx: ctx}
		// The entry must not outlive a caller that gave up, whether or not a cancellation reaches
		// the target. A late response is still judged for a short grace time, then the entry goes.
		context.AfterFunc(ctx, func() {
			time.AfterFunc(lateResponseGrace, func() {
				c.mu.Lock()
				if cur, ok := c.pending[k]; ok && cur.ctx == ctx {
					delete(c.pending, k)
				}
				c.mu.Unlock()
			})
		})
	case m.Method == "notifications/cancelled":
		var p struct {
			RequestID any `json:"requestId"`
		}
		if json.Unmarshal(m.Params, &p) == nil && p.RequestID != nil {
			if id, err := jsonrpc.MakeID(p.RequestID); err == nil {
				delete(c.pending, idKey(id))
			}
		}
	}
}

// pump reads the target and applies the guard to each message before the SDK sees it.
func (c *guardConn) pump() {
	ctx := c.life
	for {
		msg, err := c.inner.Read(ctx)
		if err != nil {
			c.lost.Store(true)
			// Deliver what is in flight first: a read error must not overtake a response already read.
			go func() {
				c.wg.Wait()
				c.deliver(readResult{err: err})
			}()
			return
		}
		// The guard may block (an ask held for a person), so the reader never runs it. A notification
		// goes to one queue, which keeps its order. A response or a server request gets a goroutine
		// from a bounded set; when the set is full the message fails closed.
		if m, ok := msg.(*jsonrpc.Request); ok && !m.ID.IsValid() {
			c.enqueueNote(msg)
			continue
		}
		select {
		case c.workers <- struct{}{}:
			c.wg.Add(1)
			go func() {
				defer func() { <-c.workers; c.wg.Done() }()
				c.handle(msg)
			}()
		default:
			if err := c.overloaded(msg); err != nil {
				// A response has no room: the connection ends. Calls in flight finish first, then
				// every later call fails at once and the pool replaces the session.
				c.lost.Store(true)
				go func() {
					c.wg.Wait()
					c.deliver(readResult{err: err})
				}()
				return
			}
		}
	}
}

// enqueueNote queues a notification for inspection. A full queue drops it: the drop is counted and
// logged with fixed fields only (ADR 0023), no method, org or agent.
func (c *guardConn) enqueueNote(msg jsonrpc.Message) {
	select {
	case c.notes <- msg:
	default:
		c.metrics.noteDropped()
		if c.log != nil {
			c.log.Warn("mcp notification dropped", "reason", "queue_full")
		}
	}
}

// handle runs the guard on a response or a server request and passes the result on.
func (c *guardConn) handle(msg jsonrpc.Message) {
	ctx, cancel, p := c.contextFor(msg)
	defer cancel()
	if out, ok := c.inbound(ctx, msg, p); ok {
		c.deliver(readResult{msg: out})
	}
}

// contextFor returns the context a message is judged under: the call's own context for a response,
// a bounded one for anything that answers no call of ours. It takes the pending record of a response
// once, so the context and the method come from the same record.
func (c *guardConn) contextFor(msg jsonrpc.Message) (context.Context, context.CancelFunc, pendingCall) {
	var p pendingCall
	if m, ok := msg.(*jsonrpc.Response); ok {
		c.mu.Lock()
		p = c.pending[idKey(m.ID)]
		delete(c.pending, idKey(m.ID))
		c.mu.Unlock()
		if p.ctx != nil {
			// The values of the call (trace, agent) stay; its cancellation does not, so a late
			// response is still judged and reported.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), c.limit())
			return ctx, cancel, p
		}
	}
	ctx, cancel := context.WithTimeout(c.life, c.limit())
	return ctx, cancel, p
}

// overloaded handles a message the connection has no room to inspect. A server request is dropped:
// the target times out, and a write here would let a hostile target make the gateway open one
// outbound request per message. A response cannot be dropped, because its call would wait to its
// deadline: it returns an error that ends the connection.
func (c *guardConn) overloaded(msg jsonrpc.Message) error {
	m, ok := msg.(*jsonrpc.Response)
	if !ok {
		return nil
	}
	c.mu.Lock()
	delete(c.pending, idKey(m.ID))
	c.mu.Unlock()
	return errors.New("mcpgateway: the target sent more replies than the gateway can inspect")
}

// noteLoop inspects notifications one at a time, in the order they came.
func (c *guardConn) noteLoop() {
	for {
		select {
		case msg := <-c.notes:
			ctx, cancel := context.WithTimeout(c.life, c.limit())
			if out, ok := c.inbound(ctx, msg, pendingCall{}); ok {
				c.deliver(readResult{msg: out})
			}
			cancel()
		case <-c.done:
			return
		}
	}
}

// inbound returns the message to hand to the SDK, or false when the message stops here.
func (c *guardConn) inbound(ctx context.Context, msg jsonrpc.Message, p pendingCall) (jsonrpc.Message, bool) {
	switch m := msg.(type) {
	case *jsonrpc.Request:
		if !m.ID.IsValid() {
			v := c.guard.Inspect(ctx, c.message(FromUpstream, KindNotification, m.Method, m))
			if v.Refuse || !slices.Contains(UpstreamPushes, m.Method) {
				return nil, false
			}
			return m, true
		}
		return c.serverRequest(ctx, m)
	case *jsonrpc.Response:
		method := p.method
		kind := KindResult
		if m.Error != nil {
			kind = KindError
		}
		gm := c.message(FromUpstream, kind, method, m)
		if kind == KindResult && isInputRequired(m.Result) {
			c.violation(ctx, gm, ViolationInputRequired)
			_ = c.guard.Inspect(ctx, gm)
			return errorResponse(m.ID, refusedCode, "refused: the server asked for input, which the gateway does not offer"), true
		}
		if v := c.guard.Inspect(ctx, gm); v.Refuse {
			return errorResponse(m.ID, refusedCode, v.Reason), true
		}
		var we *jsonrpc.Error
		if errors.As(m.Error, &we) && we.Code == refusedCode {
			// Only the gateway makes this code. A target must not forge a guard refusal.
			return errorResponse(m.ID, jsonrpc.CodeInternalError, "upstream error"), true
		}
		return m, true
	}
	return msg, true
}

// serverRequest handles a request the target sent. The gateway advertised no sampling and no
// elicitation, so those are refused here, with a violation for the guard (ADR 0097 decision 5).
func (c *guardConn) serverRequest(ctx context.Context, m *jsonrpc.Request) (jsonrpc.Message, bool) {
	gm := c.message(FromUpstream, KindRequest, m.Method, m)
	refuse := func(code int64, text string, viol Violation) (jsonrpc.Message, bool) {
		if viol != "" {
			c.violation(ctx, gm, viol)
		}
		_ = c.guard.Inspect(ctx, gm)
		_ = c.inner.Write(ctx, errorResponse(m.ID, code, text))
		return nil, false
	}
	switch m.Method {
	case "sampling/createMessage":
		return refuse(jsonrpc.CodeMethodNotFound, "refused: the gateway offers no sampling", ViolationSampling)
	case "elicitation/create":
		return refuse(jsonrpc.CodeMethodNotFound, "refused: the gateway offers no elicitation", ViolationElicitation)
	case "ping", "roots/list":
		if v := c.guard.Inspect(ctx, gm); v.Refuse {
			return refuse(refusedCode, v.Reason, "")
		}
		// The SDK writes the reply later. Until it does, the request counts against a bound, so a
		// target cannot pile up unanswered requests; the count drops at the reply or at a timeout.
		k := idKey(m.ID)
		c.mu.Lock()
		if len(c.owed) >= maxInboundWorkers {
			c.mu.Unlock()
			return refuse(refusedCode, "refused: too many requests wait for a reply", "")
		}
		e := &owedEntry{}
		c.owed[k] = e
		c.mu.Unlock()
		time.AfterFunc(c.limit(), func() {
			c.mu.Lock()
			// Only this request's own entry: a newer request may reuse the ID.
			if cur := c.owed[k]; cur == e && !e.writing {
				delete(c.owed, k)
			}
			c.mu.Unlock()
		})
		return m, true
	}
	return refuse(jsonrpc.CodeMethodNotFound, "refused: the method "+m.Method+" is not allowed", ViolationUnknownRequest)
}

// isInputRequired reports a result of MCP 2026-07-28 multi round-trip requests that asks the
// client for more input (resultType "input_required").
func isInputRequired(result json.RawMessage) bool {
	var r struct {
		ResultType string `json:"resultType"`
	}
	return json.Unmarshal(result, &r) == nil && r.ResultType == "input_required"
}
