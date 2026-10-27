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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/agentconfig"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/project"
	"github.com/stateracloud/koinon/internal/questions"
	"github.com/stateracloud/koinon/internal/scheduler"
)

// Target is the gateway target (and tool server key) of this server.
const Target = gateway.KoinonTarget

// Path is where the tools listener serves it.
const Path = "/mcp-koinon"

// ToolAskUser is the tool's name.
const ToolAskUser = gateway.ToolAskUser

// ToolCancelTask is the name of the cancel_task tool.
const ToolCancelTask = gateway.ToolCancelTask

// Options configures a Server.
type Options struct {
	Logger *slog.Logger
	// TTL and Reminder override, when positive, the agent's question timers (the active version's
	// questions settings, 7 days and 24 hours by default; ADR 0065 decision 10). For tests.
	TTL, Reminder time.Duration
	Now           func() time.Time
}

// Server is the ask_user MCP server.
type Server struct {
	pool          *pgxpool.Pool
	q             *store.Queries
	log           *slog.Logger
	ttl, reminder time.Duration
	now           func() time.Time
	mcp           *mcp.Server
}

// New returns a Server recording questions in pool.
func New(pool *pgxpool.Pool, opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{pool: pool, q: store.New(pool), log: opts.Logger, ttl: opts.TTL, reminder: opts.Reminder, now: opts.Now}
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
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: ToolCancelTask,
		Description: "Close the task you are working on as canceled, when it cannot or should not go on (for example nobody " +
			"answered your question and the safe default is not enough to continue). 'reason' is shown on the task's " +
			"timeline. It cannot be undone; the person can start a new task. After calling it, end your turn.",
	}, s.cancelTask)
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

// timers returns the reminder and the expiry of the agent's questions: the active version's
// settings, or the overrides in Options.
func (s *Server) timers(ctx context.Context, a store.Agent) (reminder, ttl time.Duration, err error) {
	var cfg agentconfig.Config
	if a.ActiveVersionID.Valid {
		v, err := s.q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: a.OrgID, ID: a.ActiveVersionID})
		if err != nil {
			return 0, 0, s.lookupErr(err, "agent version")
		}
		if cfg, err = agentconfig.Parse(v.Config); err != nil {
			s.log.Error("ask_user: read the agent's config", "agent", a.ID.String(), "err", err)
			return 0, 0, errors.New("internal error; try again later")
		}
	}
	reminder, ttl = cfg.QuestionTimers()
	if s.ttl > 0 {
		ttl = s.ttl
		if reminder >= ttl {
			reminder = 0
		}
	}
	if s.reminder > 0 && s.reminder < ttl {
		reminder = s.reminder
	}
	return reminder, ttl, nil
}

func remindAt(now time.Time, after time.Duration) time.Time {
	if after <= 0 {
		return time.Time{}
	}
	return now.Add(after)
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
	if m.Role == store.OrgRoleViewer {
		return pgtype.UUID{}, "", fmt.Errorf("%s cannot answer questions (viewer): name a member or admin", to)
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
	remind, ttl, err := s.timers(ctx, c.agent)
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
			ExpiresAt: s.now().Add(ttl), RemindAt: remindAt(s.now(), remind),
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

// CancelArgs are the arguments of cancel_task.
type CancelArgs struct {
	Reason string `json:"reason" jsonschema:"why the task is canceled, in plain words; shown on the task's timeline"`
}

// CancelResult is the structured result of cancel_task.
type CancelResult struct {
	Status string `json:"status"`
}

// kindStatusChanged is the task event of a state change (the same kind the task service writes).
const kindStatusChanged = "status.changed"

// cancelTask closes the caller's task as canceled, with the reason on the task's timeline, and
// cancels its queued runs. The caller's own run is running and ends with the turn.
func (s *Server) cancelTask(ctx context.Context, req *mcp.CallToolRequest, in CancelArgs) (*mcp.CallToolResult, CancelResult, error) {
	c, err := s.identify(ctx, req)
	if err != nil {
		return nil, CancelResult{}, err
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return nil, CancelResult{}, errors.New("reason is required: say why the task is canceled")
	}
	if n := utf8.RuneCountInString(reason); n > 1000 {
		return nil, CancelResult{}, fmt.Errorf("reason is %d characters; at most 1000", n)
	}
	status := "canceled"
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		t, err := q.GetTaskForUpdate(ctx, store.GetTaskForUpdateParams{OrgID: c.org.ID, ID: c.run.TaskID})
		if err != nil {
			return err
		}
		switch t.State {
		case store.TaskStateCompleted, store.TaskStateCanceled, store.TaskStateFailed:
			status = string(t.State)
			return nil
		}
		if _, err := q.SetTaskState(ctx, store.SetTaskStateParams{OrgID: c.org.ID, ID: t.ID, State: store.TaskStateCanceled}); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"from": string(t.State), "to": string(store.TaskStateCanceled), "reason": reason})
		if _, err := q.AppendTaskEvent(ctx, store.AppendTaskEventParams{
			OrgID: c.org.ID, TaskID: t.ID, RunID: c.run.ID, Kind: kindStatusChanged,
			ActorKind: store.TaskActorKindAgent, ActorAgentID: c.agent.ID, Payload: payload,
		}); err != nil {
			return err
		}
		_, err = scheduler.CancelQueuedForTask(ctx, q, t, "task canceled by the agent", scheduler.System)
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, CancelResult{}, errNoIdentity
		}
		s.log.Error("cancel_task: cancel the task", "org", c.org.Slug, "agent", c.agent.ID.String(), "run", c.run.ID.String(), "err", err)
		return nil, CancelResult{}, errors.New("internal error canceling the task; try again later")
	}
	s.log.Info("task canceled by its agent", "org", c.org.Slug, "agent", c.agent.ID.String(), "task", c.run.TaskID.String())
	text := "The task is " + status + ". End your turn now."
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, CancelResult{Status: status}, nil
}
