// SPDX-License-Identifier: Apache-2.0

// Package mcpask is the approval-gated proxy for the ask-tier tools of org MCP servers (ADR 0008,
// 0042, plan item MC1).
//
// agentgateway offers an agent a server's auto-tier tools on the server's own target. Its ask-tier
// tools it offers here: the gateway target "<org>-<server>-ask" points at /mcp-ask/<server> on
// koinon-api's tools listener, which lists the same tools (name, description and input schema as the
// server published them) and answers a call at once with "approval requested", exactly as the GitHub
// tools do (package githubtools). Nothing reaches the remote server here. After a human approves the
// exact arguments, the approval service calls the server itself (package approvals) and the run
// resumes with the result.
//
// # Identity
//
// agentgateway sets gateway.AgentHeader to the authenticated key's agent, overwriting what the
// client sent, and the listener is reachable from the gateway only (the chart's NetworkPolicy). Which
// tools an agent sees is decided at the gateway from the rules the reconciler renders; the proxy
// checks the agent's allowance, the server's state and the tool's tier again for every call.
package mcpask

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/approvalscope"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/project"
)

// ConnectorID is the connector recorded on the approval requests of MCP calls.
const ConnectorID = "mcp"

// StatusTool is the tool that reports the state of an approval request, like the GitHub tools'.
const StatusTool = mcpservers.AskStatusTool

// Path is where the listener serves the proxy: Path + "<server name>".
const Path = "/mcp-ask/"

// MaxArgsBytes bounds the arguments of one call.
const MaxArgsBytes = 256 << 10

// Target is the approval request's target for a server: "mcp:<server name>".
func Target(server string) string { return ConnectorID + ":" + server }

// Options configures a Server.
type Options struct {
	Logger *slog.Logger
	// ApprovalTTL defaults to githubtools.DefaultApprovalTTL.
	ApprovalTTL time.Duration
	Now         func() time.Time
	// Assess is the leak guard's view of an ask-tier call (mcpguard.Guard.Assess): a risk to show
	// on the call's one card, nil for none. Its error refuses the call, and its text is what the
	// agent reads. Nil adds nothing.
	Assess func(ctx context.Context, org store.Org, agent store.Agent, srv store.McpServer, tool string, args json.RawMessage) (map[string]any, error)
}

// Server is the ask-tier proxy.
type Server struct {
	pool   *pgxpool.Pool
	q      *store.Queries
	log    *slog.Logger
	ttl    time.Duration
	now    func() time.Time
	assess func(ctx context.Context, org store.Org, agent store.Agent, srv store.McpServer, tool string, args json.RawMessage) (map[string]any, error)
}

// New returns a Server.
func New(pool *pgxpool.Pool, opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ApprovalTTL <= 0 {
		opts.ApprovalTTL = githubtools.DefaultApprovalTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Server{pool: pool, q: store.New(pool), log: opts.Logger, ttl: opts.ApprovalTTL, now: opts.Now, assess: opts.Assess}
}

var errNoIdentity = errors.New("this request does not come from an agent of Koinon")

// caller is the agent behind a request and the server it addresses.
type caller struct {
	org    store.Org
	agent  store.Agent
	server store.McpServer
	// allowed is the agent's allowance of the server; tools nil allows all of them.
	allowed store.AgentMcpServer
}

func (s *Server) identify(ctx context.Context, header http.Header, serverName string) (caller, error) {
	ref, err := gateway.AgentFromHeader(header)
	if err != nil {
		return caller{}, errNoIdentity
	}
	var agentID pgtype.UUID
	if err := agentID.Scan(ref.Name); err != nil {
		return caller{}, errNoIdentity
	}
	org, err := s.q.GetOrgBySlug(ctx, ref.Org)
	if err != nil {
		return caller{}, s.lookupErr(err, "org")
	}
	agent, err := s.q.GetAgent(ctx, store.GetAgentParams{OrgID: org.ID, ID: agentID})
	if err != nil {
		return caller{}, s.lookupErr(err, "agent")
	}
	if agent.Status != store.AgentStatusActive {
		return caller{}, errors.New("this agent is not active")
	}
	srv, err := s.q.GetServedMCPServerByName(ctx, store.GetServedMCPServerByNameParams{OrgID: org.ID, Name: serverName})
	if err != nil {
		return caller{}, s.lookupErr(err, "server")
	}
	allowed, err := s.q.GetAgentMCPServer(ctx, store.GetAgentMCPServerParams{OrgID: org.ID, AgentID: agent.ID, ServerID: srv.ID})
	if err != nil {
		return caller{}, s.lookupErr(err, "allowance")
	}
	return caller{org: org, agent: agent, server: srv, allowed: allowed}, nil
}

func (s *Server) lookupErr(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoIdentity
	}
	s.log.Error("mcp ask: lookup failed", "what", what, "err", err)
	return errors.New("internal error; try again later")
}

