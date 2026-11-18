// SPDX-License-Identifier: Apache-2.0

package mcpgateway

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/stateracloud/koinon/internal/gateway"
)

var errDraining = errors.New("mcpgateway: draining")

type agentKey struct{}

// Handler serves agents: the MCP endpoint, Streamable HTTP in the SDK's stateless mode. An agent
// authenticates with its gateway key as a bearer token. Mount it at /mcp.
func (g *Gateway) Handler() http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		agent, _ := r.Context().Value(agentKey{}).(gateway.AgentRef)
		return g.serverFor(agent)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		draining := g.drain
		g.mu.Unlock()
		if draining {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		// A key is a header credential: a URL or another header that carries one is refused.
		if strings.Contains(r.URL.String(), gateway.KeyPrefix) || keyInOtherHeader(r.Header) {
			refuse(w, http.StatusForbidden, "refused: a gateway key belongs in the Authorization header only")
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			refuse(w, http.StatusUnauthorized, "refused: no gateway key")
			return
		}
		key, ok := g.snap.Load().authenticate(strings.TrimSpace(token))
		if !ok {
			refuse(w, http.StatusUnauthorized, "refused: unknown gateway key")
			return
		}
		// The agent's request headers stop here: the upstream requests are built by the SDK client.
		mcpHandler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, key.Agent)))
	})
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

// refuse answers with a final refusal: the do-not-retry marker (gateway.DoNotRetry) tells a client
// not to try again.
func refuse(w http.ResponseWriter, status int, text string) {
	w.Header().Set(gateway.DoNotRetryHeader, gateway.DoNotRetryValue)
	http.Error(w, text, status)
}

// serverFor builds the server of one request. It registers no tool: its middleware answers
// tools/list and tools/call from the agent's view of the targets.
func (g *Gateway) serverFor(agent gateway.AgentRef) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "koinon-gateway", Version: "0"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		Logger:       g.o.Logger,
	})
	srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if !slices.Contains(AgentMethods, method) {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "refused: the method " + method + " is not allowed"}
			}
			switch method {
			case "tools/list":
				return g.listTools(ctx, agent), nil
			case "tools/call":
				r, ok := req.(*mcp.CallToolRequest)
				if !ok {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "bad tools/call"}
				}
				return g.callTool(ctx, agent, r.Params), nil
			}
			return next(ctx, method, req)
		}
	})
	return srv
}

// listTools fans out to every target the agent may use and merges the granted tools. A target that
// fails is left out: one dead server must not hide the others.
func (g *Gateway) listTools(ctx context.Context, agent gateway.AgentRef) *mcp.ListToolsResult {
	s := g.snap.Load()
	out := &mcp.ListToolsResult{Tools: []*mcp.Tool{}}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for name, t := range s.targets {
		tools, all, ok := s.allowedTools(agent, name)
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			lctx, cancel := context.WithTimeout(ctx, g.o.ListTimeout)
			defer cancel()
			got, err := g.targetTools(lctx, t, agent)
			if err != nil {
				g.o.Logger.Warn("tools/list: target skipped", "target", name, "error", errKind(err))
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, tool := range got {
				if !all && !tools[tool.Name] {
					continue
				}
				c := *tool
				c.Name = toolName(name, tool.Name)
				out.Tools = append(out.Tools, &c)
			}
		}()
	}
	wg.Wait()
	slices.SortFunc(out.Tools, func(a, b *mcp.Tool) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (g *Gateway) targetTools(ctx context.Context, t gateway.MCPTarget, agent gateway.AgentRef) ([]*mcp.Tool, error) {
	var lastErr error
	for range 2 {
		u, err := g.session(ctx, t, agent)
		if err != nil {
			return nil, err
		}
		var tools []*mcp.Tool
		for tool, err := range u.session.Tools(ctx, nil) {
			if err != nil {
				lastErr = err
				break
			}
			tools = append(tools, tool)
			lastErr = nil
		}
		if lastErr == nil {
			return tools, nil
		}
		g.evict(u)
	}
	return nil, lastErr
}

// errKind is a loggable form of an error: its type, never its text, which can hold a URL with a token.
func errKind(err error) string {
	var je *jsonrpc.Error
	if errors.As(err, &je) {
		return "jsonrpc_error"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "error"
}

func refusedResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// callTool sends one tools/call to the target that owns the tool. Grants and org visibility are
// checked first, so the target is not even contacted for a call the agent may not make. It writes one
// span and one audit row.
func (g *Gateway) callTool(ctx context.Context, agent gateway.AgentRef, p *mcp.CallToolParamsRaw) *mcp.CallToolResult {
	start := time.Now()
	row := AuditRow{Agent: agent}
	var span trace.Span
	if g.o.Tracer != nil {
		ctx, span = g.o.Tracer.Start(ctx, "mcp.tools/call")
		defer span.End()
	}
	finish := func(o Outcome) {
		row.Outcome, row.Duration = o, time.Since(start)
		if span != nil {
			span.SetAttributes(attribute.String("mcp.target", row.Target), attribute.String("mcp.tool", row.Tool),
				attribute.String("mcp.outcome", string(o)), attribute.String("mcp.credential", row.CredKind))
			if o != OutcomeOK {
				span.SetStatus(codes.Error, string(o))
			}
		}
		if g.o.Audit != nil {
			if err := g.o.Audit.Record(ctx, row); err != nil {
				g.o.Logger.Error("audit row not written", "error", errKind(err))
			}
		}
	}

	s := g.snap.Load()
	target, tool, ok := splitToolName(p.Name)
	row.Target, row.Tool, row.CredKind = target, tool, CredentialNone
	if !ok {
		finish(OutcomeRefusedGrant)
		return refusedResult("refused: no such tool")
	}
	tools, all, allowed := s.allowedTools(agent, target)
	if !allowed || (!all && !tools[tool]) {
		finish(OutcomeRefusedGrant)
		return refusedResult("refused: the tool is not granted to this agent")
	}
	t := s.targets[target]
	if c, err := g.o.Credentials.Credential(ctx, t, agent); err == nil {
		row.CredKind = c.Kind
	}

	cctx, cancel := context.WithTimeout(ctx, g.o.CallTimeout)
	defer cancel()
	var res *mcp.CallToolResult
	var err error
	for attempt := range 2 {
		var u *upstream
		if u, err = g.session(cctx, t, agent); err != nil {
			break
		}
		res, err = u.session.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: p.Arguments})
		if err == nil {
			break
		}
		var je *jsonrpc.Error
		if errors.As(err, &je) {
			break // the target or the guard answered: the session is fine
		}
		g.evict(u)
		if attempt == 1 || !isReplayable(err) {
			break
		}
	}
	switch {
	case err == nil && res.IsError:
		finish(OutcomeToolError)
		return res
	case err == nil:
		finish(OutcomeOK)
		return res
	}
	var je *jsonrpc.Error
	if errors.As(err, &je) && je.Code == refusedCode {
		row.Verdict = je.Message
		finish(OutcomeRefusedGuard)
		return refusedResult(je.Message)
	}
	finish(OutcomeUpstreamError)
	return refusedResult("the MCP server did not answer")
}

// isReplayable says a failed call may run again on a fresh session. Only a call that never reached
// the target is replayed; a lost connection mid-call could repeat a write, so it is not.
func isReplayable(err error) bool { return errors.Is(err, mcp.ErrConnectionClosed) }
