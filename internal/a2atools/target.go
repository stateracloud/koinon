// SPDX-License-Identifier: Apache-2.0

package a2atools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stateracloud/koinon/internal/a2aout"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// The texts a model can see for a failure. They are fixed: no URL, no card text and no body of a remote
// error is in any of them (ADR 0123 decisions 10 and 11).
const (
	textNoAgent      = "refused: no remote agent of this name is available to this agent"
	textBadHandle    = "refused: the task_id or the context_id is not valid for this agent and this remote agent"
	textBadArgs      = "refused: the arguments are not valid"
	textCardChanged  = "refused: the agent card of the remote agent changed. An admin must read and accept the new card"
	textEgress       = "refused: the egress policy denies this remote agent"
	textEnded        = "refused: the task has ended and takes no new message. To continue, pass the context_id and the task_id of the ended task"
	textNotAwaiting  = "refused: the task does not wait for input. Read it with get_task"
	textNoTask       = "the remote agent has no such task"
	textNotCancelled = "the remote agent cannot cancel this task"
	textUnanswered   = "the remote agent did not answer"
	textNoKey        = "the target is not ready"
)

// Options configure a [Target].
type Options struct {
	// Policy is the platform URL policy for every dial (ADR 0120).
	Policy catalog.URLPolicy
	// Credentials resolves the credential of a remote agent. Nil is [mcpgateway.StaticCredentials].
	Credentials mcpgateway.CredentialSource
	// Transport replaces the policy dialer. It is for tests only.
	Transport http.RoundTripper
	// RequestTimeout bounds one request to a remote agent. Zero is the a2aout default.
	RequestTimeout time.Duration
	// Wait is how long send_task waits for a task to settle before it returns the state it has. Zero is
	// 30 seconds. A later get_task reads the rest.
	Wait time.Duration
	// PollInterval is the time between two reads while send_task waits. Zero is the a2aout default.
	PollInterval time.Duration
}

// Target is the built-in target of kind [gateway.BuiltinA2A]. It is safe for concurrent use.
type Target struct{ o Options }

var _ mcpgateway.CredentialBuiltin = (*Target)(nil)

// New returns the target.
func New(o Options) *Target {
	if o.Wait <= 0 {
		o.Wait = 30 * time.Second
	}
	if o.Credentials == nil {
		o.Credentials = mcpgateway.StaticCredentials{}
	}
	return &Target{o: o}
}

// WrapCredentials returns a copy of t whose credential source is wrap(source). The gateway uses it to record
// every credential the target opens in its credential scrub (#1174).
func (t *Target) WrapCredentials(wrap func(mcpgateway.CredentialSource) mcpgateway.CredentialSource) mcpgateway.Builtin {
	o := t.o
	o.Credentials = wrap(o.Credentials)
	return &Target{o: o}
}

// args is the union of the arguments of the three tools. A field that a tool does not take is an error.
type args struct {
	Agent     string `json:"agent"`
	Message   string `json:"message"`
	ContextID string `json:"context_id"`
	TaskID    string `json:"task_id"`
}

// reply is what a tool returns, as JSON text. The question and the reply are the text of another party.
type reply struct {
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	ContextID string `json:"context_id,omitempty"`
	Question  string `json:"question,omitempty"`
	Reply     string `json:"reply,omitempty"`
}

func fail(text string) mcpgateway.BuiltinResult {
	return mcpgateway.BuiltinResult{Result: &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}}
}

// Call implements [mcpgateway.Builtin].
func (t *Target) Call(ctx context.Context, tgt gateway.MCPTarget, agent gateway.AgentRef, tool string, raw json.RawMessage) mcpgateway.BuiltinResult {
	var a args
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return fail(textBadArgs)
	}
	switch tool {
	case ToolSendTask:
		if strings.TrimSpace(a.Message) == "" || len(a.Message) > a2aout.MaxText {
			return fail(textBadArgs)
		}
	case ToolGetTask, ToolCancelTask:
		if a.TaskID == "" || a.Message != "" || a.ContextID != "" {
			return fail(textBadArgs)
		}
	default:
		return fail("refused: no such tool")
	}
	// The same refusal for each reason (no grant, another org, a remote agent that is disabled or
	// unhealthy and so not in the list): it tells a caller nothing about the other orgs.
	var ra *gateway.MCPRemoteAgent
	if agent.Org == tgt.Org {
		for i := range tgt.RemoteAgents {
			if tgt.RemoteAgents[i].Name == a.Agent && slices.Contains(tgt.RemoteAgents[i].Agents, agent.Name) {
				ra = &tgt.RemoteAgents[i]
			}
		}
	}
	if ra == nil {
		return fail(textNoAgent)
	}
	h, err := readKey(tgt.HandleKeyFile)
	if err != nil {
		return fail(textNoKey)
	}
	in := call{t: t, h: h, org: tgt.Org, agent: agent, ra: ra, a: a}
	cred := mcpgateway.CredentialNone
	if ra.CredentialHeader != "" {
		cred = mcpgateway.CredentialStatic
	}
	var res mcpgateway.BuiltinResult
	switch tool {
	case ToolSendTask:
		res = in.send(ctx)
	case ToolGetTask:
		res = in.get(ctx)
	case ToolCancelTask:
		res = in.cancel(ctx)
	}
	res.Credential = cred
	return res
}

// call is one tool call, with what the handles need.
type call struct {
	t     *Target
	h     handles
	org   string
	agent gateway.AgentRef
	ra    *gateway.MCPRemoteAgent
	a     args
}