// askTools are the tools the caller may request: present, in the ask tier, within its allowance.
func (s *Server) askTools(ctx context.Context, c caller) ([]store.McpTool, error) {
	if c.server.Health == store.McpHealthUnhealthy {
		return nil, nil
	}
	tools, err := s.q.ListMCPToolsOfServer(ctx, store.ListMCPToolsOfServerParams{OrgID: c.org.ID, ServerID: c.server.ID})
	if err != nil {
		return nil, err
	}
	var out []store.McpTool
	for _, t := range tools {
		if t.Present && t.Tier == store.McpTierAsk && t.Name != StatusTool && (c.allowed.Tools == nil || slices.Contains(c.allowed.Tools, t.Name)) &&
			mcpservers.ToolNameProblem(c.org.Slug, c.server.Name, t.Name) == "" {
			out = append(out, t)
		}
	}
	return out, nil
}

// Handler serves the proxy over Streamable HTTP, statelessly: each request builds the tool list it
// answers from the database. Mount it at Path on a listener only agentgateway can reach.
func (s *Server) Handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		name := strings.Trim(strings.TrimPrefix(r.URL.Path, Path), "/")
		return s.serverFor(r, name)
	}, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, Stateless: true})
}

// serverFor builds the MCP server for one request. An unknown caller gets a server without tools.
func (s *Server) serverFor(r *http.Request, serverName string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: project.Name + "-mcp-ask", Version: project.Version}, nil)
	c, err := s.identify(r.Context(), r.Header, serverName)
	if err != nil {
		return srv
	}
	tools, err := s.askTools(r.Context(), c)
	if err != nil {
		s.log.Error("mcp ask: list tools", "err", err)
		return srv
	}
	for _, t := range tools {
		srv.AddTool(&mcp.Tool{
			Name: t.Name,
			Description: "Needs human approval: returns \"approval requested: id=...\" at once and your run pauses; once approved, " +
				"Koinon calls the server exactly as given and you get the result when the run resumes. " + t.Description,
			InputSchema: json.RawMessage(t.InputSchema),
		}, s.callTool(serverName, t.Name))
	}
	srv.AddTool(&mcp.Tool{
		Name:        StatusTool,
		Description: "The status of one of your approval requests and, once executed, its result.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
	}, s.status(serverName))
	return srv
}

// Result is the structured result of a proxied call, shaped like the GitHub tools'.
type Result struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"`
	Tool       string         `json:"tool"`
	Target     string         `json:"target"`
	ArgsSHA256 string         `json:"args_sha256"`
	ExpiresAt  string         `json:"expires_at,omitempty"`
	Result     map[string]any `json:"result,omitempty"`
	Note       string         `json:"note,omitempty"`
	Error      string         `json:"error,omitempty"`
}

func toolError(msg string) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// Canonical re-encodes arguments with sorted keys and no insignificant space, numbers as written, so
// the same call hashes the same.
func Canonical(raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("the arguments must be a JSON object: %w", err)
	}
	if v == nil {
		v = map[string]any{}
	}
	return json.Marshal(v)
}

func (s *Server) callTool(serverName, tool string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c, err := s.identify(ctx, req.Extra.Header, serverName)
		if err != nil {
			return toolError(err.Error())
		}
		tools, err := s.askTools(ctx, c)
		if err != nil {
			return toolError("internal error; try again later")
		}
		i := slices.IndexFunc(tools, func(t store.McpTool) bool { return t.Name == tool })
		if i < 0 {
			return toolError(fmt.Sprintf("the tool %s is not available to this agent", tool))
		}
		if len(req.Params.Arguments) > MaxArgsBytes {
			return toolError(fmt.Sprintf("the arguments are larger than %d bytes", MaxArgsBytes))
		}
		canonical, err := Canonical(req.Params.Arguments)
		if err != nil {
			return toolError(err.Error())
		}
		var risk map[string]any
		if s.assess != nil {
			risk, err = s.assess(ctx, c.org, c.agent, c.server, tool, req.Params.Arguments)
			if err != nil {
				return toolError(err.Error())
			}
		}
		return s.record(ctx, c, tools[i], canonical, risk)
	}
}

