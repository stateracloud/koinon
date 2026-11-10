// SPDX-License-Identifier: Apache-2.0

package acp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
		conn = acp.NewConn(context.Background(), rec, toAgentW, fromAgentR)
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
		conn := acp.NewConn(context.Background(), rec, io.Discard, fromAgentR)
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
		conn := acp.NewConn(context.Background(), obs, io.Discard, fromAgentR)
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
		conn := acp.NewConn(context.Background(), rec, io.Discard, fromAgentR)
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

// stallLogger is a slog handler whose records wait until release is closed. The SDK logs every
// notification it rejects from its dispatching goroutine, so a stalled logger stalls the
// dispatcher at a point the test controls, instead of one that depends on scheduling.
type stallLogger struct{ release chan struct{} }

func (l stallLogger) Enabled(context.Context, slog.Level) bool { return true }
func (l stallLogger) Handle(context.Context, slog.Record) error {
	<-l.release
	return nil
}
func (l stallLogger) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l stallLogger) WithGroup(string) slog.Handler      { return l }

var _ = Describe("notifications that no handler acts on", func() {
	// The SDK queues 1024 notifications for one dispatcher and closes the connection when a
	// burst overflows it. Here the dispatcher is stopped for the whole burst, so nothing the
	// reader passes on is dispatched: the burst overflows the queue unless the reader keeps
	// these lines out of it (issue 501: the same happened by chance on a loaded CI runner).
	flood := func(ctx SpecContext, line string, bad int) {
		fromAgentR, fromAgentW := io.Pipe()
		rec := &slowRecorder{}
		conn := acp.NewConn(context.Background(), rec, io.Discard, fromAgentR)
		release := make(chan struct{})
		var once sync.Once
		unstall := func() { once.Do(func() { close(release) }) }
		DeferCleanup(unstall)
		conn.SetLogger(slog.New(stallLogger{release: release}))
		const good = 20
		written := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(written)
			for range bad {
				if _, err := io.WriteString(fromAgentW, line); err != nil {
					return
				}
			}
			for i := range good {
				if _, err := io.WriteString(fromAgentW, updateLine(fmt.Sprint(i))); err != nil {
					return
				}
			}
		}()
		// The reader takes every line while the dispatcher is stopped: the lines that
		// reach the handler (the valid ones) are far below the SDK's queue.
		Eventually(written).WithTimeout(20 * time.Second).Should(BeClosed())
		Expect(conn.Dropped()).To(BeEquivalentTo(bad))
		Expect(conn.Done()).NotTo(BeClosed(), "the SDK's queue did not overflow")
		unstall()
		_ = fromAgentW.Close()
		Eventually(conn.Done()).WithTimeout(20 * time.Second).Should(BeClosed())
		Expect(conn.WaitUpdates(ctx)).To(BeTrue())
		Expect(rec.count()).To(Equal(good))
	}

	It("session/update lines the SDK rejects never reach its queue", func(ctx SpecContext) {
		flood(ctx, `{"jsonrpc":"2.0","method":"session/update","params":"not an object"}`+"\n", 3000)
	}, SpecTimeout(60*time.Second))

	It("unknown and extension methods never reach its queue", func(ctx SpecContext) {
		flood(ctx, `{"jsonrpc":"2.0","method":"_ext/ping","params":{}}`+"\n", 3000)
	}, SpecTimeout(60*time.Second))
})

// updateLine is a valid session/update notification with the given text.
func updateLine(text string) string {
	return `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + text + `"}}}}` + "\n"
}

// gate is a handler that holds every update until release is closed.
type gate struct {
	slowRecorder
	release chan struct{}
}

func (g *gate) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return g.slowRecorder.SessionUpdate(ctx, n)
}