func (c *call) client(ctx context.Context) (*a2aout.Client, *mcpgateway.BuiltinResult) {
	cl, err := a2aout.New(ctx, a2aout.Options{
		Target: c.ra.Target(c.org), Agent: c.agent, Credentials: c.t.o.Credentials, Policy: c.t.o.Policy,
		Transport: c.t.o.Transport, RequestTimeout: c.t.o.RequestTimeout, PollInterval: c.t.o.PollInterval,
		CardHash: c.ra.CardHash,
	})
	if err != nil {
		r := c.failure(err)
		return nil, &r
	}
	return cl, nil
}

// failure maps an error of the client to a fixed text. A card that changed has its own audit outcome.
func (c *call) failure(err error) mcpgateway.BuiltinResult {
	switch {
	case errors.Is(err, a2aout.ErrCardChanged):
		r := fail(textCardChanged)
		r.Outcome = mcpgateway.OutcomeCardChanged
		return r
	case errors.Is(err, a2aout.ErrEgressDenied):
		return fail(textEgress)
	case errors.Is(err, a2aout.ErrTaskEnded):
		return fail(textEnded)
	case errors.Is(err, a2aout.ErrNotAwaitingInput):
		return fail(textNotAwaiting)
	case errors.Is(err, a2a.ErrTaskNotFound):
		return fail(textNoTask)
	case errors.Is(err, a2a.ErrTaskNotCancelable):
		return fail(textNotCancelled)
	case errors.Is(err, a2aout.ErrContextMismatch), errors.Is(err, a2aout.ErrTaskMismatch), errors.Is(err, a2aout.ErrTaskIDReused):
		// The agent broke the rules of A2A: its answer is not used. The text says nothing of it.
		return fail(textUnanswered)
	}
	r := fail(textUnanswered)
	r.Outcome = mcpgateway.OutcomeUpstreamError
	return r
}

// maxID bounds a task or context ID from a remote agent. The ID is the text of another party.
const maxID = 256

func validID(s string) bool {
	if len(s) > maxID {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (c *call) sealed(r *a2aout.Result) mcpgateway.BuiltinResult {
	if !validID(string(r.TaskID)) || !validID(r.ContextID) {
		return fail(textUnanswered)
	}
	out := reply{State: string(r.State), Reason: string(r.Reason), Question: r.Question, Reply: r.Reply}
	if r.TaskID != "" {
		out.TaskID = c.h.seal(kindTask, c.org, c.agent, c.ra.ID, string(r.TaskID))
	}
	if r.ContextID != "" {
		out.ContextID = c.h.seal(kindContext, c.org, c.agent, c.ra.ID, r.ContextID)
	}
	b, _ := json.Marshal(out)
	return mcpgateway.BuiltinResult{Result: &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}}
}

func (c *call) task() (a2a.TaskID, bool) {
	id, err := c.h.open(kindTask, c.org, c.agent, c.ra.ID, c.a.TaskID)
	return a2a.TaskID(id), err == nil
}

func (c *call) send(ctx context.Context) mcpgateway.BuiltinResult {
	req := a2aout.Request{Text: c.a.Message}
	var id a2a.TaskID
	var cid string
	if c.a.TaskID != "" {
		var ok bool
		if id, ok = c.task(); !ok {
			return fail(textBadHandle)
		}
	}
	if c.a.ContextID != "" {
		var err error
		if cid, err = c.h.open(kindContext, c.org, c.agent, c.ra.ID, c.a.ContextID); err != nil {
			return fail(textBadHandle)
		}
		req.ContextID = cid
	}
	cl, bad := c.client(ctx)
	if bad != nil {
		return *bad
	}
	defer cl.Close() //nolint:errcheck // nothing to do on a failed close
	switch {
	case id != "" && cid != "":
		// Both IDs: read the task. A task that waits for input gets the answer. A task that ended gets a
		// follow-up turn (a new task with the ended task in referenceTaskIds). Any other state is refused.
		t, err := cl.Get(ctx, id)
		if err != nil {
			return c.failure(err)
		}
		if t.ContextID != cid {
			return fail(textBadHandle)
		}
		switch {
		case a2aout.Ended(t.External):
			req.After = id
		case t.External == a2a.TaskStateInputRequired:
			req.AnswerTask = id
		default:
			return fail(textNotAwaiting)
		}
	case id != "":
		// The answer to a task that waits for input. The client refuses any other state.
		req.AnswerTask = id
	}
	res, err := cl.Send(ctx, req)
	if err != nil {
		return c.failure(err)
	}
	if res.TaskID != "" && !res.Settled() {
		wctx, cancel := context.WithTimeout(ctx, c.t.o.Wait)
		defer cancel()
		if w, err := cl.Await(wctx, res.TaskID); err == nil {
			res = w
		} else if ctx.Err() == nil {
			// The wait is over and the task still runs: the caller gets the state it has now, and
			// get_task reads the rest.
			if g, err := cl.Get(ctx, res.TaskID); err == nil {
				res = g
			}
		}
	}
	return c.sealed(res)
}

func (c *call) get(ctx context.Context) mcpgateway.BuiltinResult {
	id, ok := c.task()
	if !ok {
		return fail(textBadHandle)
	}
	cl, bad := c.client(ctx)
	if bad != nil {
		return *bad
	}
	defer cl.Close() //nolint:errcheck // nothing to do on a failed close
	res, err := cl.Get(ctx, id)
	if err != nil {
		return c.failure(err)
	}
	return c.sealed(res)
}

func (c *call) cancel(ctx context.Context) mcpgateway.BuiltinResult {
	id, ok := c.task()
	if !ok {
		return fail(textBadHandle)
	}
	cl, bad := c.client(ctx)
	if bad != nil {
		return *bad
	}
	defer cl.Close() //nolint:errcheck // nothing to do on a failed close
	res, err := cl.Cancel(ctx, id)
	if err != nil {
		return c.failure(err)
	}
	return c.sealed(res)
}
