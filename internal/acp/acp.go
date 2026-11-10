// SPDX-License-Identifier: Apache-2.0

// Package acp is Koinon's client for the Agent Client Protocol (ACP), the JSON-RPC protocol
// koinon-agentd speaks to a flavor's harness over the stdin and stdout of an OpenShell exec
// stream (ADR 0016, 0049).
//
// It wraps the community SDK github.com/coder/acp-go-sdk, pinned in go.mod (ADR 0037), so that
// the rest of Koinon depends on this package only. The SDK types Koinon uses are re-exported as
// aliases; replacing the SDK means replacing them here.
//
// The client offers no file system and no terminal capability: harnesses work on their own
// sandbox file system, and the sandbox is the boundary. Permission requests go to the
// [Handler].
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/stateracloud/koinon/internal/project"
)

// Re-exported SDK types.
type (
	// SessionID identifies a harness session.
	SessionID = sdk.SessionId
	// SessionNotification is a session/update notification from the agent.
	SessionNotification = sdk.SessionNotification
	// PermissionRequest is a session/request_permission request from the agent.
	PermissionRequest = sdk.RequestPermissionRequest
	// PermissionOption is one of the answers a permission request offers.
	PermissionOption = sdk.PermissionOption
	// PermissionOptionKind classifies a permission option.
	PermissionOptionKind = sdk.PermissionOptionKind
	// InitializeResponse is the agent's answer to initialize: its capabilities and identity.
	InitializeResponse = sdk.InitializeResponse
	// StopReason is why a prompt turn ended.
	StopReason = sdk.StopReason
	// ToolCallStatus is the state of a tool call.
	ToolCallStatus = sdk.ToolCallStatus
	// ToolKind classifies what a tool call does (read, edit, execute, ...).
	ToolKind = sdk.ToolKind
	// SessionUpdate is the payload of a session/update notification.
	SessionUpdate = sdk.SessionUpdate
)

// Stop reasons.
const (
	StopEndTurn   = sdk.StopReasonEndTurn
	StopCancelled = sdk.StopReasonCancelled
	StopRefusal   = sdk.StopReasonRefusal
	StopMaxTokens = sdk.StopReasonMaxTokens
	// StopMaxTurnRequests: the turn hit its limit of model requests.
	StopMaxTurnRequests = sdk.StopReasonMaxTurnRequests
)

// Permission option kinds.
const (
	AllowOnce    = sdk.PermissionOptionKindAllowOnce
	AllowAlways  = sdk.PermissionOptionKindAllowAlways
	RejectOnce   = sdk.PermissionOptionKindRejectOnce
	RejectAlways = sdk.PermissionOptionKindRejectAlways
)

// Tool call states.
const (
	ToolPending    = sdk.ToolCallStatusPending
	ToolInProgress = sdk.ToolCallStatusInProgress
	ToolCompleted  = sdk.ToolCallStatusCompleted
	ToolFailed     = sdk.ToolCallStatusFailed
)

// Tool kinds.
const (
	ToolKindRead       = sdk.ToolKindRead
	ToolKindEdit       = sdk.ToolKindEdit
	ToolKindDelete     = sdk.ToolKindDelete
	ToolKindMove       = sdk.ToolKindMove
	ToolKindSearch     = sdk.ToolKindSearch
	ToolKindExecute    = sdk.ToolKindExecute
	ToolKindThink      = sdk.ToolKindThink
	ToolKindFetch      = sdk.ToolKindFetch
	ToolKindSwitchMode = sdk.ToolKindSwitchMode
	ToolKindOther      = sdk.ToolKindOther
)

// ProtocolVersion is the ACP protocol version Koinon speaks.
const ProtocolVersion = sdk.ProtocolVersionNumber

// Decision answers a permission request: the chosen option, or none to cancel the request.
type Decision struct {
	// OptionID is the selected option. Empty means the request is cancelled.
	OptionID string
}

// Choose returns the decision for the first option of the given kind, or a cancelling
// decision when the request offers none.
func Choose(req PermissionRequest, kinds ...PermissionOptionKind) Decision {
	for _, k := range kinds {
		for _, o := range req.Options {
			if o.Kind == k {
				return Decision{OptionID: string(o.OptionId)}
			}
		}
	}
	return Decision{}
}

