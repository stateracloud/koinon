// SPDX-License-Identifier: Apache-2.0

package acp_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/acp"
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

func (a *fakeAgent) NewSession(context.Context, sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	return sdk.NewSessionResponse{SessionId: "s1"}, nil
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

func (a *fakeAgent) SetSessionConfigOption(context.Context, sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	return sdk.SetSessionConfigOptionResponse{}, nil
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

	It("replays history on load and reports unknown sessions", func() {
		Expect(conn.LoadSession(ctx, "s1", "/sandbox/work")).To(Succeed())
		Expect(rec.text("user_message_chunk")).To(Equal("earlier"))
		Expect(conn.LoadSession(ctx, "nope", "/sandbox/work")).NotTo(Succeed())
	})
})