// RequestApproval records an approval request for exactly this call of an auto-tier tool that the
// leak guard holds back (ADR 0091 decision 5), with the guard's risk on the card, and returns the
// status line the agent reads. A leak approval is never covered by an approval scope: the person
// approves this call, this destination and this payload.
func (s *Server) RequestApproval(ctx context.Context, org store.Org, agent store.Agent, srv store.McpServer, t store.McpTool, args json.RawMessage, risk map[string]any) (string, error) {
	if len(args) > MaxArgsBytes {
		return fmt.Sprintf("refused: the arguments are larger than %d bytes", MaxArgsBytes), nil
	}
	canonical, err := Canonical(args)
	if err != nil {
		return "refused: " + err.Error(), nil
	}
	if risk == nil {
		risk = map[string]any{}
	}
	res, err := s.record(ctx, caller{org: org, agent: agent, server: srv}, t, canonical, risk)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if res.IsError {
		return "", errors.New(b.String())
	}
	return b.String(), nil
}

// scopeBindingMatches is true when the scope was granted for this server, URL and tool version.
// A scope without a binding does not cover an MCP call.
func scopeBindingMatches(scoped, current []byte) bool {
	var a, b mcpservers.CallBinding
	if json.Unmarshal(scoped, &a) != nil || json.Unmarshal(current, &b) != nil {
		return false
	}
	return a == b && a.ContentHash != "" && a.URLHash != ""
}

func hash(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// record stores the call as an approval request, or returns the existing one for the same call in
// the same run.
//
// A non-nil risk is the leak guard's: it is shown on the card, and no approval scope covers the
// call.
func (s *Server) record(ctx context.Context, c caller, t store.McpTool, canonical []byte, risk map[string]any) (*mcp.CallToolResult, error) {
	tool := t.Name
	riskJSON := []byte(`{}`)
	if risk != nil {
		b, err := json.Marshal(risk)
		if err != nil {
			return toolError("internal error; try again later")
		}
		riskJSON = b
	}
	run, err := s.q.GetRunningRunForAgent(ctx, store.GetRunningRunForAgentParams{OrgID: c.org.ID, AgentID: c.agent.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return toolError("this agent has no running run; tools are only available while working on a task")
	}
	if err != nil {
		s.log.Error("mcp ask: read run", "err", err)
		return toolError("internal error; try again later")
	}
	binding, err := json.Marshal(mcpservers.CallBinding{ServerID: c.server.ID.String(), URLHash: mcpservers.URLHash(c.server.Url), ContentHash: t.ContentHash})
	if err != nil {
		return toolError("internal error; try again later")
	}
	params := store.CreateApprovalRequestParams{
		OrgID: c.org.ID, TaskID: run.TaskID, RunID: run.ID, AgentID: c.agent.ID,
		Connector: ConnectorID, Tool: tool, Target: Target(c.server.Name),
		ArgsJson: string(canonical), ArgsSha256: hash(canonical), Risk: riskJSON,
		Decision: store.ApprovalDecisionAsk, Status: store.ApprovalStatusPending,
		ExpiresAt: pgtype.Timestamptz{Time: s.now().Add(s.ttl), Valid: true},
		// The call is bound to this server, URL and published version of the tool; the executor
		// refuses it if any of them changed before the approval.
		CallBinding: binding,
	}
	if t.RequiresTwoApprovers {
		params.ApprovalsRequired = pgtype.Int2{Int16: 2, Valid: true}
	}
	var row store.ApprovalRequest
	created := true
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		// A scoped approval of this agent (or task) for exactly this server and tool lets the call
		// run without parking. A tool that needs two approvers is never covered: the second
		// person cannot be waived.
		var scope store.ApprovalScope
		if !t.RequiresTwoApprovers && risk == nil {
			sc, ok, err := approvalscope.Match(ctx, q, params.OrgID, params.AgentID, params.TaskID, ConnectorID, tool, params.Target, nil, s.now())
			if err != nil {
				return err
			}
			// The scope is pinned to the version the approver saw: once the server republishes the
			// tool or its URL changes, the call parks again (ADR 0057).
			if ok && scopeBindingMatches(sc.CallBinding, binding) {
				scope = sc
				params.Decision, params.Status, params.ScopeID = store.ApprovalDecisionAllow, store.ApprovalStatusApproved, sc.ID
			}
		}
		row, err = q.CreateApprovalRequest(ctx, params)
		if errors.Is(err, pgx.ErrNoRows) {
			created = false
			row, err = q.GetApprovalRequestByCall(ctx, store.GetApprovalRequestByCallParams{
				OrgID: params.OrgID, RunID: params.RunID, Tool: tool, Target: params.Target, ArgsSha256: params.ArgsSha256,
			})
			return err
		}
		if err != nil {
			return err
		}
		pm := map[string]any{
			"approval_id": row.ID.String(), "tool": tool, "target": row.Target, "args_sha256": row.ArgsSha256,
			"decision": string(row.Decision), "risk": json.RawMessage(row.Risk), "approvals_required": row.ApprovalsRequired,
		}
		if scope.ID.Valid {
			for k, v := range approvalscope.Payload(scope) {
				pm[k] = v
			}
		}
		payload, err := json.Marshal(pm)
		if err != nil {
			return err
		}
		_, err = q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: row.OrgID, TaskID: row.TaskID, Kind: githubtools.KindApprovalRequested, ActorKind: store.TaskActorKindAgent,
			ActorAgentID: row.AgentID, RunID: row.RunID, Payload: payload,
		})
		return err
	})
	if err != nil {
		s.log.Error("mcp ask: record approval request", "org", c.org.Slug, "agent", c.agent.ID.String(), "tool", tool, "err", err)
		return toolError("internal error recording the request; try again later")
	}
	// Arguments are not logged: they can be large and are in the database.
	s.log.Info("approval requested", "org", c.org.Slug, "agent", c.agent.ID.String(), "run", run.ID.String(), "id", row.ID.String(),
		"tool", tool, "target", row.Target, "args_sha256", row.ArgsSha256, "new", created)
	return s.reply(row), nil
}

