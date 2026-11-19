// SPDX-License-Identifier: Apache-2.0

package a2aout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"

	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

const (
	defaultRequestTimeout = 60 * time.Second
	defaultMaxBody        = 4 << 20
	defaultPollInterval   = 2 * time.Second
	// MaxText bounds the question and the reply of a [Result]. The text comes from another party:
	// it is quoted data, and the caller never gets more than this of it.
	MaxText = 64 << 10
)

// Options configure a [Client].
type Options struct {
	// Target is the external agent: URL is its base URL (the agent card is at
	// /.well-known/agent-card.json under it). The credential header, prefix and file, the CA bundle
	// and skipped verification are the same fields as for an MCP target, and mean the same.
	Target gateway.MCPTarget
	// Agent is the Koinon agent that calls. The credential source sees it.
	Agent gateway.AgentRef
	// Credentials resolves the credential on every request. Nil is [mcpgateway.StaticCredentials].
	Credentials mcpgateway.CredentialSource
	// Policy is the platform URL policy (ADR 0120 egress): the client dials only addresses it
	// allows, checked at dial time, as the other outbound clients of Koinon do.
	Policy catalog.URLPolicy
	// Transport replaces the policy dialer. It is for tests only.
	Transport http.RoundTripper
	// RequestTimeout bounds one request to the agent. Zero is 60 seconds.
	RequestTimeout time.Duration
	// MaxBody bounds one response body. Zero is 4 MiB.
	MaxBody int64
	// PollInterval is the time between two reads in [Client.Await]. Zero is 2 seconds.
	PollInterval time.Duration
}

// Client calls one external A2A agent on behalf of one Koinon agent. It holds no credential: the
// credential is read on each request and set on the request by the HTTP client (ADR 0120).
type Client struct {
	c        *a2aclient.Client
	poll     time.Duration
	timeout  time.Duration
	endpoint string
}

// New resolves the agent card of the target and returns a client for it. The card must offer an
// interface on the host of the target: a card that points elsewhere is refused, so a card cannot
// move the credential or the traffic to another host.
func New(ctx context.Context, o Options) (*Client, error) {
	if o.Credentials == nil {
		o.Credentials = mcpgateway.StaticCredentials{}
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = defaultRequestTimeout
	}
	if o.MaxBody <= 0 {
		o.MaxBody = defaultMaxBody
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	base, err := url.Parse(o.Target.URL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return nil, fmt.Errorf("%w: %q", ErrBadTarget, o.Target.URL)
	}
	if err := o.Policy.CheckStrict(ctx, o.Target.URL); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEgressDenied, err)
	}
	rt := o.Transport
	if rt == nil {
		rt = policyTransport(o.Policy)
	}
	hc, err := mcpgateway.NewTargetHTTPClient(rt, o.Target, o.Agent, o.Credentials)
	if err != nil {
		return nil, err
	}
	hc.Transport = &limitTransport{base: hc.Transport, max: o.MaxBody}
	hc.Timeout = o.RequestTimeout

	rctx, cancel := context.WithTimeout(ctx, o.RequestTimeout)
	defer cancel()
	card, err := resolveCard(rctx, hc, o.Target.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCard, err)
	}
	card.SupportedInterfaces = sameHost(card.SupportedInterfaces, base.Host)
	if len(card.SupportedInterfaces) == 0 {
		return nil, fmt.Errorf("%w: no JSON-RPC interface on %s", ErrCardHost, base.Host)
	}
	c, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithJSONRPCTransport(hc),
		a2av0.WithJSONRPCTransport(a2av0.JSONRPCTransportConfig{Client: hc}),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCard, err)
	}
	return &Client{c: c, poll: o.PollInterval, timeout: o.RequestTimeout, endpoint: base.Host}, nil
}

// resolveCard reads the card. A v1.0 card is parsed first; a v0.3 card, which kagent serves, is
// parsed by the compatibility parser.
func resolveCard(ctx context.Context, hc *http.Client, baseURL string) (*a2a.AgentCard, error) {
	r := agentcard.NewResolver(hc)
	r.CardParser = func(b []byte) (*a2a.AgentCard, error) {
		card, err := agentcard.DefaultCardParser(b)
		if err == nil && len(card.SupportedInterfaces) > 0 {
			return card, nil
		}
		return a2av0.NewAgentCardParser()(b)
	}
	return r.Resolve(ctx, baseURL)
}