// Handler receives what the agent sends on its own initiative. Methods may be called
// concurrently with each other and with the [Conn] methods.
type Handler interface {
	// SessionUpdate receives a session/update notification. Updates of one connection arrive
	// in order.
	SessionUpdate(ctx context.Context, n SessionNotification) error
	// RequestPermission answers a session/request_permission request.
	RequestPermission(ctx context.Context, req PermissionRequest) (Decision, error)
}

// ReadObserver is a [Handler] that also sees each session/update notification as the connection
// reads it off the agent's stream, before the SDK queues it for SessionUpdate. UpdateRead runs on
// the reading goroutine, so the stream waits for it; line is only valid during the call.
type ReadObserver interface {
	UpdateRead(line []byte)
}

// Conn is an ACP client connection to one harness process.
type Conn struct {
	c *sdk.ClientSideConnection
	t *tracker
}

// NewConn starts a client connection that writes requests to toAgent (the harness's stdin)
// and reads from fromAgent (its stdout). The connection ends when fromAgent returns an error
// or end-of-file. Cancelling ctx or calling [Conn.Close] releases a reader that is held back
// by a lagging handler; it does not close fromAgent, so the caller still ends the process.
func NewConn(ctx context.Context, h Handler, toAgent io.Writer, fromAgent io.Reader) *Conn {
	t := newTracker()
	obs, _ := h.(ReadObserver)
	c := &Conn{
		c: sdk.NewClientSideConnection(client{h: h, t: t}, toAgent, &countingReader{r: fromAgent, t: t, obs: obs}),
		t: t,
	}
	// A reader held back by waitLag sits inside the SDK's receive goroutine, which is also what
	// closes Done on a drop, so Done cannot be the only release: ctx and Close release it from
	// outside, and Done covers a connection that ends some other way.
	go func() {
		select {
		case <-ctx.Done():
		case <-c.c.Done():
		}
		t.abort()
	}()
	return c
}

// Close releases the reader if it is held back for a handler that lags. It does not end the
// connection by itself: that happens when the agent's stream ends.
func (c *Conn) Close() { c.t.abort() }

// Done is closed when the connection has ended.
func (c *Conn) Done() <-chan struct{} { return c.c.Done() }

// WaitUpdates waits until the connection has ended and every session update the agent sent
// has been handed to the [Handler]. When a connection drops, the SDK fails pending calls
// first and delivers the updates received before the drop afterwards; waiting for them keeps
// what the handler records in the agent's order. It returns false if ctx ends first.
func (c *Conn) WaitUpdates(ctx context.Context) bool {
	select {
	case <-c.t.drained:
		return true
	case <-ctx.Done():
		return false
	}
}

// SetLogger sets the logger for protocol-level events, such as the connection closing.
func (c *Conn) SetLogger(l *slog.Logger) { c.c.SetLogger(l) }

// Initialize negotiates the protocol version and returns the agent's capabilities.
func (c *Conn) Initialize(ctx context.Context) (InitializeResponse, error) {
	return c.c.Initialize(ctx, sdk.InitializeRequest{
		ProtocolVersion: ProtocolVersion,
		ClientInfo:      &sdk.Implementation{Name: "koinon-agentd", Version: project.Version},
	})
}

// NewSession starts a session working in cwd, an absolute path inside the sandbox. MCP
// servers come from the harness's own configuration (ADR 0048), so none are passed here.
func (c *Conn) NewSession(ctx context.Context, cwd string) (SessionID, error) {
	r, err := c.c.NewSession(ctx, sdk.NewSessionRequest{Cwd: cwd, McpServers: []sdk.McpServer{}})
	if err != nil {
		return "", err
	}
	return r.SessionId, nil
}

// LoadSession loads a stored session. The agent replays its history as session updates
// before the call returns.
func (c *Conn) LoadSession(ctx context.Context, id SessionID, cwd string) error {
	_, err := c.c.LoadSession(ctx, sdk.LoadSessionRequest{SessionId: id, Cwd: cwd, McpServers: []sdk.McpServer{}})
	return err
}

