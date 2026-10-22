// SPDX-License-Identifier: Apache-2.0

// Package githubtools is the MCP server for the GitHub connector's ask-tier
// tools (plan item C3; ADR 0008, 0042, 0049), served behind agentgateway:
//
//   - create_pull_request (scope pull_request.create): branch, title, body, files;
//   - comment_on_issue (scope issue.comment): issue number and body;
//   - get_approval_status: the state of one of the agent's requests.
//
// A write call is validated, then recorded exactly as an approval request
// (canonical arguments, their SHA-256, tool, target, agent, run and ticket)
// and answered at once with "approval requested: id=…". Nothing is sent to
// GitHub here and the server holds no GitHub credential: after approval,
// Koinon executes the recorded call (plan item P1). Invalid calls are refused
// with a tool error that says why, so the agent can fix them.
//
// # Identity
//
// agentgateway authenticates the agent's key and sets the gateway.AgentHeader
// request header to the key's agent ("<org slug>/<agent id>"), overwriting
// anything the client sent. The server trusts that header and nothing else, so
// it must only be reachable from agentgateway: koinon-api serves it on its own
// listener, which the chart's NetworkPolicy opens to the gateway pods only.
// Which tools an agent sees is decided at the gateway (the CEL allowlist built
// from its grants); the server checks the grant again for every call.
package githubtools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/procoduck/koinon/connectors"
	"github.com/procoduck/koinon/internal/agentconfig"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/gateway"
	"github.com/procoduck/koinon/internal/project"
)

// Tool names.
const (
	ToolCreatePullRequest = "create_pull_request"
	ToolCommentOnIssue    = "comment_on_issue"
	ToolGetApprovalStatus = "get_approval_status"
)

// DefaultApprovalTTL is how long a request waits for a decision (ADR 0049:
// a parked run expires after 7 days).
const DefaultApprovalTTL = 7 * 24 * time.Hour

// Options configures a Server.
type Options struct {
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// ApprovalTTL defaults to DefaultApprovalTTL.
	ApprovalTTL time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
	// Execute, if set, is called in its own goroutine for a newly recorded call the grant's
	// preset allows without a human (Trusted), so it runs at once; the approval sweeper
	// executes it otherwise.
	Execute func(ctx context.Context, org, id pgtype.UUID)
}

// KindApprovalRequested is the ticket event written when a call is recorded.
const KindApprovalRequested = "approval.requested"

// Server is the GitHub tools MCP server.
type Server struct {
	pool *pgxpool.Pool
	q    *store.Queries
	log  *slog.Logger
	ttl  time.Duration
	now  func() time.Time
	ct   *connectors.Type
	mcp  *mcp.Server
	exec func(ctx context.Context, org, id pgtype.UUID)
}

// New returns a Server recording requests in pool.
func New(pool *pgxpool.Pool, opts Options) (*Server, error) {
	ct, ok := connectors.Get(connectors.GitHubID)
	if !ok {
		return nil, errors.New("githubtools: the github connector type is not embedded")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ApprovalTTL <= 0 {
		opts.ApprovalTTL = DefaultApprovalTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{pool: pool, q: store.New(pool), log: opts.Logger, ttl: opts.ApprovalTTL, now: opts.Now, ct: ct, exec: opts.Execute}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: project.Name + "-github-tools", Version: project.Version}, nil)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolCreatePullRequest,
		Description: "Propose a pull request on the ticket's repository. Needs human approval: returns " +
			"\"approval requested: id=...\" at once and your run pauses; once approved, Koinon creates the branch, " +
			"the commit and the pull request exactly as given, and you get the result when the run resumes. " +
			"A file marked executable is written as 100755; otherwise a new file is regular (100644) and a modified file keeps its mode. " +
			"Symbolic links and submodules cannot be written. " +
			"Refused: changes under .github/workflows/, more than 100 files or 1 MiB, unsafe paths.",
	}, s.createPullRequest)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolCommentOnIssue,
		Description: "Propose a comment on an issue or pull request of the ticket's repository. Needs human " +
			"approval: returns \"approval requested: id=...\" at once; Koinon posts it once approved.",
	}, s.commentOnIssue)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        ToolGetApprovalStatus,
		Description: "The status of one of your approval requests and, once executed, its result (for example the pull request URL).",
	}, s.getApprovalStatus)
	return s, nil
}

// Handler serves MCP over Streamable HTTP. Mount it on a listener only
// agentgateway can reach (see the package documentation).
func (s *Server) Handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp },
		// agentgateway connects by Service name, not localhost; the listener's
		// NetworkPolicy is the access control.
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
}