var _ = Describe("a reader held back by a handler that does not return", func() {
	// 700 updates against a handler that is stuck: the reader stops at the lag limit. The
	// connection must still be releasable from outside, since the SDK's own Done is closed by
	// the goroutine that is held.
	run := func(release func(cancel context.CancelFunc, conn *acp.Conn)) {
		ctx, cancel := context.WithCancel(context.Background())
		DeferCleanup(cancel)
		fromAgentR, fromAgentW := io.Pipe()
		g := &gate{release: make(chan struct{})}
		DeferCleanup(func() { close(g.release) })
		conn := acp.NewConn(ctx, g, io.Discard, fromAgentR)
		conn.SetLogger(slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
		var written atomic.Int32
		go func() {
			for i := range 700 {
				if _, err := io.WriteString(fromAgentW, updateLine(fmt.Sprint(i))); err != nil {
					return
				}
				written.Add(1)
			}
		}()
		Eventually(written.Load).Should(BeNumerically(">=", 512))
		Consistently(written.Load, "300ms").Should(BeNumerically("<", 700), "the reader is held back")
		release(cancel, conn)
		Eventually(written.Load).Should(BeNumerically("==", 700))
		_ = fromAgentW.Close()
		Eventually(conn.Done()).WithTimeout(6 * time.Second).Should(BeClosed())
	}

	It("is freed when the context ends", func() {
		run(func(cancel context.CancelFunc, _ *acp.Conn) { cancel() })
	})

	It("is freed by Close", func() {
		run(func(_ context.CancelFunc, conn *acp.Conn) { conn.Close() })
	})
})

var _ = Describe("what counts as a notification", func() {
	It("counts a session/update with a null id, which the SDK treats as a notification", func(ctx SpecContext) {
		fromAgentR, fromAgentW := io.Pipe()
		rec := &slowRecorder{}
		conn := acp.NewConn(context.Background(), rec, io.Discard, fromAgentR)
		conn.SetLogger(slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
		const n = 3000
		go func() {
			for i := range n {
				line := strings.Replace(updateLine(fmt.Sprint(i)), `"jsonrpc":"2.0",`, `"jsonrpc":"2.0","id":null,`, 1)
				if _, err := io.WriteString(fromAgentW, line); err != nil {
					return
				}
			}
			_ = fromAgentW.Close()
		}()
		Eventually(conn.Done()).WithTimeout(60 * time.Second).Should(BeClosed())
		Expect(conn.WaitUpdates(ctx)).To(BeTrue())
		Expect(rec.count()).To(Equal(n))
	}, SpecTimeout(90*time.Second))

	It("does not count a line the SDK cannot decode", func(ctx SpecContext) {
		fromAgentR, fromAgentW := io.Pipe()
		rec := &slowRecorder{}
		conn := acp.NewConn(context.Background(), rec, io.Discard, fromAgentR)
		conn.SetLogger(slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
		const bad, good = 1500, 20
		go func() {
			for range bad {
				// jsonrpc is not a string: the SDK drops the line.
				line := strings.Replace(updateLine("x"), `"jsonrpc":"2.0"`, `"jsonrpc":2`, 1)
				if _, err := io.WriteString(fromAgentW, line); err != nil {
					return
				}
			}
			for i := range good {
				if _, err := io.WriteString(fromAgentW, updateLine(fmt.Sprint(i))); err != nil {
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

var _ = Describe("a burst of mixed notifications against a slow handler", func() {
	It("holds the stream back for every notification the SDK queues, not only updates", func(ctx SpecContext) {
		fromAgentR, fromAgentW := io.Pipe()
		rec := &slowRecorder{}
		conn := acp.NewConn(context.Background(), rec, io.Discard, fromAgentR)
		conn.SetLogger(slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
		// 1000 updates, each followed by three extension or unknown notifications: 4000 queued
		// notifications, but never more than 512 updates in flight.
		const n = 1000
		go func() {
			for i := range n {
				if _, err := io.WriteString(fromAgentW, updateLine(fmt.Sprint(i))); err != nil {
					return
				}
				for _, m := range []string{"_ext/one", "_ext/two", "unknown/three"} {
					if _, err := io.WriteString(fromAgentW, `{"jsonrpc":"2.0","method":"`+m+`","params":{}}`+"\n"); err != nil {
						return
					}
				}
			}
			_ = fromAgentW.Close()
		}()
		Eventually(conn.Done()).WithTimeout(60 * time.Second).Should(BeClosed())
		Expect(conn.WaitUpdates(ctx)).To(BeTrue())
		Expect(rec.count()).To(Equal(n))
	}, SpecTimeout(90*time.Second))
})

var _ = Describe("a line without a newline", func() {
	It("ends the connection at the SDK's size limit instead of buffering without bound", func() {
		fromAgentR, fromAgentW := io.Pipe()
		conn := acp.NewConn(context.Background(), &slowRecorder{}, io.Discard, fromAgentR)
		conn.SetLogger(slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
		go func() {
			chunk := []byte(strings.Repeat("a", 1<<20))
			for range 12 {
				if _, err := fromAgentW.Write(chunk); err != nil {
					return
				}
			}
		}()
		DeferCleanup(fromAgentW.Close)
		Eventually(conn.Done()).WithTimeout(10 * time.Second).Should(BeClosed())
		Expect(conn.ReadErr()).To(MatchError(acp.ErrLineTooLong))
	})

	It("passes a line of 10 MiB minus the newline's byte to the handler", func(ctx SpecContext) {
		fromAgentR, fromAgentW := io.Pipe()
		rec := &lenRecorder{}
		conn := acp.NewConn(context.Background(), rec, io.Discard, fromAgentR)
		conn.SetLogger(slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
		const limit = 10 << 20
		pad := limit - 1 - len(strings.TrimSuffix(updateLine(""), "\n"))
		go func() {
			_, _ = io.WriteString(fromAgentW, updateLine(strings.Repeat("a", pad)))
			_ = fromAgentW.Close()
		}()
		Eventually(conn.Done()).WithTimeout(20 * time.Second).Should(BeClosed())
		Expect(conn.WaitUpdates(ctx)).To(BeTrue())
		Expect(conn.ReadErr()).To(BeNil())
		Expect(rec.n.Load()).To(BeEquivalentTo(pad))
	}, SpecTimeout(40*time.Second))
})

// lenRecorder remembers the length of the last update's text.
type lenRecorder struct{ n atomic.Int64 }

func (r *lenRecorder) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	r.n.Store(int64(len(acp.Flatten(n).Text)))
	return nil
}

func (r *lenRecorder) RequestPermission(context.Context, acp.PermissionRequest) (acp.Decision, error) {
	return acp.Decision{}, nil
}