// ModelOption is the session configuration option that selects the model (ACP session config
// options, category "model"); OpenCode v2 has no session/set_model.
const ModelOption = "model"

// SetModel selects the session's model through session/set_config_option. It fails unless the
// agent's answer shows model as the option's current value, so a caller never runs on a model
// the harness chose for itself.
func (c *Conn) SetModel(ctx context.Context, id SessionID, model string) error {
	r, err := c.c.SetSessionConfigOption(ctx, sdk.SetSessionConfigOptionRequest{
		ValueId: &sdk.SetSessionConfigOptionValueId{
			SessionId: id, ConfigId: ModelOption, Value: sdk.SessionConfigValueId(model),
		},
	})
	if err != nil {
		return err
	}
	for _, o := range r.ConfigOptions {
		if o.Select != nil && o.Select.Id == ModelOption {
			if string(o.Select.CurrentValue) != model {
				return fmt.Errorf("acp: the agent did not select model %q", model)
			}
			return nil
		}
	}
	return fmt.Errorf("acp: the agent reports no %q option after selecting %q", ModelOption, model)
}

// Prompt sends a user message and blocks until the turn ends.
func (c *Conn) Prompt(ctx context.Context, id SessionID, text string) (StopReason, error) {
	return c.PromptMeta(ctx, id, text, nil)
}

// PromptMeta is Prompt with meta in the request's _meta (for example a traceparent, ADR 0074).
func (c *Conn) PromptMeta(ctx context.Context, id SessionID, text string, meta map[string]any) (StopReason, error) {
	r, err := c.c.Prompt(ctx, sdk.PromptRequest{Meta: meta, SessionId: id, Prompt: []sdk.ContentBlock{sdk.TextBlock(text)}})
	if err != nil {
		return "", err
	}
	return r.StopReason, nil
}

// Cancel asks the agent to end the session's current turn; the pending [Conn.Prompt] then
// returns [StopCancelled].
func (c *Conn) Cancel(ctx context.Context, id SessionID) error {
	return c.c.Cancel(ctx, sdk.CancelNotification{SessionId: id})
}

// Update is the part of a session update Koinon looks at, flattened.
type Update struct {
	// Kind is the update's discriminator, for example "agent_message_chunk" or "tool_call".
	Kind string
	// Text is the text of a user, agent or thought message chunk.
	Text string
	// ToolCallID, ToolTitle and ToolStatus describe a tool_call or tool_call_update.
	ToolCallID string
	ToolTitle  string
	ToolStatus ToolCallStatus
	// Commands are the names in an available_commands_update.
	Commands []string
}

// Flatten extracts the fields of n that Koinon records.
func Flatten(n SessionNotification) Update {
	u := n.Update
	switch {
	case u.UserMessageChunk != nil:
		return Update{Kind: "user_message_chunk", Text: text(u.UserMessageChunk.Content)}
	case u.AgentMessageChunk != nil:
		return Update{Kind: "agent_message_chunk", Text: text(u.AgentMessageChunk.Content)}
	case u.AgentThoughtChunk != nil:
		return Update{Kind: "agent_thought_chunk", Text: text(u.AgentThoughtChunk.Content)}
	case u.ToolCall != nil:
		return Update{
			Kind: "tool_call", ToolCallID: string(u.ToolCall.ToolCallId),
			ToolTitle: u.ToolCall.Title, ToolStatus: u.ToolCall.Status,
		}
	case u.ToolCallUpdate != nil:
		up := Update{Kind: "tool_call_update", ToolCallID: string(u.ToolCallUpdate.ToolCallId)}
		if u.ToolCallUpdate.Title != nil {
			up.ToolTitle = *u.ToolCallUpdate.Title
		}
		if u.ToolCallUpdate.Status != nil {
			up.ToolStatus = *u.ToolCallUpdate.Status
		}
		return up
	case u.AvailableCommandsUpdate != nil:
		up := Update{Kind: "available_commands_update", Commands: []string{}}
		for _, c := range u.AvailableCommandsUpdate.AvailableCommands {
			up.Commands = append(up.Commands, c.Name)
		}
		return up
	default:
		return Update{Kind: "other"}
	}
}

func text(b sdk.ContentBlock) string {
	if b.Text != nil {
		return b.Text.Text
	}
	return ""
}