// Result is the structured result of every tool.
type Result struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Tool       string `json:"tool"`
	Target     string `json:"target"`
	ArgsSHA256 string `json:"args_sha256"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	// Result is the executed call's outcome, once there is one.
	Result map[string]any `json:"result,omitempty"`
	// Note is the approver's note on a denial.
	Note string `json:"note,omitempty"`
	// Error is why execution failed.
	Error string `json:"error,omitempty"`
}

// caller is the agent behind one tool call and the run it belongs to.
type caller struct {
	org    store.Org
	agent  store.Agent
	run    store.Run
	config agentconfig.Config
}

func (c caller) attrs() []any {
	return []any{"org", c.org.Slug, "agent", c.agent.ID.String(), "run", c.run.ID.String(), "ticket", c.run.TicketID.String()}
}

// identify resolves the gateway-set agent header to the agent's running run.
func (s *Server) identify(ctx context.Context, req *mcp.CallToolRequest) (caller, error) {
	var h string
	if req != nil && req.Extra != nil {
		h = req.Extra.Header.Get(gateway.AgentHeader)
	}
	ref, err := gateway.ParseAgentRef(h)
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
	run, err := s.q.GetRunningRunForAgent(ctx, store.GetRunningRunForAgentParams{OrgID: org.ID, AgentID: agent.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return caller{}, errors.New("this agent has no running run; tools are only available while working on a ticket")
		}
		return caller{}, s.lookupErr(err, "run")
	}
	v, err := s.q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: org.ID, ID: run.AgentVersionID})
	if err != nil {
		return caller{}, s.lookupErr(err, "agent version")
	}
	cfg, err := agentconfig.Parse(v.Config)
	if err != nil {
		return caller{}, s.lookupErr(err, "agent version")
	}
	return caller{org: org, agent: agent, run: run, config: cfg}, nil
}

func (s *Server) lookupErr(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoIdentity
	}
	s.log.Error("github tools: lookup failed", "what", what, "err", err)
	return errors.New("internal error; try again later")
}

// authorise checks the run's grant for tool and returns the preset's
// decision and the target repository.
func (s *Server) authorise(c caller, tool string) (connectors.Decision, string, error) {
	grant, ok := c.config.Grant(connectors.GitHubID)
	if !ok {
		return connectors.Deny, "", errors.New("this agent has no GitHub grant")
	}
	wt, err := LookupWriteTool(tool)
	if err != nil {
		return connectors.Deny, "", err
	}
	scope := wt.Scope
	granted := false
	for _, sc := range grant.Scopes {
		granted = granted || sc == scope
	}
	d := s.ct.Decide(grant.Preset, scope)
	if !granted || d == connectors.Deny {
		return connectors.Deny, "", fmt.Errorf("this agent's GitHub grant does not allow %s (scope %s)", tool, scope)
	}
	b, ok := c.config.Binding(connectors.GitHubID)
	switch {
	case !ok || len(b.Targets) == 0:
		return connectors.Deny, "", errors.New("this agent's GitHub grant has no repository yet; ask an admin to choose one")
	case len(b.Targets) > 1:
		return connectors.Deny, "", errors.New("this agent's GitHub grant has several repositories; v0.1 supports one per agent")
	case !s.ct.ValidTarget(b.Targets[0]):
		return connectors.Deny, "", fmt.Errorf("the granted repository %q is not owner/name", b.Targets[0])
	}
	return d, b.Targets[0], nil
}

func (s *Server) createPullRequest(ctx context.Context, req *mcp.CallToolRequest, in PullRequestArgs) (*mcp.CallToolResult, Result, error) {
	if err := ValidatePullRequest(in); err != nil {
		return nil, Result{}, err
	}
	canonical, err := canonicalPullRequest(in)
	if err != nil {
		return nil, Result{}, err
	}
	return s.record(ctx, req, ToolCreatePullRequest, canonical, AssessRisk(in))
}

func (s *Server) commentOnIssue(ctx context.Context, req *mcp.CallToolRequest, in CommentArgs) (*mcp.CallToolResult, Result, error) {
	if err := ValidateComment(in); err != nil {
		return nil, Result{}, err
	}
	canonical, err := Canonical(in)
	if err != nil {
		return nil, Result{}, err
	}
	return s.record(ctx, req, ToolCommentOnIssue, canonical, struct{}{})
}

// HashArgs is the args_sha256 of canonical arguments: hex SHA-256 of the
// exact bytes stored in approval_requests.args_json.
func HashArgs(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// record stores the call as an approval request, or returns the existing one
// for the same call in the same run.
func (s *Server) record(ctx context.Context, req *mcp.CallToolRequest, tool string, canonical []byte, risk any) (*mcp.CallToolResult, Result, error) {
	c, err := s.identify(ctx, req)
	if err != nil {
		return nil, Result{}, err
	}
	decision, repo, err := s.authorise(c, tool)
	if err != nil {
		return nil, Result{}, err
	}
	riskJSON, err := json.Marshal(risk)
	if err != nil {
		return nil, Result{}, err
	}
	params := store.CreateApprovalRequestParams{
		OrgID: c.org.ID, TicketID: c.run.TicketID, RunID: c.run.ID, AgentID: c.agent.ID,
		Connector: connectors.GitHubID, Tool: tool, Target: connectors.GitHubID + ":" + repo,
		ArgsJson: string(canonical), ArgsSha256: HashArgs(canonical), Risk: riskJSON,
		Decision: store.ApprovalDecisionAsk, Status: store.ApprovalStatusPending,
		ExpiresAt: pgtype.Timestamptz{Time: s.now().Add(s.ttl), Valid: true},
	}
	if decision == connectors.Allow {
		// The Trusted preset: no human decision; Koinon executes it (P1).
		params.Decision, params.Status = store.ApprovalDecisionAllow, store.ApprovalStatusApproved
	}
	var row store.ApprovalRequest
	created := true
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
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
		payload, err := json.Marshal(map[string]any{
			"approval_id": row.ID.String(), "tool": tool, "target": row.Target, "args_sha256": row.ArgsSha256,
			"decision": string(row.Decision), "risk": json.RawMessage(riskJSON),
		})
		if err != nil {
			return err
		}
		_, err = q.AppendTicketEvent(ctx, store.AppendTicketEventParams{
			OrgID: row.OrgID, TicketID: row.TicketID, Kind: KindApprovalRequested, ActorKind: store.TicketActorKindAgent,
			ActorAgentID: row.AgentID, RunID: row.RunID, Payload: payload,
		})
		return err
	})
	if err != nil {
		s.log.Error("github tools: record approval request", append(c.attrs(), "tool", tool, "err", err)...)
		return nil, Result{}, errors.New("internal error recording the request; try again later")
	}
	// Arguments are not logged: they can be large and are in the database.
	s.log.Info("approval requested", append(c.attrs(), "id", row.ID.String(), "tool", tool, "target", row.Target,
		"args_sha256", row.ArgsSha256, "decision", string(row.Decision), "status", string(row.Status), "new", created)...)
	if created && row.Status == store.ApprovalStatusApproved && s.exec != nil {
		go s.exec(context.WithoutCancel(ctx), row.OrgID, row.ID)
	}
	out := resultOf(row)
	return textResult(statusLine(out)), out, nil
}

func (s *Server) getApprovalStatus(ctx context.Context, req *mcp.CallToolRequest, in StatusArgs) (*mcp.CallToolResult, Result, error) {
	c, err := s.identify(ctx, req)
	if err != nil {
		return nil, Result{}, err
	}
	var id pgtype.UUID
	if err := id.Scan(strings.TrimSpace(in.ID)); err != nil || !id.Valid {
		return nil, Result{}, fmt.Errorf("id %q is not an approval request id", in.ID)
	}
	row, err := s.q.GetApprovalRequestForAgent(ctx, store.GetApprovalRequestForAgentParams{OrgID: c.org.ID, ID: id, AgentID: c.agent.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, Result{}, fmt.Errorf("no approval request %s for this agent", in.ID)
	}
	if err != nil {
		return nil, Result{}, s.lookupErr(err, "approval request")
	}
	out := resultOf(row)
	return textResult(statusLine(out)), out, nil
}

func resultOf(row store.ApprovalRequest) Result {
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
	return out
}

func statusLine(r Result) string {
	switch store.ApprovalStatus(r.Status) {
	case store.ApprovalStatusPending:
		return fmt.Sprintf("approval requested: id=%s status=pending args_sha256=%s. A human decides on the ticket; "+
			"your run pauses now and resumes with the result. Do not retry or work around this call.", r.ID, r.ArgsSHA256)
	case store.ApprovalStatusApproved:
		return fmt.Sprintf("accepted: id=%s status=approved args_sha256=%s. Koinon executes it and your run resumes with the result.", r.ID, r.ArgsSHA256)
	case store.ApprovalStatusDenied:
		return fmt.Sprintf("id=%s status=denied note=%q. The human denied this call; do not retry it.", r.ID, r.Note)
	case store.ApprovalStatusFailed:
		return fmt.Sprintf("id=%s status=failed error=%q", r.ID, r.Error)
	case store.ApprovalStatusExecuted:
		res, _ := json.Marshal(r.Result)
		if url, _ := r.Result["url"].(string); url != "" {
			return fmt.Sprintf("id=%s status=executed url=%s result=%s", r.ID, url, res)
		}
		return fmt.Sprintf("id=%s status=executed result=%s", r.ID, res)
	default:
		res, _ := json.Marshal(r.Result)
		return fmt.Sprintf("id=%s status=%s result=%s", r.ID, r.Status, res)
	}
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}
