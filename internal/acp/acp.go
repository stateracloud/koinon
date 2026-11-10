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
// or end-of-file.
func NewConn(h Handler, toAgent io.Writer, fromAgent io.Reader) *Conn {
	t := newTracker()
	obs, _ := h.(ReadObserver)
	c := &Conn{
		c: sdk.NewClientSideConnection(client{h: h, t: t}, toAgent, &countingReader{r: fromAgent, t: t, obs: obs}),
		t: t,
	}
	// When the connection ends for any reason, no handler call will free a blocked reader.
	go func() {
		<-c.c.Done()
		t.abort()
	}()
	return c
}

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

// maxUpdateLag is how many session/update notifications may be read from the agent but not yet
// handled. The SDK queues 1024 notifications between its reader and the handler and closes the
// connection when a burst overflows the queue, so the reader holds the stream (and so the
// harness, through the pipe) at half of that until the handler catches up.
const maxUpdateLag = 512

// tracker counts the session/update notifications read from the agent and those handled,
// to tell when every update of an ended connection has been delivered, and to hold the
// reader back while the handler lags.
type tracker struct {
	mu      sync.Mutex
	cond    *sync.Cond
	read    int64
	done    int64
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

func (t *tracker) handled() { t.update(func() { t.done++ }) }

// abort releases a reader held by waitLag: the connection ended, so nothing will be handled.
func (t *tracker) abort() { t.update(func() { t.aborted = true }) }

// waitLag blocks until fewer than maxUpdateLag updates are waiting for the handler, or the
// connection has ended.
func (t *tracker) waitLag() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for t.read-t.done >= maxUpdateLag && !t.aborted {
		t.cond.Wait()
	}
}

// countingReader passes the agent's output through, one line per Read, and counts its
// session/update notifications. The SDK reads lines in order and queues each notification
// before it reads the next, so once Read has returned an error every counted notification is
// queued. Because a Read never returns more than one line, the reader can stop between two
// notifications: it waits while maxUpdateLag updates are unhandled, which slows the harness to
// the handler's pace instead of overflowing the SDK's queue.
type countingReader struct {
	r   io.Reader
	t   *tracker
	obs ReadObserver

	br   *bufio.Reader
	out  []byte // the rest of the line being handed out
	fail error  // the error to return once out is empty
}

func (cr *countingReader) counted(line []byte) {
	cr.t.waitLag()
	cr.t.update(func() { cr.t.read++ })
	if cr.obs != nil {
		cr.obs.UpdateRead(line)
	}
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
			continue
		}
		if err != nil {
			cr.fail = err
			if len(line) > 0 && isUpdate(line) {
				cr.counted(line)
			}
			cr.t.update(func() { cr.t.eof = true })
		} else if isUpdate(line) {
			cr.counted(line[:len(line)-1])
		}
		break
	}
	cr.out = line
}

// isUpdate reports whether the SDK will hand line to SessionUpdate: a notification of the
// session/update method whose params decode and validate as the SDK does. A line it rejects
// (InvalidParams) is never handled, so counting it would leave the reader waiting for a
// handler call that never comes.
func isUpdate(line []byte) bool {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil || m.ID != nil || m.Method != sdk.ClientMethodSessionUpdate {
		return false
	}
	var n sdk.SessionNotification
	return json.Unmarshal(m.Params, &n) == nil && n.Validate() == nil
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
