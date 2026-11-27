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

var (
	errDraining      = errors.New("mcpgateway: draining")
	errConfigChanged = errors.New("mcpgateway: the target config changed")
)

type agentKey struct{}

// reqKey carries the snapshot that authenticated a request, so the grants of one request come from
// the same configuration as its key.
type reqKey struct{}

func (g *Gateway) snapOf(ctx context.Context) *snapshot {
	if s, ok := ctx.Value(reqKey{}).(*snapshot); ok && s != nil {
		return s
	}
	return g.snap.Load()
}

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
		token = strings.TrimSpace(token)
		snap := g.snap.Load()
		var agent gateway.AgentRef
		if g.o.Actors != nil && !gateway.IsGatewayKey(token) {
			// A Substrate guest: atenet-egress put the actor JWT here (issue #1489). No fallback to a key.
			a, refusal := gateway.IdentifyActor(g.o.Actors, token)
			if refusal != nil {
				g.o.Logger.Info("refused an MCP request: actor token", "reason", refusal.Reason, "status", refusal.Status)
				refuseActor(w, refusal)
				return
			}
			agent = a
		} else {
			key, ok := snap.authenticate(token)
			if !ok {
				refuse(w, http.StatusUnauthorized, "refused: unknown gateway key")
				return
			}
			agent = key.Agent
		}
		// The agent's request headers stop here: the upstream requests are built by the SDK client, so the
		// actor JWT never goes upstream.
		rctx := context.WithValue(context.WithValue(r.Context(), agentKey{}, agent), reqKey{}, snap)
		// The logs, spans and audit rows of the request belong to the agent's org: they are scrubbed with the
		// credential values of that org only (#1174).
		rctx = WithScrubOrg(rctx, agent.Org)
		mcpHandler.ServeHTTP(w, r.WithContext(rctx))
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

// refuseActor answers an actor JWT that is not accepted. A 503 (no issuer keys) is not final.
func refuseActor(w http.ResponseWriter, r *gateway.ActorRefusal) {
	if r.Final {
		refuse(w, r.Status, r.Text)
		return
	}
	w.Header().Set("Retry-After", "5")
	http.Error(w, r.Text, r.Status)
}

// serverFor builds the server of one request. It registers no tool and no prompt: its middleware
// answers tools/list and tools/call from the agent's view of the targets, and prompts/list and
// prompts/get with the readiness prompt only (prompts.go).
func (g *Gateway) serverFor(agent gateway.AgentRef) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "koinon-gateway", Version: "0"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}, Prompts: &mcp.PromptCapabilities{}},
		Logger:       g.o.Logger,
	})
	srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if !slices.Contains(AgentMethods, method) {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "refused: the method " + method + " is not allowed"}
			}
			switch method {
			case "tools/list":
				return g.o.Scrub.ToolList(agent.Org, g.listTools(ctx, agent)), nil
			case "tools/call":
				r, ok := req.(*mcp.CallToolRequest)
				if !ok {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "bad tools/call"}
				}
				return g.o.Scrub.Result(agent.Org, g.callTool(ctx, agent, r.Params)), nil
			case "prompts/list":
				return g.listPrompts(ctx, agent), nil
			case "prompts/get":
				r, ok := req.(*mcp.GetPromptRequest)
				if !ok {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "bad prompts/get"}
				}
				return g.getPrompt(ctx, agent, r.Params)
			}
			return next(ctx, method, req)
		}
	})
	return srv
}

// listTools fans out to every target the agent may use and merges the granted tools. A target that
// fails is left out: one dead server must not hide the others.
func (g *Gateway) listTools(ctx context.Context, agent gateway.AgentRef) *mcp.ListToolsResult {
	s := g.snapOf(ctx)
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
				g.o.Logger.WarnContext(scrubCtx(ctx, agent), "tools/list: target skipped", "target", name, "error", errKind(err))
				return
			}
			// The text of each tool is the approved text, or it is left out (#1132).
			got, _, ok := g.vetted(lctx, t, agent, got)
			if !ok {
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
	if t.Builtin != "" {
		if b := g.o.Builtins[t.Builtin]; b != nil {
			return b.Tools(), nil
		}
		return nil, errors.New("mcpgateway: no code for the built-in target")
	}
	var lastErr error
	for range 2 {
		u, err := g.session(ctx, t, agent)
		if err != nil {
			return nil, err
		}
		var tools []*mcp.Tool
		for tool, err := range u.session.Tools(scrubCtx(ctx, agent), nil) {
			if err != nil {
				lastErr = err
				break
			}
			tools = append(tools, tool)
			lastErr = nil
		}
		if lastErr == nil {
			u.release()
			// A target can quote the credential it was sent in a tool's text. The list is scrubbed with the
			// values of the org as it enters, so the tool-list check, the pin hash, the prober and the agent
			// see one text (#1174). The guard's message inspection on the wire still sees the raw list: it must,
			// to judge it.
			return g.o.Scrub.Tools(agent.Org, tools), nil
		}
		u.release()
		if ctx.Err() == nil {
			g.evict(u)
		}
	}
	return nil, lastErr
}

