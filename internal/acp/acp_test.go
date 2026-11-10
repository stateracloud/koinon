// SPDX-License-Identifier: Apache-2.0

package acp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/acp"
)

func TestACP(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "acp")
}

// fakeAgent is an in-process ACP agent: a prompt streams one message chunk, asks for
// permission, reports the answer as a second chunk and ends the turn. A prompt with the text
// "wait" blocks until it is cancelled.
type fakeAgent struct {
	conn *sdk.AgentSideConnection

	mu        sync.Mutex
	cancelled chan struct{}
}

func (a *fakeAgent) Initialize(context.Context, sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return sdk.InitializeResponse{
		ProtocolVersion:   sdk.ProtocolVersionNumber,
		AgentCapabilities: sdk.AgentCapabilities{LoadSession: true},
	}, nil
}

func (a *fakeAgent) NewSession(ctx context.Context, _ sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	err := a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: "s1", Update: sdk.SessionUpdate{
		AvailableCommandsUpdate: &sdk.SessionAvailableCommandsUpdate{
			SessionUpdate:     "available_commands_update",
			AvailableCommands: []sdk.AvailableCommand{{Name: "compact"}, {Name: "koinon:koinon-ready"}},
		},
	}})
	return sdk.NewSessionResponse{SessionId: "s1"}, err
}

func (a *fakeAgent) LoadSession(ctx context.Context, r sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
	if r.SessionId != "s1" {
		return sdk.LoadSessionResponse{}, sdk.NewInvalidParams(map[string]any{"sessionId": "unknown"})
	}
	err := a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: r.SessionId, Update: sdk.UpdateUserMessageText("earlier")})
	return sdk.LoadSessionResponse{}, err
}

func (a *fakeAgent) Prompt(ctx context.Context, r sdk.PromptRequest) (sdk.PromptResponse, error) {
	if r.Prompt[0].Text.Text == "wait" {
		// The SDK also cancels the request's context on session/cancel; either way the turn
		// ends with stopReason "cancelled", as the protocol requires.
		select {
		case <-a.cancelled:
		case <-ctx.Done():
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil
	}
	if err := a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: r.SessionId, Update: sdk.UpdateAgentMessageText("hello ")}); err != nil {
		return sdk.PromptResponse{}, err
	}
	title := "write notes.txt"
	p, err := a.conn.RequestPermission(ctx, sdk.RequestPermissionRequest{
		SessionId: r.SessionId,
		ToolCall:  sdk.ToolCallUpdate{ToolCallId: "t1", Title: &title},
		Options: []sdk.PermissionOption{
			{OptionId: "no", Name: "Reject", Kind: sdk.PermissionOptionKindRejectOnce},
			{OptionId: "yes", Name: "Allow", Kind: sdk.PermissionOptionKindAllowOnce},
		},
	})
	if err != nil {
		return sdk.PromptResponse{}, err
	}
	answer := "cancelled"
	if p.Outcome.Selected != nil {
		answer = string(p.Outcome.Selected.OptionId)
	}
	if err := a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: r.SessionId, Update: sdk.UpdateAgentMessageText(answer)}); err != nil {
		return sdk.PromptResponse{}, err
	}
	return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
}

func (a *fakeAgent) Cancel(context.Context, sdk.CancelNotification) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-a.cancelled:
	default:
		close(a.cancelled)
	}
	return nil
}

func (a *fakeAgent) Authenticate(context.Context, sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	return sdk.AuthenticateResponse{}, nil
}

func (a *fakeAgent) Logout(context.Context, sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	return sdk.LogoutResponse{}, nil
}

func (a *fakeAgent) CloseSession(context.Context, sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	return sdk.CloseSessionResponse{}, nil
}

func (a *fakeAgent) ListSessions(context.Context, sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.ListSessionsResponse{}, nil
}

func (a *fakeAgent) ResumeSession(context.Context, sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	return sdk.ResumeSessionResponse{}, nil
}

// SetSessionConfigOption selects any model except "stubborn", for which it keeps "default",
// and reports no options at all for "silent".
func (a *fakeAgent) SetSessionConfigOption(_ context.Context, r sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	if r.ValueId == nil || r.ValueId.ConfigId != "model" {
		return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]any{"configId": "unknown"})
	}
	current := r.ValueId.Value
	switch current {
	case "silent":
		return sdk.SetSessionConfigOptionResponse{}, nil
	case "stubborn":
		current = "default"
	}
	option := func(id, current string) sdk.SessionConfigOption {
		values := sdk.SessionConfigSelectOptionsUngrouped{{Name: current, Value: sdk.SessionConfigValueId(current)}}
		return sdk.SessionConfigOption{Select: &sdk.SessionConfigOptionSelect{
			Id: sdk.SessionConfigId(id), Name: id, Type: "select", CurrentValue: sdk.SessionConfigValueId(current),
			Options: sdk.SessionConfigSelectOptions{Ungrouped: &values},
		}}
	}
	return sdk.SetSessionConfigOptionResponse{ConfigOptions: []sdk.SessionConfigOption{
		option("mode", "build"), option("model", string(current)),
	}}, nil
}