// Text concatenates the text of the updates of the given kind.
func Text(updates []Update, kind string) string {
	var b strings.Builder
	for _, u := range updates {
		if u.Kind == kind {
			b.WriteString(u.Text)
		}
	}
	return b.String()
}

// ErrUnsupported answers the client methods Koinon does not offer (file system, terminals).
var ErrUnsupported = errors.New("acp: not supported by koinon-agentd; the harness works on its own sandbox file system")

// client adapts a Handler to the SDK's Client interface.
type client struct {
	h Handler
	t *tracker
}

var _ sdk.Client = client{}

func (c client) SessionUpdate(ctx context.Context, n sdk.SessionNotification) error {
	defer c.t.handled()
	return c.h.SessionUpdate(ctx, n)
}

// maxNotificationLag is how many notifications may be read from the agent but not yet
// processed. The SDK queues 1024 notifications (session/update and every other method without
// an id, except $/cancel_request) between its reader and one dispatching goroutine, and closes
// the connection when a burst overflows the queue, so the reader holds the stream (and so the
// harness, through the pipe) at half of that until the handler catches up.
const maxNotificationLag = 512

// tracker follows the notifications read from the agent. Two things come out of it: when every
// session/update of an ended connection has reached the handler (drained), and how far the
// handler lags behind the reader, to hold the reader back.
//
// The SDK dispatches notifications in order, so the oldest unhandled session/update marks the
// head of the SDK's queue: every notification read after it is still queued, and everything
// before it is done. Only session/update runs the handler; other notifications cost the SDK
// next to nothing, so with no update pending nothing is queued for long.
type tracker struct {
	mu      sync.Mutex
	cond    *sync.Cond
	seq     int64   // notifications read so far
	pend    []int64 // seq of each session/update read and not yet handled, oldest first
	read    int64   // session/updates the SDK will dispatch
	done    int64   // session/updates handled
	eof     bool
	aborted bool
	drained chan struct{}
}

func newTracker() *tracker {
	t := &tracker{drained: make(chan struct{})}
	t.cond = sync.NewCond(&t.mu)
	return t
}

func (t *tracker) update(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f()
	t.cond.Broadcast()
	if t.eof && t.done >= t.read {
		select {
		case <-t.drained:
		default:
			close(t.drained)
		}
	}
}

// notified records a notification read; update is true for one the SDK hands to SessionUpdate.
func (t *tracker) notified(update bool) {
	t.update(func() {
		t.seq++
		if update {
			t.read++
			t.pend = append(t.pend, t.seq)
		}
	})
}

func (t *tracker) handled() {
	t.update(func() {
		t.done++
		if len(t.pend) > 0 {
			t.pend = t.pend[1:]
		}
	})
}

// abort releases a reader held by waitLag: nothing more will be handled.
func (t *tracker) abort() { t.update(func() { t.aborted = true }) }

// waitLag blocks until reading one more notification would leave fewer than maxNotificationLag
// queued behind the oldest unhandled update, or until abort.
func (t *tracker) waitLag() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(t.pend) > 0 && t.seq-t.pend[0]+1 >= maxNotificationLag && !t.aborted {
		t.cond.Wait()
	}
}

// countingReader passes the agent's output through, one line per Read, and counts its
// notifications. The SDK reads lines in order and queues each notification before it reads the
// next, so once Read has returned an error every counted notification is queued. Because a Read
// never returns more than one line, the reader can stop between two notifications: it waits
// while maxNotificationLag are queued behind an unhandled update, which slows the harness to
// the handler's pace instead of overflowing the SDK's queue.
type countingReader struct {
	r   io.Reader
	t   *tracker
	obs ReadObserver

	br   *bufio.Reader
	out  []byte // the rest of the line being handed out
	fail error  // the error to return once out is empty
}

// counted accounts for one notification line. The observer runs before the wait, so the number
// of updates it has seen ahead of the handler does not grow with the lag: the leak guard counts
// them against what the gateway has reported (taint.go).
func (cr *countingReader) counted(line []byte, kind notificationKind) {
	if kind == notifNone {
		return
	}
	if kind == notifUpdate && cr.obs != nil {
		cr.obs.UpdateRead(line)
	}
	cr.t.waitLag()
	cr.t.notified(kind == notifUpdate)
}