// scrubCtx marks ctx with the org of agent, unless it is marked already: its logs and spans are scrubbed with
// the values of that org.
func scrubCtx(ctx context.Context, agent gateway.AgentRef) context.Context {
	if _, ok := scrubOrgOf(ctx); ok {
		return ctx
	}
	return WithScrubOrg(ctx, agent.Org)
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
	ctx = scrubCtx(ctx, agent)
	var span trace.Span
	if g.o.Tracer != nil {
		ctx, span = g.o.Tracer.Start(ctx, "mcp.tools/call")
		defer span.End()
	}
	finish := func(o Outcome) {
		row.Outcome, row.Duration = o, time.Since(start)
		// A guard reason may quote the text it refused, and the tool name of an "all" grant is the
		// agent's text. The audit row, its log line and the span never carry an opened credential. They
		// belong to the agent's org, so only that org's values are removed: another org cannot change
		// them (#1174).
		row.Target = g.o.Scrub.TextFor(agent.Org, row.Target)
		row.Tool = g.o.Scrub.TextFor(agent.Org, row.Tool)
		row.Verdict = g.o.Scrub.TextFor(agent.Org, row.Verdict)
		if span != nil {
			span.SetAttributes(attribute.String("mcp.target", row.Target), attribute.String("mcp.tool", row.Tool),
				attribute.String("mcp.outcome", string(o)), attribute.String("mcp.credential", row.CredKind))
			if o != OutcomeOK {
				span.SetStatus(codes.Error, string(o))
			}
		}
		if g.o.Audit != nil {
			if err := g.o.Audit.Record(ctx, row); err != nil {
				g.o.Logger.ErrorContext(ctx, "audit row not written", "error", errKind(err))
			}
		}
	}

	s := g.snapOf(ctx)
	// The row and the span carry names only after the grant check: a caller's text is never recorded.
	row.Target, row.Tool, row.CredKind = "unknown", "unknown", CredentialNone.String()
	target, tool, ok := splitToolName(p.Name)
	if !ok {
		finish(OutcomeRefusedGrant)
		return refusedResult("refused: no such tool")
	}
	tools, all, allowed := s.allowedTools(agent, target)
	if s.needsGuard(agent, target, tool) {
		// Granted, but the target serves nothing without a guard: say so, not "not granted".
		row.Target, row.Tool = target, tool
		row.Verdict = gateway.GuardUnconfiguredReason
		finish(OutcomeRefusedGuard)
		return refusedResult(gateway.GuardUnconfiguredReason)
	}
	if !allowed || (!all && !tools[tool]) {
		finish(OutcomeRefusedGrant)
		return refusedResult("refused: the tool is not granted to this agent")
	}
	row.Target, row.Tool = target, tool
	t := s.targets[target]
	// A replica that has not checked this target's list lately checks it now: the list may have been
	// served by another replica, and a changed tool must not pass because this one never saw it.
	if key := (poolKey{target: target, agent: agent}); g.g.checksToolLists() && !g.left.fresh(key, toolPinTTL) && !g.recheck(ctx, t, agent) {
		row.Verdict = reasonToolListUnreadable
		finish(OutcomeRefusedGuard)
		return refusedResult(reasonToolListUnreadable)
	}
	// A tool that a list left out (it changed since an admin approved it, or no admin reviewed it yet):
	// no call reaches the target.
	if reason, left := g.left.refusal(poolKey{target: target, agent: agent}, tool); left {
		row.Verdict = reason
		finish(OutcomeRefusedGuard)
		return refusedResult(reason)
	}
	if c, err := g.creds.Credential(ctx, t, agent); err == nil {
		row.CredKind, row.RecipientKeyID = c.Kind.String(), c.RecipientKeyID
	} else {
		row.CredKind = CredentialError.String()
		var oe *OpenError
		if errors.As(err, &oe) {
			row.RecipientKeyID = oe.KeyID
		}
	}

	// The call check is the ask tier and the request-side checks (ADR 0097 decision 5). It runs after
	// the grants and before any upstream message, and it may wait for a person; CallTimeout bounds it.
	kctx, kcancel := context.WithTimeout(ctx, g.o.CallTimeout)
	v := g.g.checkCall(kctx, Call{
		Agent: agent, Target: target, Org: t.Org, Tool: tool, Credential: row.CredKind, Arguments: p.Arguments,
	})
	kcancel()
	if v.Refuse {
		row.Verdict = v.Reason
		finish(OutcomeRefusedGuard)
		return refusedResult(v.Reason)
	}
	if t.Builtin != "" {
		return g.callBuiltin(ctx, agent, t, tool, p.Arguments, &row, finish)
	}
	// The upstream call has its own full limit: a long wait for a person must not shorten it.
	cctx, cancel := context.WithTimeout(ctx, g.o.CallTimeout)
	defer cancel()
	// One attempt. A call is never sent again: a lost session error can reach every call on a shared
	// session, so it does not prove that this call ran nothing, and a write could run twice. The
	// session is evicted, and the next call connects again.
	u, err := g.session(cctx, t, agent)
	var res *mcp.CallToolResult
	if err == nil {
		res, err = u.session.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: p.Arguments})
		u.release()
		if err == nil {
			// The result is scrubbed with the values of the org as it enters (#1174).
			res = g.o.Scrub.Result(agent.Org, res)
		}
		var je *jsonrpc.Error
		if err != nil && !errors.As(err, &je) && cctx.Err() == nil {
			g.evict(u) // a lost connection: the next call connects again
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
