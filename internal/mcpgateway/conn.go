// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"

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
}

// Connect implements mcp.Transport.
func (t *guardTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	inner, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	c := &guardConn{
		inner: inner, guard: t.guard, target: t.target, agent: t.agent,
		in: make(chan readResult, 16), done: make(chan struct{}), pending: map[string]string{},
	}
	// The connect context may end when Connect returns; the pump lives as long as the connection.
	c.life, c.stop = context.WithCancel(context.WithoutCancel(ctx))
	go c.pump()
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

	mu      sync.Mutex
	pending map[string]string // request ID -> method, for the calls the gateway sent
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
	c.once.Do(func() { close(c.done); c.stop() })
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
		c.track(m, kind)
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
func (c *guardConn) track(m *jsonrpc.Request, kind Kind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case kind == KindRequest:
		c.pending[idKey(m.ID)] = m.Method
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
			c.deliver(readResult{err: err})
			return
		}
		// The guard may block (an ask held for a person). A response or a server request is handled
		// on its own goroutine, so one held message does not stop the others on this connection.
		if m, ok := msg.(*jsonrpc.Request); ok && !m.ID.IsValid() {
			if out, ok := c.inbound(ctx, msg); ok {
				c.deliver(readResult{msg: out})
			}
			continue
		}
		go func() {
			if out, ok := c.inbound(ctx, msg); ok {
				c.deliver(readResult{msg: out})
			}
		}()
	}
}

// inbound returns the message to hand to the SDK, or false when the message stops here.
func (c *guardConn) inbound(ctx context.Context, msg jsonrpc.Message) (jsonrpc.Message, bool) {
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
		c.mu.Lock()
		method := c.pending[idKey(m.ID)]
		delete(c.pending, idKey(m.ID))
		c.mu.Unlock()
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