func (cr *countingReader) Read(p []byte) (int, error) {
	if len(cr.out) == 0 {
		if cr.fail != nil {
			return 0, cr.fail
		}
		cr.next()
	}
	n := copy(p, cr.out)
	cr.out = cr.out[n:]
	if len(cr.out) == 0 && cr.fail != nil {
		return n, cr.fail
	}
	return n, nil
}

// maxLineBytes is the longest line the SDK reads (maxBufSize of its receive loop in connection.go).
const maxLineBytes = 10 << 20

var errLineTooLong = errors.New("acp: a line from the agent exceeds the size limit")

// next reads one line (or what is left at the end of the stream) into cr.out.
func (cr *countingReader) next() {
	if cr.br == nil {
		cr.br = bufio.NewReaderSize(cr.r, 64<<10)
	}
	var line []byte
	for {
		chunk, err := cr.br.ReadSlice('\n')
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(line) >= maxLineBytes {
				// The SDK would refuse the line at the same size (ErrTooLong); stop before the
				// harness makes agentd buffer more than that.
				cr.fail = errLineTooLong
				cr.t.update(func() { cr.t.eof = true })
				return
			}
			continue
		}
		if err != nil {
			cr.fail = err
			if len(line) > 0 {
				cr.counted(line, classify(line))
			}
			cr.t.update(func() { cr.t.eof = true })
		} else {
			cr.counted(line[:len(line)-1], classify(line))
		}
		break
	}
	cr.out = line
}

type notificationKind int

const (
	notifNone   notificationKind = iota // not queued by the SDK: a response, a request, a cancel or garbage
	notifOther                          // queued, and answered without the handler
	notifUpdate                         // queued, and handed to SessionUpdate
)

// classify tells what the SDK does with line, decoding it as the SDK's receive loop does
// (anyMessage in connection.go). A line that does not decode is dropped there. A message
// without an id (a JSON null is none) and a method is queued, except $/cancel_request, which is
// handled at once. A session/update whose params fail to decode or validate is answered with
// InvalidParams and never reaches the handler, so it counts as other: waiting for a handler
// call that never comes would hold the reader forever.
func classify(line []byte) notificationKind {
	var m struct {
		JSONRPC string            `json:"jsonrpc"`
		ID      *json.RawMessage  `json:"id,omitempty"`
		Method  string            `json:"method,omitempty"`
		Params  json.RawMessage   `json:"params,omitempty"`
		Result  json.RawMessage   `json:"result,omitempty"`
		Error   *sdk.RequestError `json:"error,omitempty"`
	}
	if json.Unmarshal(line, &m) != nil || m.ID != nil || m.Method == "" || m.Method == "$/cancel_request" {
		return notifNone
	}
	if m.Method != sdk.ClientMethodSessionUpdate {
		return notifOther
	}
	var n sdk.SessionNotification
	if json.Unmarshal(m.Params, &n) != nil || n.Validate() != nil {
		return notifOther
	}
	return notifUpdate
}

func (c client) RequestPermission(ctx context.Context, req sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	d, err := c.h.RequestPermission(ctx, req)
	if err != nil {
		return sdk.RequestPermissionResponse{}, err
	}
	if d.OptionID == "" {
		return sdk.RequestPermissionResponse{Outcome: sdk.NewRequestPermissionOutcomeCancelled()}, nil
	}
	return sdk.RequestPermissionResponse{
		Outcome: sdk.NewRequestPermissionOutcomeSelected(sdk.PermissionOptionId(d.OptionID)),
	}, nil
}

func (client) ReadTextFile(context.Context, sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, ErrUnsupported
}

func (client) WriteTextFile(context.Context, sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, ErrUnsupported
}

func (client) CreateTerminal(context.Context, sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, ErrUnsupported
}

func (client) KillTerminal(context.Context, sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, ErrUnsupported
}

func (client) TerminalOutput(context.Context, sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, ErrUnsupported
}

func (client) ReleaseTerminal(context.Context, sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, ErrUnsupported
}

func (client) WaitForTerminalExit(context.Context, sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, ErrUnsupported
}