func (s *Server) status(serverName string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c, err := s.identify(ctx, req.Extra.Header, serverName)
		if err != nil {
			return toolError(err.Error())
		}
		var in struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		var id pgtype.UUID
		if err := id.Scan(strings.TrimSpace(in.ID)); err != nil || !id.Valid {
			return toolError(fmt.Sprintf("id %q is not an approval request id", in.ID))
		}
		row, err := s.q.GetApprovalRequestForAgent(ctx, store.GetApprovalRequestForAgentParams{OrgID: c.org.ID, ID: id, AgentID: c.agent.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return toolError(fmt.Sprintf("no approval request %s for this agent", in.ID))
		}
		if err != nil {
			return toolError("internal error; try again later")
		}
		return s.reply(row), nil
	}
}

func (s *Server) reply(row store.ApprovalRequest) *mcp.CallToolResult {
	out := Result{
		ID: row.ID.String(), Status: string(row.Status), Tool: row.Tool, Target: row.Target,
		ArgsSHA256: row.ArgsSha256, Note: row.DecisionNote.String, Error: row.Error.String,
	}
	if row.ExpiresAt.Valid {
		out.ExpiresAt = row.ExpiresAt.Time.UTC().Format(time.RFC3339)
	}
	if len(row.Result) > 0 {
		_ = json.Unmarshal(row.Result, &out.Result)
	}
	res := text(statusLine(out))
	res.StructuredContent = out
	return res
}

func statusLine(r Result) string {
	switch store.ApprovalStatus(r.Status) {
	case store.ApprovalStatusPending:
		return fmt.Sprintf("approval requested: id=%s status=pending args_sha256=%s. A human decides on the task; "+
			"your run pauses now and resumes with the result. Do not retry or work around this call.", r.ID, r.ArgsSHA256)
	case store.ApprovalStatusApproved:
		return fmt.Sprintf("accepted: id=%s status=approved args_sha256=%s. Koinon executes it and your run resumes with the result.", r.ID, r.ArgsSHA256)
	case store.ApprovalStatusDenied:
		return fmt.Sprintf("id=%s status=denied note=%q. The human denied this call; do not retry it.", r.ID, r.Note)
	case store.ApprovalStatusFailed:
		return fmt.Sprintf("id=%s status=failed error=%q", r.ID, r.Error)
	default:
		res, _ := json.Marshal(r.Result)
		return fmt.Sprintf("id=%s status=%s result=%s", r.ID, r.Status, res)
	}
}