// sameHost keeps the JSON-RPC interfaces of the card that are on host.
func sameHost(in []*a2a.AgentInterface, host string) []*a2a.AgentInterface {
	var out []*a2a.AgentInterface
	for _, i := range in {
		u, err := url.Parse(i.URL)
		if err != nil || u.Host != host || i.ProtocolBinding != a2a.TransportProtocolJSONRPC {
			continue
		}
		out = append(out, i)
	}
	return out
}

// policyTransport dials through the URL policy and never through a proxy: the policy must see the
// address that is dialled.
func policyTransport(p catalog.URLPolicy) http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = p.DialContext(&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second})
	return t
}

// Request is one turn sent to the external agent (ADR 0123 decision 8).
type Request struct {
	// Text is the message.
	Text string
	// MessageID is the idempotency key. Empty makes a new one. A retry of the same message uses
	// the same ID.
	MessageID string
	// ContextID is the thread. Empty starts a new one.
	ContextID string
	// AnswerTask answers a task that waits for input. The task must be in `input-required`: the
	// client reads it first and refuses any other state.
	AnswerTask a2a.TaskID
	// After starts a new task in ContextID that follows an ended task (a follow-up turn). The new
	// message carries it in referenceTaskIds. It needs ContextID, and excludes AnswerTask.
	After a2a.TaskID
	// ReturnImmediately returns at once with the task as it stands. Otherwise the agent waits
	// until the task is settled, up to its own limit, and [Client.Await] can wait further.
	ReturnImmediately bool
}

// Result is what the external agent answered, in Koinon's terms.
type Result struct {
	// TaskID is empty when the agent answered with a message and no task.
	TaskID    a2a.TaskID
	ContextID string
	// State is the Koinon task state (see [State]). An answer that is only a message is `completed`.
	State  store.TaskState
	Reason Reason
	// External is the state as the agent sent it.
	External a2a.TaskState
	// Question is the text of the status message of a task that waits for input.
	Question string
	// Reply is the text of the artifacts of the task, or of the message. It is another party's
	// text: quoted data, cut at [MaxText].
	Reply string
}

// Settled reports whether the turn needs no more waiting: it ended, or it waits for input.
func (r *Result) Settled() bool {
	return r.External == "" || settled(r.External)
}

// Send sends one turn. The rules of A2A 1.0.0 hold on the client side too: a task that ended never
// gets a message (the follow-up is a new task with referenceTaskIds), and a task ID is not reused.
func (c *Client) Send(ctx context.Context, r Request) (*Result, error) {
	if strings.TrimSpace(r.Text) == "" {
		return nil, fmt.Errorf("%w: the message is empty", ErrBadRequest)
	}
	switch {
	case r.AnswerTask != "" && r.After != "":
		return nil, fmt.Errorf("%w: AnswerTask and After exclude each other", ErrBadRequest)
	case r.After != "" && r.ContextID == "":
		return nil, fmt.Errorf("%w: a follow-up turn needs the context ID", ErrBadRequest)
	}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(r.Text))
	if r.MessageID != "" {
		msg.ID = r.MessageID
	}
	msg.ContextID = r.ContextID
	if r.AnswerTask != "" {
		t, err := c.get(ctx, r.AnswerTask)
		if err != nil {
			return nil, err
		}
		if Ended(t.Status.State) {
			return nil, fmt.Errorf("%w: %s is %s", ErrTaskEnded, r.AnswerTask, t.Status.State)
		}
		if t.Status.State != a2a.TaskStateInputRequired && t.Status.State != a2a.TaskStateAuthRequired {
			return nil, fmt.Errorf("%w: %s is %s", ErrNotAwaitingInput, r.AnswerTask, t.Status.State)
		}
		if r.ContextID != "" && t.ContextID != r.ContextID {
			return nil, fmt.Errorf("%w: task %s is not in context %s", ErrContextMismatch, r.AnswerTask, r.ContextID)
		}
		msg.TaskID, msg.ContextID = r.AnswerTask, t.ContextID
	}
	if r.After != "" {
		msg.ReferenceTasks = []a2a.TaskID{r.After}
	}
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	res, err := c.c.SendMessage(rctx, &a2a.SendMessageRequest{
		Message: msg,
		Config:  &a2a.SendMessageConfig{ReturnImmediately: r.ReturnImmediately},
	})
	if err != nil {
		return nil, wrap(err)
	}
	out, err := fromResult(res)
	if err != nil {
		return nil, err
	}
	switch {
	case r.After != "" && out.TaskID == r.After:
		return nil, fmt.Errorf("%w: the agent answered a follow-up with task %s", ErrTaskIDReused, out.TaskID)
	case r.AnswerTask != "" && out.TaskID != "" && out.TaskID != r.AnswerTask:
		return nil, fmt.Errorf("%w: answered %s, got task %s", ErrTaskMismatch, r.AnswerTask, out.TaskID)
	case r.ContextID != "" && out.ContextID != "" && out.ContextID != r.ContextID:
		return nil, fmt.Errorf("%w: sent %s, got %s", ErrContextMismatch, r.ContextID, out.ContextID)
	}
	return out, nil
}

