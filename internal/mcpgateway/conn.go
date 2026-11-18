// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
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
	inner  mcp.Transport
	guard  Guard
	target string
	agent  gateway.AgentRef

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
		inner: inner, guard: t.guard, target: t.target, agent: t.agent,
		in: make(chan readResult, 16), done: make(chan struct{}), pending: map[string]pendingCall{},
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
	inner  mcp.Connection
	guard  Guard
	target string
	agent  gateway.AgentRef

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
	// unsolicitedTimeout bounds the guard on a message that answers no call of ours.
	unsolicitedTimeout = 30 * time.Second
)

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
	return Message{Target: c.target, Agent: c.agent, Dir: dir, Kind: kind, Method: method, Raw: raw}
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
		if v := c.guard.Inspect(ctx, c.message(ToUpstream, kind, "", m)); v.Refuse {
			return c.inner.Write(ctx, errorResponse(m.ID, refusedCode, v.Reason))
		}
		return c.inner.Write(ctx, msg)
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
		c.pending[idKey(m.ID)] = pendingCall{method: m.Method, ctx: ctx}
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
			select {
			case c.notes <- msg:
			default: // the queue is full: drop the notification
			}
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
			c.overloaded(msg)
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
			ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), unsolicitedTimeout)
			return ctx, cancel, p
		}
	}
	ctx, cancel := context.WithTimeout(c.life, unsolicitedTimeout)
	return ctx, cancel, p
}

// overloaded answers a message the connection has no room to inspect. A response becomes an error
// for its call; a server request is refused to the target.
func (c *guardConn) overloaded(msg jsonrpc.Message) {
	switch m := msg.(type) {
	case *jsonrpc.Response:
		c.mu.Lock()
		delete(c.pending, idKey(m.ID))
		c.mu.Unlock()
		// No goroutine: a hostile target must not grow one per message. The error goes in at once, or
		// the response is dropped and its call ends at its own deadline.
		select {
		case c.in <- readResult{msg: errorResponse(m.ID, jsonrpc.CodeInternalError, "gateway overloaded")}:
		default:
		}
	case *jsonrpc.Request:
		// Dropped, with no answer: the target times out. A write here would let a hostile target make
		// the gateway open one outbound request per message.
	}
}

// noteLoop inspects notifications one at a time, in the order they came.
func (c *guardConn) noteLoop() {
	for {
		select {
		case msg := <-c.notes:
			ctx, cancel := context.WithTimeout(c.life, unsolicitedTimeout)
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
