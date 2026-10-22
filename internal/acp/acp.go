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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/procoduck/koinon/internal/project"
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
	return &Conn{
		c: sdk.NewClientSideConnection(client{h: h, t: t}, toAgent, &countingReader{r: fromAgent, t: t}),
		t: t,
	}
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

// Prompt sends a user message and blocks until the turn ends.
func (c *Conn) Prompt(ctx context.Context, id SessionID, text string) (StopReason, error) {
	r, err := c.c.Prompt(ctx, sdk.PromptRequest{SessionId: id, Prompt: []sdk.ContentBlock{sdk.TextBlock(text)}})
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

// tracker counts the session/update notifications read from the agent and those handled,
// to tell when every update of an ended connection has been delivered.
type tracker struct {
	mu      sync.Mutex
	read    int64
	done    int64
	eof     bool
	drained chan struct{}
}

func newTracker() *tracker { return &tracker{drained: make(chan struct{})} }

func (t *tracker) update(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f()
	if t.eof && t.done >= t.read {
		select {
		case <-t.drained:
		default:
			close(t.drained)
		}
	}
}

func (t *tracker) handled() { t.update(func() { t.done++ }) }

// countingReader passes the agent's output through and counts its session/update
// notifications. The SDK reads lines in order and queues each notification before it reads
// the next, so once Read has returned an error every counted notification is queued.
type countingReader struct {
	r       io.Reader
	t       *tracker
	partial []byte
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	data := p[:n]
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			cr.partial = append(cr.partial, data...)
			break
		}
		line := data[:i]
		if len(cr.partial) > 0 {
			line = append(cr.partial, line...)
			cr.partial = nil
		}
		if isUpdate(line) {
			cr.t.update(func() { cr.t.read++ })
		}
		data = data[i+1:]
	}
	if err != nil {
		if len(cr.partial) > 0 && isUpdate(cr.partial) {
			cr.t.update(func() { cr.t.read++ })
		}
		cr.partial = nil
		cr.t.update(func() { cr.t.eof = true })
	}
	return n, err
}

func isUpdate(line []byte) bool {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	return json.Unmarshal(line, &m) == nil && m.ID == nil && m.Method == sdk.ClientMethodSessionUpdate
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