// Get reads a task.
func (c *Client) Get(ctx context.Context, id a2a.TaskID) (*Result, error) {
	t, err := c.get(ctx, id)
	if err != nil {
		return nil, err
	}
	return fromTask(t)
}

func (c *Client) get(ctx context.Context, id a2a.TaskID) (*a2a.Task, error) {
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	t, err := c.c.GetTask(rctx, &a2a.GetTaskRequest{ID: id})
	if err != nil {
		return nil, wrap(err)
	}
	if t.ID != id {
		return nil, fmt.Errorf("%w: asked %s, got %s", ErrTaskMismatch, id, t.ID)
	}
	return t, nil
}

// Cancel asks the agent to cancel a task. An agent that cannot cancel it, for example because it
// ended, answers with [a2a.ErrTaskNotCancelable].
func (c *Client) Cancel(ctx context.Context, id a2a.TaskID) (*Result, error) {
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	t, err := c.c.CancelTask(rctx, &a2a.CancelTaskRequest{ID: id})
	if err != nil {
		return nil, wrap(err)
	}
	return fromTask(t)
}

// Await reads the task until it is settled (it ended, or it waits for input) or ctx ends. The
// client does not stream: a streaming subscription to a task that ended is an error in A2A 1.0.0,
// and a read loop has no such state to race with.
func (c *Client) Await(ctx context.Context, id a2a.TaskID) (*Result, error) {
	tick := time.NewTicker(c.poll)
	defer tick.Stop()
	for {
		r, err := c.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if r.Settled() {
			return r, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

// Close frees the client.
func (c *Client) Close() error { return c.c.Destroy() }

func wrap(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		// A transport error names the URL and the cause. It never holds a header value.
		return fmt.Errorf("%w: %w", ErrTransport, err)
	}
	return err
}

func fromResult(res a2a.SendMessageResult) (*Result, error) {
	switch v := res.(type) {
	case *a2a.Task:
		return fromTask(v)
	case *a2a.Message:
		// A message with no task is a finished answer: nothing is left to wait for.
		return &Result{
			ContextID: v.ContextID, State: store.TaskStateCompleted,
			Reply: clip(text(v.Parts)),
		}, nil
	default:
		return nil, fmt.Errorf("%w: unexpected result %T", ErrBadResponse, res)
	}
}

func fromTask(t *a2a.Task) (*Result, error) {
	st, reason, err := State(t.Status.State)
	if err != nil {
		return nil, err
	}
	r := &Result{
		TaskID: t.ID, ContextID: t.ContextID, State: st, Reason: reason, External: t.Status.State,
	}
	if st == store.TaskStateInputRequired && t.Status.Message != nil {
		r.Question = clip(text(t.Status.Message.Parts))
	}
	var parts []string
	for _, a := range t.Artifacts {
		if s := text(a.Parts); s != "" {
			parts = append(parts, s)
		}
	}
	r.Reply = clip(strings.Join(parts, "\n\n"))
	if r.Reply == "" && st == store.TaskStateCompleted && t.Status.Message != nil {
		r.Reply = clip(text(t.Status.Message.Parts))
	}
	return r, nil
}

func text(parts a2a.ContentParts) string {
	var b []string
	for _, p := range parts {
		if s := p.Text(); s != "" {
			b = append(b, s)
		}
	}
	return strings.Join(b, "\n")
}

func clip(s string) string {
	if len(s) <= MaxText {
		return s
	}
	cut := MaxText
	for cut > 0 && s[cut]&0xC0 == 0x80 { // do not cut inside a UTF-8 sequence
		cut--
	}
	return s[:cut]
}

// limitTransport bounds each response body: an agent is another party and can send without end.
type limitTransport struct {
	base http.RoundTripper
	max  int64
}

func (t *limitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &limitedBody{rc: resp.Body, left: t.max}
	return resp, nil
}

type limitedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, ErrBodyTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	if b.left <= 0 && err == nil {
		// Only an error tells the reader that the body was cut: do not look like a clean end.
		var one [1]byte
		if m, _ := b.rc.Read(one[:]); m > 0 {
			return n, ErrBodyTooLarge
		}
	}
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }
