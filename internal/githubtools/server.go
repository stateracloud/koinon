// SPDX-License-Identifier: Apache-2.0

// Package githubtools is the MCP server for the GitHub connector's ask-tier
// tools (plan item C3; ADR 0008, 0042, 0049), served behind agentgateway:
//
//   - create_pull_request (scope pull_request.create): branch, title, body, files;
//   - comment_on_issue (scope issue.comment): issue number and body;
//   - get_approval_status: the state of one of the agent's requests.
//
// A write call is validated, then recorded exactly as an approval request
// (canonical arguments, their SHA-256, tool, target, agent, run and task)
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
//
// # Readiness prompt
//
// The server also publishes one MCP prompt, gateway.ReadyPrompt, to every
// caller. OpenCode turns MCP prompts into commands and announces them over
// ACP once the server is connected, which is how koinon-agentd knows the
// tools are there before it sends the first prompt (ADR 0052).
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

	"github.com/stateracloud/koinon/connectors"
	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/approvalscope"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/project"
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
	// Hosts are the code-host providers a grant can be on; defaults to the shipped providers.
	Hosts *codehost.Registry
	// Execute, if set, is called in its own goroutine for a newly recorded call the grant's
	// preset allows without a human (Trusted), so it runs at once; the approval sweeper
	// executes it otherwise.
	Execute func(ctx context.Context, org, id pgtype.UUID)
	// Keys opens the connection's secrets so create_pull_request can check, before it asks for
	// approval, that the branch is free. Nil skips the check; the executor still makes it.
	Keys connections.Opener
}

// KindApprovalRequested is the task event written when a call is recorded.
const KindApprovalRequested = "approval.requested"

// Server is the GitHub tools MCP server.
type Server struct {
	pool  *pgxpool.Pool
	q     *store.Queries
	log   *slog.Logger
	ttl   time.Duration
	now   func() time.Time
	hosts *codehost.Registry
	mcp   *mcp.Server
	exec  func(ctx context.Context, org, id pgtype.UUID)
	keys  connections.Opener
}

// New returns a Server recording requests in pool.
func New(pool *pgxpool.Pool, opts Options) (*Server, error) {
	if opts.Hosts == nil {
		opts.Hosts = providers.Default()
	}
	for _, id := range opts.Hosts.IDs() {
		if _, ok := connectors.Get(id); !ok {
			return nil, fmt.Errorf("githubtools: the %s connector type is not embedded", id)
		}
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
	s := &Server{pool: pool, q: store.New(pool), log: opts.Logger, ttl: opts.ApprovalTTL, now: opts.Now, hosts: opts.Hosts, exec: opts.Execute, keys: opts.Keys}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: project.Name + "-github-tools", Version: project.Version}, nil)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolCreatePullRequest,
		Description: "Propose a pull request on the task's repository. Needs human approval: returns " +
			"\"approval requested: id=...\" at once and your run pauses; once approved, Koinon creates the branch, " +
			"the commit and the pull request exactly as given, and you get the result when the run resumes. " +
			"Koinon owns the branch namespace: give branch as a short name such as fix-readme-typo (a leading koinon/ is dropped) and the branch is created as koinon/<your agent>/<name>; " +
			"the approval shows that final name. A branch that already exists on the code host is refused: choose another name. " +
			"A file marked executable is written as 100755; otherwise a new file is regular (100644) and a modified file keeps its mode. " +
			"Symbolic links and submodules cannot be written. " +
			"Refused: changes to the code host's CI paths (for GitHub .github/workflows/, and any the connection lists), more than 100 files or 1 MiB, unsafe paths.",
	}, s.createPullRequest)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolCommentOnIssue,
		Description: "Propose a comment on an issue or pull request of the task's repository. Needs human " +
			"approval: returns \"approval requested: id=...\" at once; Koinon posts it once approved.",
	}, s.commentOnIssue)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        ToolGetApprovalStatus,
		Description: "The status of one of your approval requests and, once executed, its result (for example the pull request URL).",
	}, s.getApprovalStatus)
	s.mcp.AddPrompt(&mcp.Prompt{
		Name:        gateway.ReadyPrompt,
		Description: "Koinon's tools are connected. Koinon uses this to know the session is ready; there is nothing to run.",
	}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{
			Role: "user", Content: &mcp.TextContent{Text: "Koinon's tools are connected."},
		}}}, nil
	})
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
	return []any{"org", c.org.Slug, "agent", c.agent.ID.String(), "run", c.run.ID.String(), "task", c.run.TaskID.String()}
}

// identify resolves the gateway-set agent header to the agent's running run.
func (s *Server) identify(ctx context.Context, req *mcp.CallToolRequest) (caller, error) {
	var h http.Header
	if req != nil && req.Extra != nil {
		h = req.Extra.Header
	}
	ref, err := gateway.AgentFromHeader(h)
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
			return caller{}, errors.New("this agent has no running run; tools are only available while working on a task")
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
func (s *Server) authorise(c caller, tool string) (connectors.Decision, string, string, error) {
	connector, grant, ok := s.hosts.Grant(c.config)
	if !ok {
		return connectors.Deny, "", "", errors.New("this agent has no GitHub grant")
	}
	ct, _ := connectors.Get(connector) // checked in New
	wt, err := LookupWriteTool(tool)
	if err != nil {
		return connectors.Deny, "", "", err
	}
	scope := wt.Scope
	granted := false
	for _, sc := range grant.Scopes {
		granted = granted || sc == scope
	}
	d := ct.Decide(grant.Preset, scope)
	if !granted || d == connectors.Deny {
		return connectors.Deny, "", "", fmt.Errorf("this agent's GitHub grant does not allow %s (scope %s)", tool, scope)
	}
	b, ok := c.config.Binding(connector)
	switch {
	case !ok || len(b.Targets) == 0:
		return connectors.Deny, "", "", errors.New("this agent's GitHub grant has no repository yet; ask an admin to choose one")
	case len(b.Targets) > 1:
		return connectors.Deny, "", "", errors.New("this agent's GitHub grant has several repositories; v0.1 supports one per agent")
	case !ct.ValidTarget(b.Targets[0]):
		return connectors.Deny, "", "", fmt.Errorf("the granted repository %q is not owner/name", b.Targets[0])
	}
	return d, b.Targets[0], connector, nil
}

func (s *Server) createPullRequest(ctx context.Context, req *mcp.CallToolRequest, in PullRequestArgs) (*mcp.CallToolResult, Result, error) {
	c, err := s.identify(ctx, req)
	if err != nil {
		return nil, Result{}, err
	}
	// Koinon owns the branch namespace: the recorded (and approved) branch is the final name,
	// so agents of one template, which suggest the same name, never collide (ADR 0062).
	branch, err := FinalBranch(AgentNamespace(c.agent.DisplayName, c.agent.ID.String()), in.Branch)
	if err != nil {
		return nil, Result{}, err
	}
	in.Branch = branch
	guard, err := s.guardFor(ctx, req)
	if err != nil {
		return nil, Result{}, err
	}
	if err := ValidatePullRequest(in, guard); err != nil {
		return nil, Result{}, err
	}
	canonical, err := canonicalPullRequest(in)
	if err != nil {
		return nil, Result{}, err
	}
	if err := s.checkBranchFree(ctx, c, in.Branch, HashArgs(canonical)); err != nil {
		return nil, Result{}, err
	}
	return s.record(ctx, req, ToolCreatePullRequest, canonical, AssessRisk(in))
}

// checkBranchFree refuses, with an error the agent can act on, a branch that already exists on
// the code host, so no human approves a call that cannot run. The executor checks again (a
// branch can appear after approval, and only it knows the branch is not its own). A host that
// cannot be asked does not block the call: the executor's check stands. The same call made
// again in the run (a retry, or a repeat once the approval created the branch) is not refused:
// record answers it with the request already recorded.
func (s *Server) checkBranchFree(ctx context.Context, c caller, branch, argsSHA256 string) error {
	if s.keys == nil {
		return nil
	}
	skip := func(why string, err error) error {
		s.log.Warn("github tools: branch check skipped", append(c.attrs(), "why", why, "err", err)...)
		return nil
	}
	connector, _, ok := s.hosts.Grant(c.config)
	if !ok {
		return nil // record refuses a call without a grant
	}
	b, ok := c.config.Binding(connector)
	if !ok || len(b.Targets) != 1 {
		return nil // record refuses it
	}
	p, ok := s.hosts.Get(connector)
	if !ok {
		return nil
	}
	repo, err := p.ParseRepo(b.Targets[0])
	if err != nil {
		return nil //nolint:nilerr // record refuses it
	}
	var id pgtype.UUID
	if id.Scan(b.ConnectionID) != nil || !id.Valid {
		return nil
	}
	row, err := s.q.GetConnection(ctx, store.GetConnectionParams{OrgID: c.org.ID, ID: id})
	if err != nil {
		return skip("load the connection", err)
	}
	conn, err := connections.Load(ctx, s.keys, row)
	if err != nil {
		return skip("open the connection", err)
	}
	acc, err := p.Open(ctx, conn, codehost.Scope{
		Repositories: []string{repo.String()},
		Permissions:  map[string]string{"metadata": "read", "contents": "read"},
	})
	if err != nil {
		return skip("open the code host", err)
	}
	_, found, err := acc.Branch(ctx, repo, branch)
	if err != nil {
		return skip("look the branch up", err)
	}
	if found {
		_, err := s.q.GetApprovalRequestByCall(ctx, store.GetApprovalRequestByCallParams{
			OrgID: c.org.ID, RunID: c.run.ID, Tool: ToolCreatePullRequest, Target: connector + ":" + b.Targets[0], ArgsSha256: argsSHA256,
		})
		switch {
		case err == nil:
			return nil // this very call, recorded before; record returns it
		case !errors.Is(err, pgx.ErrNoRows):
			return skip("look the request up", err)
		}
		return refuse([]string{fmt.Sprintf("branch %s already exists in %s and was not created for this request; choose another branch name", branch, repo)})
	}
	return nil
}

// guardFor builds the workflow guard of the connection the caller's grant uses: the provider's
// CI paths plus the directories configured on the connection. A grant whose connection cannot be
// read gets the provider's paths alone; the executor needs the connection and fails without it.
func (s *Server) guardFor(ctx context.Context, req *mcp.CallToolRequest) (codehost.Guard, error) {
	c, err := s.identify(ctx, req)
	if err != nil {
		return codehost.Guard{}, err
	}
	connector, _, ok := s.hosts.Grant(c.config)
	if !ok {
		return codehost.Guard{}, nil // record refuses a call without a grant
	}
	b, ok := c.config.Binding(connector)
	if !ok {
		return codehost.Guard{}, nil // record refuses a grant without a repository
	}
	p, ok := s.hosts.Get(connector)
	if !ok {
		return codehost.Guard{}, fmt.Errorf("no code host %q", connector)
	}
	var config []byte
	var id pgtype.UUID
	if id.Scan(b.ConnectionID) == nil && id.Valid {
		row, err := s.q.GetConnection(ctx, store.GetConnectionParams{OrgID: c.org.ID, ID: id})
		switch {
		case err == nil:
			config = row.Config
		case !errors.Is(err, pgx.ErrNoRows):
			s.log.Error("github tools: load the connection", append(c.attrs(), "err", err)...)
			return codehost.Guard{}, errors.New("internal error; try again later")
		}
	}
	g, err := codehost.GuardOf(p.Capabilities(), config)
	if err != nil {
		s.log.Error("github tools: build the workflow guard", append(c.attrs(), "err", err)...)
		return codehost.Guard{}, errors.New("this agent's connection has invalid CI directories; ask an admin to fix the connection")
	}
	return g, nil
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

// scopeable says whether a scoped approval may cover a call with this risk. Coverage is decided
// per risk type: the empty risk is covered, a Risk only when it cannot push and run code, and any
// other type (a pointer, a new risk type) is not covered until it is added here.
func scopeable(risk any) bool {
	switch r := risk.(type) {
	case struct{}:
		return true
	case Risk:
		return !r.PushAndRun()
	default:
		return false
	}
}

// callBinding is what a code-host approval request is bound to: the connection the grant uses and
// its base URL. A scope created from the request carries it, and covers only calls with the same
// binding, so a scope granted on github.com does not cover the same owner/repo on a GHES
// connection after the agent is rebound or the connection replaced (ADR 0065 AP1).
func (s *Server) callBinding(ctx context.Context, c caller, connector string) ([]byte, error) {
	b, ok := c.config.Binding(connector)
	if !ok {
		return nil, errors.New("this agent's GitHub grant has no repository yet; ask an admin to choose one")
	}
	bind := struct {
		ConnectionID string `json:"connectionId"`
		BaseURL      string `json:"baseUrl,omitempty"`
	}{ConnectionID: b.ConnectionID}
	var id pgtype.UUID
	if id.Scan(b.ConnectionID) == nil && id.Valid {
		row, err := s.q.GetConnection(ctx, store.GetConnectionParams{OrgID: c.org.ID, ID: id})
		switch {
		case err == nil:
			site, err := codehost.SiteOf(row.Config)
			if err != nil {
				return nil, errors.New("this agent's connection has an invalid configuration; ask an admin to fix the connection")
			}
			bind.BaseURL = site.BaseURL
		case !errors.Is(err, pgx.ErrNoRows):
			s.log.Error("github tools: load the connection", append(c.attrs(), "err", err)...)
			return nil, errors.New("internal error; try again later")
		}
	}
	return json.Marshal(bind)
}

// record stores the call as an approval request, or returns the existing one
// for the same call in the same run.
func (s *Server) record(ctx context.Context, req *mcp.CallToolRequest, tool string, canonical []byte, risk any) (*mcp.CallToolResult, Result, error) {
	c, err := s.identify(ctx, req)
	if err != nil {
		return nil, Result{}, err
	}
	decision, repo, connector, err := s.authorise(c, tool)
	if err != nil {
		return nil, Result{}, err
	}
	riskJSON, err := json.Marshal(risk)
	if err != nil {
		return nil, Result{}, err
	}
	binding, err := s.callBinding(ctx, c, connector)
	if err != nil {
		return nil, Result{}, err
	}
	params := store.CreateApprovalRequestParams{
		CallBinding: binding,
		OrgID:       c.org.ID, TaskID: c.run.TaskID, RunID: c.run.ID, AgentID: c.agent.ID,
		Connector: connector, Tool: tool, Target: connector + ":" + repo,
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
		// A scoped approval of this agent (or task) for exactly this connector, tool and target
		// lets the call run without parking; the timeline records the scope. Calls that could
		// push and run code are never covered.
		var scope store.ApprovalScope
		if params.Status == store.ApprovalStatusPending {
			if scopeable(risk) {
				sc, ok, err := approvalscope.Match(ctx, q, params.OrgID, params.AgentID, params.TaskID, connector, tool, params.Target, binding, s.now())
				if err != nil {
					return err
				}
				if ok {
					scope = sc
					params.Decision, params.Status, params.ScopeID = store.ApprovalDecisionAllow, store.ApprovalStatusApproved, sc.ID
				}
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
			"decision": string(row.Decision), "risk": json.RawMessage(riskJSON),
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
			OrgID: row.OrgID, TaskID: row.TaskID, Kind: KindApprovalRequested, ActorKind: store.TaskActorKindAgent,
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
		return fmt.Sprintf("approval requested: id=%s status=pending args_sha256=%s. A human decides on the task; "+
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
