// SPDX-License-Identifier: Apache-2.0

// Package askuser is the MCP server for Koinon's own agent tool ask_user (plan item TK3; ADR 0065
// decisions 8 and 10a), served behind agentgateway as the "koinon" target, so an agent sees it as
// koinon_ask_user.
//
// ask_user(to, question, draft?, choices?, default) records a question for a person, answers the
// agent at once ("question recorded") and has no other effect: when the agent's turn ends, agentd
// opens the question and parks the run (reason input_required), moves the task to input-required
// and puts the question in the person's inbox, in one transaction (package questions). The answer
// reaches the agent as the next prompt of the resumed run. Every question names the safe default
// that is used when it is dismissed or expires, and the agent is told when that happened.
//
// # Identity
//
// As for the GitHub tools (package githubtools): agentgateway sets gateway.AgentHeader to the
// authenticated key's agent, and this server trusts that header and nothing else. koinon-api serves
// it on the tools listener, which the chart's NetworkPolicy opens to the gateway only.
package askuser

import (
	"context"
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

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/questions"
)

// Target is the gateway target (and tool server key) of this server.
const Target = gateway.KoinonTarget

// Path is where the tools listener serves it.
const Path = "/mcp-koinon"

// ToolAskUser is the tool's name.
const ToolAskUser = gateway.ToolAskUser

// Options configures a Server.
type Options struct {
	Logger *slog.Logger
	// TTL is how long a question waits before it expires; defaults to questions.DefaultTTL.
	TTL time.Duration
	Now func() time.Time
}

// Server is the ask_user MCP server.
type Server struct {
	pool *pgxpool.Pool
	q    *store.Queries
	log  *slog.Logger
	ttl  time.Duration
	now  func() time.Time
	mcp  *mcp.Server
}

// New returns a Server recording questions in pool.
func New(pool *pgxpool.Pool, opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.TTL <= 0 {
		opts.TTL = questions.DefaultTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{pool: pool, q: store.New(pool), log: opts.Logger, ttl: opts.TTL, now: opts.Now}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: project.Name + "-agent-tools", Version: project.Version}, nil)
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolAskUser,
		Description: "Ask a person a question and wait for the answer. Your run pauses when your turn ends and resumes " +
			"with the answer as the next message, possibly much later (the sandbox may have been stopped meanwhile). " +
			"'to' is the email or user ID of a member of the org; leave it out to ask the person who created the task. " +
			"'choices' lists answers the person can pick (at most 10); 'draft' is text you propose (a reply to post, a " +
			"title) that they can accept or edit. 'default' is required: the safe answer Koinon uses if the question is " +
			"dismissed or nobody answers within 7 days; if you give choices it must be one of them. Choose a default that " +
			"is safe to act on without a person (for example 'do not post', 'skip'). You are told when the default " +
			"was applied. Do not ask twice, and do not work around the pause.",
	}, s.askUser)
	return s
}

// Handler serves MCP over Streamable HTTP. Mount it on a listener only agentgateway can reach.
func (s *Server) Handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
}

// Args are the tool's arguments.
type Args struct {
	To       string   `json:"to,omitempty" jsonschema:"email or user ID of an org member; omit to ask the task's creator"`
	Question string   `json:"question" jsonschema:"the question, in plain words"`
	Draft    string   `json:"draft,omitempty" jsonschema:"text you propose, which the person can accept or edit"`
	Choices  []string `json:"choices,omitempty" jsonschema:"answers to offer as buttons, at most 10"`
	Default  string   `json:"default" jsonschema:"the safe answer used if the question is dismissed or expires; one of choices when given"`
}

// Result is the structured result of the tool.
type Result struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	To        string `json:"to"`
	Default   string `json:"default"`
	ExpiresAt string `json:"expires_at"`
}

var errNoIdentity = errors.New("this tool server answers only calls routed through Koinon's agentgateway, which identifies the agent")

type caller struct {
	org   store.Org
	agent store.Agent
	run   store.Run
}

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
			return caller{}, errors.New("this agent has no running run; tools are only available while working on a task")
		}
		return caller{}, s.lookupErr(err, "run")
	}
	return caller{org: org, agent: agent, run: run}, nil
}

func (s *Server) lookupErr(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoIdentity
	}
	s.log.Error("ask_user: lookup failed", "what", what, "err", err)
	return errors.New("internal error; try again later")
}

// addressee resolves "to": a member named by email or ID, or the task's creator.
func (s *Server) addressee(ctx context.Context, c caller, to string) (pgtype.UUID, string, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		task, err := s.q.GetTask(ctx, store.GetTaskParams{OrgID: c.org.ID, ID: c.run.TaskID})
		if err != nil {
			return pgtype.UUID{}, "", s.lookupErr(err, "task")
		}
		if task.CreatedByKind != store.TaskCreatorKindUser || !task.CreatedByUserID.Valid {
			return pgtype.UUID{}, "", errors.New("this task was not created by a person, so 'to' is required: name an org member by email or user ID")
		}
		return task.CreatedByUserID, "the task's creator", nil
	}
	m, err := s.q.FindOrgMemberByIdent(ctx, store.FindOrgMemberByIdentParams{OrgID: c.org.ID, Ident: to})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, "", fmt.Errorf("%q is not a member of this org: 'to' takes a member's email or user ID", to)
	}
	if err != nil {
		return pgtype.UUID{}, "", s.lookupErr(err, "member")
	}
	return m.ID, to, nil
}

func (s *Server) askUser(ctx context.Context, req *mcp.CallToolRequest, in Args) (*mcp.CallToolResult, Result, error) {
	c, err := s.identify(ctx, req)
	if err != nil {
		return nil, Result{}, err
	}
	to, label, err := s.addressee(ctx, c, in.To)
	if err != nil {
		return nil, Result{}, err
	}
	var row store.Question
	var created bool
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		row, created, err = questions.Create(ctx, store.New(tx), questions.Ask{
			Org: c.org.ID, Task: c.run.TaskID, Run: c.run.ID, Agent: c.agent.ID, To: to,
			Question: in.Question, Choices: in.Choices, Draft: in.Draft, Default: in.Default,
			ExpiresAt: s.now().Add(s.ttl),
		})
		return err
	})
	switch {
	case errors.Is(err, questions.ErrInvalid), errors.Is(err, questions.ErrNotMember):
		return nil, Result{}, err
	case err != nil:
		s.log.Error("ask_user: record the question", "org", c.org.Slug, "agent", c.agent.ID.String(), "run", c.run.ID.String(), "err", err)
		return nil, Result{}, errors.New("internal error recording the question; try again later")
	}
	s.log.Info("question asked", "org", c.org.Slug, "agent", c.agent.ID.String(), "run", c.run.ID.String(),
		"task", c.run.TaskID.String(), "question", row.ID.String(), "new", created)
	out := Result{
		ID: row.ID.String(), Status: string(row.State), To: label, Default: row.DefaultAnswer,
		ExpiresAt: row.ExpiresAt.Time.UTC().Format(time.RFC3339),
	}
	text := fmt.Sprintf("question recorded: id=%s to=%s. End your turn now: Koinon pauses your run until the answer arrives, "+
		"and you get it as your next message. If the question is dismissed or not answered by %s, Koinon applies your default (%q) "+
		"and tells you. Do not ask again and do not work around the pause.", out.ID, label, out.ExpiresAt, row.DefaultAnswer)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
}
