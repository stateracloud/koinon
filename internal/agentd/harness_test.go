// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	. "github.com/onsi/ginkgo/v2"
)

// Prompt markers that script the fake harness.
const (
	// markPermit: ask permission for an edit and for a fetch, then report the answers.
	markPermit = "PERMIT"
	// markWait: stream one chunk and wait until the turn is cancelled.
	markWait = "WAIT"
	// markCrash: on the first attempt stream one chunk and exit with 137 mid-turn, as a
	// crashed harness or a dropped stream would; the resumed turn finishes.
	markCrash = "CRASH-ONCE"
	// markHang: on the first attempt stream two chunks and hang until the stream goes away
	// (the worker is killed); the resumed turn finishes.
	markHang = "HANG-ONCE"
	// markOutside: ask permission for reads and an edit inside and outside the session
	// directory, all with ordinary kinds, then report the answers.
	markOutside = "OUTSIDE"
	// markRefuse: ask permission for an edit outside the session directory; when it is rejected
	// end the turn as cancelled, as OpenCode 2.2.0 does (a plain decline interrupts the step).
	// The prompt that continues the turn carries agentd's note instead.
	markRefuse = "REFUSE-PATH"
	// wrapUpMarker opens agentd's wrap-up notice (a prompt of its own, or the start of a resumed
	// turn's prompt). The fake answers it with wrapUpReply and waits for the next cancel.
	wrapUpMarker = "[Koinon] About"
	// limitWrapUpMarker opens the notice of a request or spend limit (ADR 0081).
	limitWrapUpMarker = "[Koinon] This run has"
	// wrapUpReply is what the fake writes when it is told to wrap up.
	wrapUpReply = "Done so far: A. Left: B."
	// resumeMarker is how the fake harness recognises agentd's resume note.
	resumeMarker = "[Koinon]"
	// summaryMarker opens agentd's end-of-turn summary prompt.
	summaryMarker = "[Koinon] Your turn is over."
	// readyCommand is what the fake harness announces once a session is up, as OpenCode does
	// for Koinon's MCP readiness prompt.
	readyCommand = "koinon:github_koinon-ready"
)

// harness is a scripted ACP agent standing in for a flavor's harness behind exec. Each exec
// of the launch command is a new process, but sessions persist across them, as they do on
// the agent's home volume.
type harness struct {
	mu       sync.Mutex
	sessions map[string][]sdk.SessionUpdate
	nextID   int
	args     [][]string
	prompts  []string
	// summaries are the end-of-turn summary prompts, kept apart from prompts. summaryReply is
	// what the fake writes (default a short note); summaryTool makes it call a tool and ask for
	// permission instead; summaryHang makes it wait until the prompt is cancelled.
	summaries     []string
	summaryReply  string
	summaryTool   bool
	summaryHang   bool
	summaryAnswer string
	// onSummary runs when the summary prompt arrives.
	onSummary func()
	// summaryUpdates are sent during the summary prompt, before its reply: tools that run then.
	summaryUpdates []sdk.SessionUpdate
	loads          []string
	cancels        int
	crashed        bool
	hung           bool
	hangs          chan struct{} // receives when a hang starts
	// onPrompt, if set, may answer a prompt itself (for example by calling Koinon's tools as
	// the agent would): it returns the reply and true, or false to fall through.
	onPrompt func(ctx context.Context, text string) (string, bool)
	// script, if set, may drive a prompt itself with session updates (tool calls, thinking): it
	// returns true when it answered, and the turn then ends with end_turn.
	metas  []map[string]any
	script func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool
	// modelFault, if set, runs before every prompt is answered, as the harness's request to its
	// model would: a non-nil error is what the prompt fails with.
	modelFault func(ctx context.Context, text string) error
	// models are the values of session/set_config_option for the model, in order.
	models []string
	// silent: announce no ready command; stubborn: keep its own model.
	silent, stubborn bool
	// refuseAll makes every prompt but the summary ask for an outside edit and end cancelled when
	// it is rejected, so a run never gets past its refusals.
	refuseAll bool
	// lingers: keep running after end-of-file on stdin, until the exec is closed.
	lingers bool
	// noChunk makes a markWait turn stream nothing before it waits.
	noChunk bool
}