func (a *fakeAgent) SetSessionMode(context.Context, sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	return sdk.SetSessionModeResponse{}, nil
}

// recorder is a Handler that records updates and answers permissions with a fixed policy.
type recorder struct {
	mu      sync.Mutex
	updates []acp.Update
	kinds   []acp.PermissionOptionKind
}

func (r *recorder) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, acp.Flatten(n))
	return nil
}

func (r *recorder) RequestPermission(_ context.Context, req acp.PermissionRequest) (acp.Decision, error) {
	return acp.Choose(req, r.kinds...), nil
}

func (r *recorder) text(kind string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return acp.Text(r.updates, kind)
}

var _ = Describe("Conn", func() {
	var (
		conn *acp.Conn
		rec  *recorder
		ctx  context.Context
	)

	BeforeEach(func() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		DeferCleanup(cancel)

		toAgentR, toAgentW := io.Pipe()
		fromAgentR, fromAgentW := io.Pipe()
		DeferCleanup(func() {
			_ = toAgentW.Close()
			_ = fromAgentW.Close()
		})
		agent := &fakeAgent{cancelled: make(chan struct{})}
		agent.conn = sdk.NewAgentSideConnection(agent, fromAgentW, toAgentR)
		rec = &recorder{}
		conn = acp.NewConn(rec, toAgentW, fromAgentR)
		conn.SetLogger(slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
	})

	It("initializes, prompts and answers permission requests from the handler", func() {
		init, err := conn.Initialize(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(init.AgentCapabilities.LoadSession).To(BeTrue())

		id, err := conn.NewSession(ctx, "/sandbox/work")
		Expect(err).NotTo(HaveOccurred())

		rec.kinds = []acp.PermissionOptionKind{acp.AllowOnce}
		stop, err := conn.Prompt(ctx, id, "hi")
		Expect(err).NotTo(HaveOccurred())
		Expect(stop).To(Equal(acp.StopEndTurn))
		Expect(rec.text("agent_message_chunk")).To(Equal("hello yes"))
	})

	It("cancels a permission request when no option matches", func() {
		id, err := conn.NewSession(ctx, "/sandbox/work")
		Expect(err).NotTo(HaveOccurred())
		rec.kinds = []acp.PermissionOptionKind{acp.AllowAlways}
		_, err = conn.Prompt(ctx, id, "hi")
		Expect(err).NotTo(HaveOccurred())
		Expect(rec.text("agent_message_chunk")).To(Equal("hello cancelled"))
	})

	It("cancels a running turn", func() {
		id, err := conn.NewSession(ctx, "/sandbox/work")
		Expect(err).NotTo(HaveOccurred())
		done := make(chan acp.StopReason, 1)
		go func() {
			defer GinkgoRecover()
			stop, err := conn.Prompt(ctx, id, "wait")
			Expect(err).NotTo(HaveOccurred())
			done <- stop
		}()
		Consistently(done, 100*time.Millisecond).ShouldNot(Receive())
		Expect(conn.Cancel(ctx, id)).To(Succeed())
		Eventually(done).Should(Receive(Equal(acp.StopCancelled)))
	})

	It("reports the commands the agent announces", func() {
		_, err := conn.NewSession(ctx, "/sandbox/work")
		Expect(err).NotTo(HaveOccurred())
		rec.mu.Lock()
		defer rec.mu.Unlock()
		Expect(rec.updates).To(ContainElement(And(
			HaveField("Kind", "available_commands_update"),
			HaveField("Commands", Equal([]string{"compact", "koinon:koinon-ready"})),
		)))
	})

	It("selects the model and checks the agent's answer", func() {
		id, err := conn.NewSession(ctx, "/sandbox/work")
		Expect(err).NotTo(HaveOccurred())
		Expect(conn.SetModel(ctx, id, "koinon/m-1")).To(Succeed())
		Expect(conn.SetModel(ctx, id, "stubborn")).To(MatchError(ContainSubstring(`did not select model "stubborn"`)))
		Expect(conn.SetModel(ctx, id, "silent")).To(MatchError(ContainSubstring("no \"model\" option")))
	})

	It("replays history on load and reports unknown sessions", func() {
		Expect(conn.LoadSession(ctx, "s1", "/sandbox/work")).To(Succeed())
		Expect(rec.text("user_message_chunk")).To(Equal("earlier"))
		Expect(conn.LoadSession(ctx, "nope", "/sandbox/work")).NotTo(Succeed())
	})
})

var _ = Describe("Conn.WaitUpdates", func() {
	It("returns once the connection ended and every update reached the handler", func(ctx SpecContext) {
		fromAgentR, fromAgentW := io.Pipe()
		rec := &slowRecorder{}
		conn := acp.NewConn(rec, io.Discard, fromAgentR)
		conn.SetLogger(slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
		const n = 50
		go func() {
			defer GinkgoRecover()
			for i := range n {
				_, err := fmt.Fprintf(fromAgentW, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"%d"}}}}`+"\n", i)
				Expect(err).NotTo(HaveOccurred())
			}
			// A response is not counted.
			_, _ = io.WriteString(fromAgentW, `{"jsonrpc":"2.0","id":7,"result":{}}`+"\n")
			_ = fromAgentW.Close()
		}()
		Eventually(conn.Done()).Should(BeClosed())
		Expect(conn.WaitUpdates(ctx)).To(BeTrue())
		Expect(rec.count()).To(Equal(n))
	})
})

// slowRecorder counts updates and takes its time with each, so that updates are still being
// delivered after the connection ended.
type slowRecorder struct {
	mu sync.Mutex
	n  int
}

func (r *slowRecorder) SessionUpdate(context.Context, acp.SessionNotification) error {
	time.Sleep(2 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return nil
}

func (r *slowRecorder) RequestPermission(context.Context, acp.PermissionRequest) (acp.Decision, error) {
	return acp.Decision{}, nil
}

func (r *slowRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// observer records the order in which updates are read off the stream and handed to the handler.
type observer struct {
	mu     sync.Mutex
	events []string
}

func (o *observer) add(e string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, e)
}

func (o *observer) UpdateRead(line []byte) {
	var m struct {
		Params struct {
			Update struct {
				Content struct{ Text string } `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	Expect(json.Unmarshal(line, &m)).To(Succeed())
	o.add("read " + m.Params.Update.Content.Text)
}

func (o *observer) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	o.add("handled " + acp.Flatten(n).Text)
	return nil
}

func (o *observer) RequestPermission(context.Context, acp.PermissionRequest) (acp.Decision, error) {
	return acp.Decision{}, nil
}

func (o *observer) got() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.events)
}

var _ = Describe("a ReadObserver handler", func() {
	It("sees each update as it is read, before the handler gets it, and nothing else", func(ctx SpecContext) {
		fromAgentR, fromAgentW := io.Pipe()
		obs := &observer{}
		conn := acp.NewConn(obs, io.Discard, fromAgentR)
		go func() {
			defer GinkgoRecover()
			for i := range 2 {
				_, err := fmt.Fprintf(fromAgentW, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"u%d"}}}}`+"\n", i)
				Expect(err).NotTo(HaveOccurred())
			}
			_, _ = io.WriteString(fromAgentW, `{"jsonrpc":"2.0","id":7,"result":{}}`+"\n")
			_ = fromAgentW.Close()
		}()
		Eventually(conn.Done()).Should(BeClosed())
		Expect(conn.WaitUpdates(ctx)).To(BeTrue())
		ev := obs.got()
		Expect(ev).To(HaveLen(4), "the response is not an update")
		for _, u := range []string{"u0", "u1"} {
			read, handled := slices.Index(ev, "read "+u), slices.Index(ev, "handled "+u)
			Expect(read).To(BeNumerically(">=", 0), u)
			Expect(handled).To(BeNumerically(">", read), u)
		}
	})
})

var _ = Describe("a burst of updates against a slow handler", func() {
	It("holds the agent's stream back instead of overflowing the SDK's queue", func(ctx SpecContext) {
		fromAgentR, fromAgentW := io.Pipe()
		rec := &slowRecorder{}
		conn := acp.NewConn(rec, io.Discard, fromAgentR)
		conn.SetLogger(slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
		// Three times the SDK's queue of 1024. slowRecorder takes its time with each update, so
		// without backpressure the reader gets ahead and the SDK closes the connection.
		const n = 3000
		go func() {
			defer GinkgoRecover()
			for i := range n {
				_, err := fmt.Fprintf(fromAgentW, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"%d"}}}}`+"\n", i)
				if err != nil {
					return
				}
			}
			_ = fromAgentW.Close()
		}()
		Eventually(conn.Done()).WithTimeout(60 * time.Second).Should(BeClosed())
		Expect(conn.WaitUpdates(ctx)).To(BeTrue())
		Expect(rec.count()).To(Equal(n), "every update reaches the handler")
	}, SpecTimeout(90*time.Second))
})

var _ = Describe("session/update lines the SDK rejects", func() {
	It("do not count as unhandled, so the stream is never held back for them", func(ctx SpecContext) {
		fromAgentR, fromAgentW := io.Pipe()
		rec := &slowRecorder{}
		conn := acp.NewConn(rec, io.Discard, fromAgentR)
		conn.SetLogger(slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
		// More than the 512 lag limit of lines whose params the SDK refuses (InvalidParams),
		// so the handler never sees them; then valid ones.
		const bad, good = 1500, 20
		go func() {
			defer GinkgoRecover()
			for range bad {
				if _, err := io.WriteString(fromAgentW, `{"jsonrpc":"2.0","method":"session/update","params":"not an object"}`+"\n"); err != nil {
					return
				}
			}
			for i := range good {
				if _, err := fmt.Fprintf(fromAgentW, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"%d"}}}}`+"\n", i); err != nil {
					return
				}
			}
			_ = fromAgentW.Close()
		}()
		Eventually(conn.Done()).WithTimeout(20 * time.Second).Should(BeClosed())
		Expect(conn.WaitUpdates(ctx)).To(BeTrue())
		Expect(rec.count()).To(Equal(good))
	}, SpecTimeout(40*time.Second))
})