func newHarness() *harness {
	return &harness{sessions: map[string][]sdk.SessionUpdate{}, hangs: make(chan struct{}, 4)}
}

// command is the fakegateway command for the launch command.
func (h *harness) command(ctx context.Context, args []string, stdin io.Reader, stdout, _ io.Writer) int {
	h.mu.Lock()
	h.args = append(h.args, slices.Clone(args))
	h.mu.Unlock()
	p := &process{h: h, crash: make(chan struct{}), cancelled: make(chan struct{}), ready: make(chan struct{})}
	p.conn = sdk.NewAgentSideConnection(p, stdout, stdin)
	close(p.ready)
	select {
	case <-p.conn.Done():
		h.mu.Lock()
		lingers := h.lingers
		h.mu.Unlock()
		if lingers {
			<-ctx.Done()
			return 143
		}
		return 0
	case <-ctx.Done():
		return 143
	case <-p.crash:
		return 137
	}
}

func (h *harness) scriptHook() func(ctx context.Context, text string, say func(sdk.SessionUpdate) error) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.script
}

// promptMetas are the _meta of the prompts that arrived, in order.
func (h *harness) promptMetas() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.metas)
}

func (h *harness) selectedModels() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.models)
}

func (h *harness) set(f func(h *harness)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

// announce sends the ready command after the session/new or session/load answer, as OpenCode
// does once its MCP server is connected.
func (p *process) announce(id sdk.SessionId) {
	p.h.mu.Lock()
	silent := p.h.silent
	p.h.mu.Unlock()
	if silent {
		return
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = p.agent().SessionUpdate(context.Background(), sdk.SessionNotification{SessionId: id, Update: sdk.SessionUpdate{
			AvailableCommandsUpdate: &sdk.SessionAvailableCommandsUpdate{
				SessionUpdate:     "available_commands_update",
				AvailableCommands: []sdk.AvailableCommand{{Name: "compact"}, {Name: readyCommand}},
			},
		}})
	}()
}

// summary answers agentd's end-of-turn summary prompt.
func (p *process) summary(ctx context.Context, id sdk.SessionId, text string) (sdk.PromptResponse, error) {
	p.h.mu.Lock()
	p.h.summaries = append(p.h.summaries, text)
	reply, tool, hang, hook, updates := p.h.summaryReply, p.h.summaryTool, p.h.summaryHang, p.h.onSummary, p.h.summaryUpdates
	p.h.mu.Unlock()
	if hook != nil {
		hook()
	}
	for _, u := range updates {
		if err := p.agent().SessionUpdate(ctx, sdk.SessionNotification{SessionId: id, Update: u}); err != nil {
			return sdk.PromptResponse{}, err
		}
	}
	if reply == "" {
		reply = "Notes: the task is done; nothing is left."
	}
	end := sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}
	if hang {
		select {
		case <-p.cancelCh():
		case <-ctx.Done():
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil
	}
	if tool {
		tid := sdk.ToolCallId("summary-tool")
		name, k := "edit a file", sdk.ToolKindEdit
		if err := p.agent().SessionUpdate(ctx, sdk.SessionNotification{SessionId: id, Update: sdk.StartToolCall(tid, name, sdk.WithStartKind(k))}); err != nil {
			return sdk.PromptResponse{}, err
		}
		res, err := p.agent().RequestPermission(ctx, sdk.RequestPermissionRequest{
			SessionId: id,
			ToolCall:  sdk.ToolCallUpdate{ToolCallId: tid, Title: &name, Kind: &k},
			Options: []sdk.PermissionOption{
				{OptionId: "allow", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
				{OptionId: "reject", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil {
			return sdk.PromptResponse{}, err
		}
		p.h.mu.Lock()
		if res.Outcome.Selected != nil {
			p.h.summaryAnswer = string(res.Outcome.Selected.OptionId)
		}
		p.h.mu.Unlock()
	}
	if err := p.agent().SessionUpdate(ctx, sdk.SessionNotification{SessionId: id, Update: sdk.UpdateAgentMessageText(reply)}); err != nil {
		return sdk.PromptResponse{}, err
	}
	return end, nil
}

// summarySnapshot returns the summary prompts and the answer to the summary's permission request.
func (h *harness) summarySnapshot() ([]string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.summaries), h.summaryAnswer
}

func (h *harness) snapshot() (prompts, loads []string, cancels int, args [][]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.prompts), slices.Clone(h.loads), h.cancels, slices.Clone(h.args)
}

// process is one harness process: one ACP connection.
type process struct {
	h         *harness
	conn      *sdk.AgentSideConnection
	crash     chan struct{}
	ready     chan struct{} // closed once conn is set
	once      sync.Once
	cancelled chan struct{}
}

var _ sdk.Agent = (*process)(nil)

// agent returns the connection, once the command has set it.
func (p *process) agent() *sdk.AgentSideConnection {
	<-p.ready
	return p.conn
}

func (p *process) say(ctx context.Context, id sdk.SessionId, u sdk.SessionUpdate) error {
	p.h.mu.Lock()
	p.h.sessions[string(id)] = append(p.h.sessions[string(id)], u)
	p.h.mu.Unlock()
	return p.agent().SessionUpdate(ctx, sdk.SessionNotification{SessionId: id, Update: u})
}

func (p *process) Initialize(context.Context, sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return sdk.InitializeResponse{
		ProtocolVersion:   sdk.ProtocolVersionNumber,
		AgentCapabilities: sdk.AgentCapabilities{LoadSession: true},
		AgentInfo:         &sdk.Implementation{Name: "fake-harness", Version: "1"},
	}, nil
}

func (p *process) NewSession(_ context.Context, r sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	p.h.mu.Lock()
	p.h.nextID++
	id := fmt.Sprintf("ses_%d", p.h.nextID)
	p.h.sessions[id] = nil
	p.h.mu.Unlock()
	_ = r
	p.announce(sdk.SessionId(id))
	return sdk.NewSessionResponse{SessionId: sdk.SessionId(id)}, nil
}

func (p *process) LoadSession(ctx context.Context, r sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
	p.h.mu.Lock()
	hist, ok := p.h.sessions[string(r.SessionId)]
	p.h.loads = append(p.h.loads, string(r.SessionId))
	hist = slices.Clone(hist)
	p.h.mu.Unlock()
	if !ok {
		return sdk.LoadSessionResponse{}, sdk.NewInvalidParams(map[string]any{"sessionId": "unknown"})
	}
	for _, u := range hist {
		if err := p.agent().SessionUpdate(ctx, sdk.SessionNotification{SessionId: r.SessionId, Update: u}); err != nil {
			return sdk.LoadSessionResponse{}, err
		}
	}
	p.announce(r.SessionId)
	return sdk.LoadSessionResponse{}, nil
}

func (p *process) Prompt(ctx context.Context, r sdk.PromptRequest) (sdk.PromptResponse, error) {
	// The callbacks a spec installs (modelFault, onPrompt, onRefuse, onSummary) run here, on the
	// ACP SDK's goroutine: a failed Expect in one fails the spec instead of crashing the binary.
	defer GinkgoRecover()
	text := r.Prompt[0].Text.Text
	if strings.HasPrefix(text, summaryMarker) {
		return p.summary(ctx, r.SessionId, text)
	}
	p.h.mu.Lock()
	p.h.prompts = append(p.h.prompts, text)
	p.h.metas = append(p.h.metas, r.Meta)
	p.h.sessions[string(r.SessionId)] = append(p.h.sessions[string(r.SessionId)], sdk.UpdateUserMessageText(text))
	firstCrash := strings.Contains(text, markCrash) && !p.h.crashed
	if firstCrash {
		p.h.crashed = true
	}
	firstHang := strings.Contains(text, markHang) && !p.h.hung
	if firstHang {
		p.h.hung = true
	}
	onPrompt := p.h.onPrompt
	modelFault := p.h.modelFault
	refuseAll := p.h.refuseAll
	p.h.mu.Unlock()
	id := r.SessionId
	end := sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}

	if modelFault != nil {
		if err := modelFault(ctx, text); err != nil {
			return sdk.PromptResponse{}, err
		}
	}
	if script := p.h.scriptHook(); script != nil {
		if script(ctx, text, func(u sdk.SessionUpdate) error { return p.say(ctx, id, u) }) {
			return end, nil
		}
	}
	if onPrompt != nil {
		if reply, ok := onPrompt(ctx, text); ok {
			if err := p.say(ctx, id, sdk.UpdateAgentMessageText(reply)); err != nil {
				return sdk.PromptResponse{}, err
			}
			return end, nil
		}
	}

	switch {
	case strings.Contains(text, markRefuse) || refuseAll:
		tid, name, k := sdk.ToolCallId("refuse-"+fmt.Sprint(len(text))), "write", sdk.ToolKindEdit
		if err := p.say(ctx, id, sdk.StartToolCall(tid, name, sdk.WithStartKind(k))); err != nil {
			return sdk.PromptResponse{}, err
		}
		res, err := p.agent().RequestPermission(ctx, sdk.RequestPermissionRequest{
			SessionId: id,
			ToolCall: sdk.ToolCallUpdate{
				ToolCallId: tid, Title: &name, Kind: &k,
				Locations: []sdk.ToolCallLocation{{Path: "/sandbox/.koinon/notes/external/report.md"}},
			},
			Options: []sdk.PermissionOption{
				{OptionId: "allow", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
				{OptionId: "reject", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
			},
		})
		if err != nil {
			return sdk.PromptResponse{}, err
		}
		if res.Outcome.Selected != nil && res.Outcome.Selected.OptionId != "reject" {
			return end, nil
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil

	case strings.HasPrefix(text, wrapUpMarker), strings.HasPrefix(text, limitWrapUpMarker):
		// Told to wrap up: say where things stand, then wait for the next cancel (the time limit).
		p.resetCancel()
		if err := p.say(ctx, id, sdk.UpdateAgentMessageText(wrapUpReply)); err != nil {
			return sdk.PromptResponse{}, err
		}
		select {
		case <-p.cancelCh():
		case <-ctx.Done():
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil

	case strings.Contains(text, resumeMarker):
		if err := p.say(ctx, id, sdk.UpdateAgentMessageText("resumed")); err != nil {
			return sdk.PromptResponse{}, err
		}
		return end, nil

	case firstCrash:
		if err := p.say(ctx, id, sdk.UpdateAgentMessageText("before the crash ")); err != nil {
			return sdk.PromptResponse{}, err
		}
		p.once.Do(func() { close(p.crash) })
		<-ctx.Done()
		return sdk.PromptResponse{}, ctx.Err()

	case firstHang:
		for _, s := range []string{"one ", "two "} {
			if err := p.say(ctx, id, sdk.UpdateAgentMessageText(s)); err != nil {
				return sdk.PromptResponse{}, err
			}
		}
		p.h.hangs <- struct{}{}
		<-ctx.Done()
		return sdk.PromptResponse{}, ctx.Err()

	case strings.Contains(text, markWait):
		p.h.mu.Lock()
		noChunk := p.h.noChunk
		p.h.mu.Unlock()
		if !noChunk {
			if err := p.say(ctx, id, sdk.UpdateAgentMessageText("working")); err != nil {
				return sdk.PromptResponse{}, err
			}
		}
		select {
		case <-p.cancelCh():
		case <-ctx.Done():
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil

	case strings.Contains(text, markOutside):
		var answers []string
		for i, c := range []struct {
			name, path string
			kind       sdk.ToolKind
		}{
			{"read-in", "/sandbox/work/notes.txt", sdk.ToolKindRead},
			{"read-relative", "src/main.go", sdk.ToolKindRead},
			{"read-out", "/etc/passwd", sdk.ToolKindRead},
			{"read-dotdot", "/sandbox/work/../.koinon/AGENTS.md", sdk.ToolKindRead},
			{"edit-out", "/sandbox/.koinon/AGENTS.md", sdk.ToolKindEdit},
		} {
			tid := sdk.ToolCallId(fmt.Sprintf("o-%d", i))
			k := c.kind
			if err := p.say(ctx, id, sdk.StartToolCall(tid, c.name, sdk.WithStartKind(k))); err != nil {
				return sdk.PromptResponse{}, err
			}
			res, err := p.agent().RequestPermission(ctx, sdk.RequestPermissionRequest{
				SessionId: id,
				ToolCall: sdk.ToolCallUpdate{
					ToolCallId: tid, Title: &c.name, Kind: &k,
					Locations: []sdk.ToolCallLocation{{Path: c.path}},
				},
				Options: []sdk.PermissionOption{
					{OptionId: "allow", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
					{OptionId: "reject", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
				},
			})
			if err != nil {
				return sdk.PromptResponse{}, err
			}
			answer := "cancelled"
			if res.Outcome.Selected != nil {
				answer = string(res.Outcome.Selected.OptionId)
			}
			answers = append(answers, c.name+"="+answer)
		}
		if err := p.say(ctx, id, sdk.UpdateAgentMessageText(strings.Join(answers, ","))); err != nil {
			return sdk.PromptResponse{}, err
		}
		return end, nil

	case strings.Contains(text, markPermit):
		if err := p.say(ctx, id, sdk.UpdateAgentMessageText("asking: ")); err != nil {
			return sdk.PromptResponse{}, err
		}
		var answers []string
		for _, kind := range []sdk.ToolKind{sdk.ToolKindEdit, sdk.ToolKindFetch} {
			title := "use " + string(kind)
			k := kind
			if err := p.say(ctx, id, sdk.StartToolCall(sdk.ToolCallId("t-"+string(kind)), title, sdk.WithStartKind(k))); err != nil {
				return sdk.PromptResponse{}, err
			}
			res, err := p.agent().RequestPermission(ctx, sdk.RequestPermissionRequest{
				SessionId: id,
				ToolCall:  sdk.ToolCallUpdate{ToolCallId: sdk.ToolCallId("t-" + string(kind)), Title: &title, Kind: &k},
				Options: []sdk.PermissionOption{
					{OptionId: "allow", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
					{OptionId: "reject", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
				},
			})
			if err != nil {
				return sdk.PromptResponse{}, err
			}
			answer := "cancelled"
			if res.Outcome.Selected != nil {
				answer = string(res.Outcome.Selected.OptionId)
			}
			answers = append(answers, string(kind)+"="+answer)
		}
		if err := p.say(ctx, id, sdk.UpdateAgentMessageText(strings.Join(answers, ","))); err != nil {
			return sdk.PromptResponse{}, err
		}
		return end, nil

	default:
		if err := p.say(ctx, id, sdk.UpdateAgentMessageText("Hello from the fake harness.")); err != nil {
			return sdk.PromptResponse{}, err
		}
		return end, nil
	}
}

func (p *process) Cancel(context.Context, sdk.CancelNotification) error {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	p.h.cancels++
	select {
	case <-p.cancelled:
	default:
		close(p.cancelled)
	}
	return nil
}

// cancelCh is the channel a session/cancel closes.
func (p *process) cancelCh() <-chan struct{} {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	return p.cancelled
}

// resetCancel arms the channel again, so a turn after a cancelled one can be cancelled in its turn.
func (p *process) resetCancel() {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	p.cancelled = make(chan struct{})
}

func (p *process) Authenticate(context.Context, sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	return sdk.AuthenticateResponse{}, nil
}

func (p *process) Logout(context.Context, sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	return sdk.LogoutResponse{}, nil
}

func (p *process) CloseSession(context.Context, sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	return sdk.CloseSessionResponse{}, nil
}

func (p *process) ListSessions(context.Context, sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.ListSessionsResponse{}, nil
}

func (p *process) ResumeSession(context.Context, sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	return sdk.ResumeSessionResponse{}, nil
}

// SetSessionConfigOption answers like OpenCode: the full option set, with the selected model
// as current, unless the harness is stubborn.
func (p *process) SetSessionConfigOption(_ context.Context, r sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	if r.ValueId == nil || r.ValueId.ConfigId != "model" {
		return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]any{"configId": "unsupported"})
	}
	p.h.mu.Lock()
	p.h.models = append(p.h.models, string(r.ValueId.Value))
	current := r.ValueId.Value
	if p.h.stubborn {
		current = "opencode/some-free-model"
	}
	p.h.mu.Unlock()
	values := sdk.SessionConfigSelectOptionsUngrouped{{Name: string(current), Value: current}}
	return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{{
		Select: &sdk.SessionConfigOptionSelect{
			Id: "model", Name: "Model", Type: "select", CurrentValue: current,
			Options: sdk.SessionConfigSelectOptions{Ungrouped: &values},
		},
	}}}, nil
}

func (p *process) SetSessionMode(context.Context, sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	return sdk.SetSessionModeResponse{}, nil
}
