// SPDX-License-Identifier: Apache-2.0

package a2aout_test

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/a2aout"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
)

const fakeToken = "koinon_test_not-a-real-token"

// fakeAgent is an external A2A agent. It answers as its hooks say. The methods it does not
// override stay nil: a call to one panics, which fails the spec.
type fakeAgent struct {
	a2asrv.RequestHandler
	mu    sync.Mutex
	sent  []*a2a.SendMessageRequest
	auth  []string
	send  func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error)
	tasks map[a2a.TaskID]*a2a.Task
	gets  atomic.Int32
	// afterGets turns the task to completed after that many reads.
	afterGets int32
}

func (f *fakeAgent) SendMessage(_ context.Context, r *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	f.mu.Lock()
	f.sent = append(f.sent, r)
	f.mu.Unlock()
	return f.send(r)
}

func (f *fakeAgent) GetTask(_ context.Context, r *a2a.GetTaskRequest) (*a2a.Task, error) {
	n := f.gets.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[r.ID]
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	if f.afterGets > 0 && n >= f.afterGets {
		c := *t
		c.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
		c.Artifacts = []*a2a.Artifact{{ID: "reply", Parts: a2a.ContentParts{a2a.NewTextPart("all done")}}}
		return &c, nil
	}
	return t, nil
}

func (f *fakeAgent) CancelTask(_ context.Context, r *a2a.CancelTaskRequest) (*a2a.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[r.ID]
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	if t.Status.State.Terminal() {
		return nil, a2a.ErrTaskNotCancelable
	}
	c := *t
	c.Status = a2a.TaskStatus{State: a2a.TaskStateCanceled}
	return &c, nil
}

func (f *fakeAgent) SubscribeToTask(context.Context, *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	Fail("the client must not subscribe")
	return nil
}

func task(id, ctx string, st a2a.TaskState, msg string) *a2a.Task {
	t := &a2a.Task{ID: a2a.TaskID(id), ContextID: ctx, Status: a2a.TaskStatus{State: st}}
	if msg != "" {
		t.Status.Message = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(msg))
	}
	return t
}

func card(base string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name: "kagent-fake", Version: "1", DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(base+"/jsonrpc", a2a.TransportProtocolJSONRPC)},
		Skills:              []a2a.AgentSkill{{ID: "chat", Name: "Chat", Description: "Chat", Tags: []string{"chat"}}},
	}
}

// serve runs the fake agent over TLS (a credential goes over https only).
func serve(f *fakeAgent, mut func(*a2a.AgentCard)) *httptest.Server {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		c := card(srv.URL)
		if mut != nil {
			mut(c)
		}
		body, err := a2av0.NewStaticAgentCardProducer(c).(a2asrv.AgentCardJSONProducer).CardJSON(r.Context())
		Expect(err).NotTo(HaveOccurred())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	rpc := a2asrv.NewJSONRPCHandler(f)
	mux.HandleFunc("POST /jsonrpc", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		rpc.ServeHTTP(w, r)
	})
	srv = httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	return srv
}

var _ = Describe("Client", func() {
	var (
		f       *fakeAgent
		srv     *httptest.Server
		dir     string
		target  gateway.MCPTarget
		policy  catalog.URLPolicy
		bgctx   = context.Background()
		connect = func(mod ...func(*a2aout.Options)) (*a2aout.Client, error) {
			o := a2aout.Options{
				Target: target, Agent: gateway.AgentRef{Org: "acme", Name: "scout"}, Policy: policy,
				PollInterval: 5 * time.Millisecond, RequestTimeout: 5 * time.Second,
			}
			for _, m := range mod {
				m(&o)
			}
			return a2aout.New(bgctx, o)
		}
	)

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		f = &fakeAgent{tasks: map[a2a.TaskID]*a2a.Task{}}
		f.send = func(r *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
			return task("t1", "c1", a2a.TaskStateWorking, ""), nil
		}
		policy = catalog.URLPolicy{Namespace: "koinon", AllowedHosts: []string{"127.0.0.1"}}
	})
	start := func(mut func(*a2a.AgentCard)) {
		srv = serve(f, mut)
		DeferCleanup(srv.Close)
		caFile := filepath.Join(dir, "ca.pem")
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
		Expect(os.WriteFile(caFile, pemBytes, 0o600)).To(Succeed())
		target = gateway.MCPTarget{Name: "kagent", URL: srv.URL, CAFile: caFile}
	}
	withCredential := func() {
		file := filepath.Join(dir, "token")
		Expect(os.WriteFile(file, []byte(fakeToken+"\n"), 0o600)).To(Succeed())
		target.CredentialHeader, target.CredentialPrefix, target.CredentialFile = "Authorization", "Bearer ", file
	}

	Describe("sending a turn", func() {
		BeforeEach(func() { start(nil) })

		It("starts a thread and waits until the task is completed", func() {
			f.tasks["t1"] = task("t1", "c1", a2a.TaskStateWorking, "")
			f.afterGets = 2
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			r, err := c.Send(bgctx, a2aout.Request{Text: "summarise", ReturnImmediately: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.State).To(Equal(store.TaskStateWorking))
			Expect(r.Settled()).To(BeFalse())
			Expect(f.sent[0].Message.TaskID).To(BeEmpty())
			Expect(f.sent[0].Message.ContextID).To(BeEmpty())

			done, err := c.Await(bgctx, r.TaskID)
			Expect(err).NotTo(HaveOccurred())
			Expect(done.State).To(Equal(store.TaskStateCompleted))
			Expect(done.Reply).To(Equal("all done"))
		})

		It("shows input-required with the question", func() {
			f.send = func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
				return task("t1", "c1", a2a.TaskStateInputRequired, "Which region?"), nil
			}
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			r, err := c.Send(bgctx, a2aout.Request{Text: "deploy"})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.State).To(Equal(store.TaskStateInputRequired))
			Expect(r.Question).To(Equal("Which region?"))
			Expect(r.Settled()).To(BeTrue())
		})

		It("maps an answer that is only a message to a completed turn", func() {
			f.send = func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
				m := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("42"))
				m.ContextID = "c9"
				return m, nil
			}
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			r, err := c.Send(bgctx, a2aout.Request{Text: "answer?"})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.State).To(Equal(store.TaskStateCompleted))
			Expect(r.TaskID).To(BeEmpty())
			Expect(r.Reply).To(Equal("42"))
			Expect(r.ContextID).To(Equal("c9"))
		})

		It("maps rejected and auth-required with a reason", func() {
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			for st, want := range map[a2a.TaskState]struct {
				s store.TaskState
				r a2aout.Reason
			}{
				a2a.TaskStateRejected:     {store.TaskStateFailed, a2aout.ReasonRejected},
				a2a.TaskStateAuthRequired: {store.TaskStateInputRequired, a2aout.ReasonAuthRequired},
			} {
				f.send = func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
					return task("t1", "c1", st, ""), nil
				}
				r, err := c.Send(bgctx, a2aout.Request{Text: "x"})
				Expect(err).NotTo(HaveOccurred())
				Expect(r.State).To(Equal(want.s), string(st))
				Expect(r.Reason).To(Equal(want.r), string(st))
				Expect(r.External).To(Equal(st))
			}
		})

		It("refuses an answer with a state the client does not know", func() {
			f.send = func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
				return task("t1", "c1", "TASK_STATE_SLEEPING", ""), nil
			}
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			_, err = c.Send(bgctx, a2aout.Request{Text: "x"})
			Expect(err).To(HaveOccurred())
		})

		It("refuses a request that is not well formed", func() {
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			for _, r := range []a2aout.Request{
				{Text: "  "},
				{Text: "x", After: "t1"},
				{Text: "x", After: "t1", AnswerTask: "t2", ContextID: "c1"},
			} {
				_, err := c.Send(bgctx, r)
				Expect(errors.Is(err, a2aout.ErrBadRequest)).To(BeTrue(), "%+v", r)
			}
			Expect(f.sent).To(BeEmpty())
		})
	})

	Describe("the A2A 1.0.0 task rules", func() {
		BeforeEach(func() { start(nil) })

		It("answers a task that waits for input, in the task and the context of the task", func() {
			f.tasks["t1"] = task("t1", "c1", a2a.TaskStateInputRequired, "Which region?")
			f.send = func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
				return task("t1", "c1", a2a.TaskStateCompleted, ""), nil
			}
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			r, err := c.Send(bgctx, a2aout.Request{Text: "eu-west", AnswerTask: "t1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.State).To(Equal(store.TaskStateCompleted))
			Expect(string(f.sent[0].Message.TaskID)).To(Equal("t1"))
			Expect(f.sent[0].Message.ContextID).To(Equal("c1"))
		})

		It("never sends a message to a task that ended", func() {
			for _, st := range []a2a.TaskState{a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected} {
				f.tasks["t1"] = task("t1", "c1", st, "")
				c, err := connect()
				Expect(err).NotTo(HaveOccurred())
				_, err = c.Send(bgctx, a2aout.Request{Text: "more", AnswerTask: "t1"})
				Expect(errors.Is(err, a2aout.ErrTaskEnded)).To(BeTrue(), string(st))
			}
			Expect(f.sent).To(BeEmpty())
		})

		It("does not answer a task that is working", func() {
			f.tasks["t1"] = task("t1", "c1", a2a.TaskStateWorking, "")
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			_, err = c.Send(bgctx, a2aout.Request{Text: "more", AnswerTask: "t1"})
			Expect(errors.Is(err, a2aout.ErrNotAwaitingInput)).To(BeTrue())
			Expect(f.sent).To(BeEmpty())
		})

		It("sends a follow-up as a new task in the same context with referenceTaskIds", func() {
			f.send = func(r *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
				return task("t2", "c1", a2a.TaskStateCompleted, ""), nil
			}
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			r, err := c.Send(bgctx, a2aout.Request{Text: "and again", ContextID: "c1", After: "t1"})
			Expect(err).NotTo(HaveOccurred())
			Expect(string(r.TaskID)).To(Equal("t2"))
			m := f.sent[0].Message
			Expect(m.TaskID).To(BeEmpty())
			Expect(m.ContextID).To(Equal("c1"))
			Expect(m.ReferenceTasks).To(Equal([]a2a.TaskID{"t1"}))
		})

		It("refuses an agent that reuses the ID of the task it follows", func() {
			f.send = func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
				return task("t1", "c1", a2a.TaskStateCompleted, ""), nil
			}
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			_, err = c.Send(bgctx, a2aout.Request{Text: "again", ContextID: "c1", After: "t1"})
			Expect(errors.Is(err, a2aout.ErrTaskIDReused)).To(BeTrue())
		})

		It("refuses an answer in another context or for another task", func() {
			f.send = func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
				return task("t2", "c2", a2a.TaskStateWorking, ""), nil
			}
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			_, err = c.Send(bgctx, a2aout.Request{Text: "x", ContextID: "c1"})
			Expect(errors.Is(err, a2aout.ErrContextMismatch)).To(BeTrue())

			f.tasks["t1"] = task("t1", "c1", a2a.TaskStateInputRequired, "?")
			_, err = c.Send(bgctx, a2aout.Request{Text: "x", AnswerTask: "t1"})
			Expect(errors.Is(err, a2aout.ErrTaskMismatch)).To(BeTrue())
		})

		It("keeps the A2A errors of the agent for errors.Is", func() {
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			_, err = c.Get(bgctx, "nope")
			Expect(errors.Is(err, a2a.ErrTaskNotFound)).To(BeTrue())
			f.tasks["t1"] = task("t1", "c1", a2a.TaskStateCompleted, "")
			_, err = c.Cancel(bgctx, "t1")
			Expect(errors.Is(err, a2a.ErrTaskNotCancelable)).To(BeTrue())
		})

		It("cancels a task that runs", func() {
			f.tasks["t1"] = task("t1", "c1", a2a.TaskStateWorking, "")
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			r, err := c.Cancel(bgctx, "t1")
			Expect(err).NotTo(HaveOccurred())
			Expect(r.State).To(Equal(store.TaskStateCanceled))
		})

		It("stops waiting when the context ends, and never subscribes", func() {
			f.tasks["t1"] = task("t1", "c1", a2a.TaskStateWorking, "")
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			ctx, cancel := context.WithTimeout(bgctx, 60*time.Millisecond)
			defer cancel()
			_, err = c.Await(ctx, "t1")
			Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue())
		})
	})

	Describe("the egress and credential rules (ADR 0120)", func() {
		It("sets the credential from the gateway side, on every request, and never shows it", func() {
			start(nil)
			withCredential()
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			_, err = c.Send(bgctx, a2aout.Request{Text: "x"})
			Expect(err).NotTo(HaveOccurred())
			Expect(f.auth).To(HaveLen(2)) // the card and the call
			for _, a := range f.auth {
				Expect(a).To(Equal("Bearer " + fakeToken))
			}
		})

		It("does not put the credential in an error", func() {
			start(nil)
			withCredential()
			f.send = func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error) { return nil, errors.New("boom") }
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			_, err = c.Send(bgctx, a2aout.Request{Text: "x"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring(fakeToken))
		})

		It("refuses a card that names another host, so the credential cannot be moved", func() {
			other := httptest.NewTLSServer(http.NotFoundHandler())
			DeferCleanup(other.Close)
			start(func(c *a2a.AgentCard) {
				c.SupportedInterfaces = []*a2a.AgentInterface{a2a.NewAgentInterface(other.URL+"/jsonrpc", a2a.TransportProtocolJSONRPC)}
			})
			withCredential()
			_, err := connect()
			Expect(errors.Is(err, a2aout.ErrCardHost)).To(BeTrue())
		})

		It("sends no credential over plain http", func() {
			plain := httptest.NewServer(http.NotFoundHandler())
			DeferCleanup(plain.Close)
			start(nil)
			target.URL, target.CAFile = plain.URL, ""
			withCredential()
			_, err := connect()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring(fakeToken))
			Expect(f.auth).To(BeEmpty())
		})

		It("does not follow a redirect of the card", func() {
			var hit atomic.Bool
			second := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
			DeferCleanup(second.Close)
			start(nil)
			redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, second.URL+r.URL.Path, http.StatusFound)
			}))
			DeferCleanup(redirect.Close)
			pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redirect.Certificate().Raw})
			Expect(os.WriteFile(target.CAFile, pemBytes, 0o600)).To(Succeed())
			target.URL = redirect.URL
			_, err := connect()
			Expect(errors.Is(err, a2aout.ErrCard)).To(BeTrue())
			Expect(hit.Load()).To(BeFalse())
		})

		It("refuses a target the URL policy denies, before any request", func() {
			start(nil)
			policy = catalog.URLPolicy{Namespace: "koinon"}
			_, err := connect()
			Expect(errors.Is(err, a2aout.ErrEgressDenied)).To(BeTrue())
			Expect(f.auth).To(BeEmpty())
		})

		It("checks the address again at dial time (DNS rebinding)", func() {
			start(nil)
			var n atomic.Int32
			policy = catalog.URLPolicy{
				Namespace: "koinon",
				Resolver: func(context.Context, string) ([]netip.Addr, error) {
					if n.Add(1) == 1 {
						return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil // public: the check passes
					}
					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil // the dial finds loopback
				},
			}
			target.URL = "https://agent.example.com:" + strings.Split(srv.URL, ":")[2]
			target.CAFile = ""
			_, err := connect()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("egress policy denies"))
			Expect(f.auth).To(BeEmpty())
		})

		It("refuses a response over the size limit", func() {
			start(nil)
			f.send = func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
				return task("t1", "c1", a2a.TaskStateInputRequired, strings.Repeat("a", 8192)), nil
			}
			c, err := connect(func(o *a2aout.Options) { o.MaxBody = 4096 })
			Expect(err).NotTo(HaveOccurred())
			_, err = c.Send(bgctx, a2aout.Request{Text: "x"})
			Expect(err).To(HaveOccurred())
		})

		It("cuts a long reply at MaxText", func() {
			start(nil)
			f.send = func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
				return task("t1", "c1", a2a.TaskStateInputRequired, strings.Repeat("é", a2aout.MaxText)), nil
			}
			c, err := connect(func(o *a2aout.Options) { o.MaxBody = 1 << 20 })
			Expect(err).NotTo(HaveOccurred())
			r, err := c.Send(bgctx, a2aout.Request{Text: "x"})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(r.Question)).To(BeNumerically("<=", a2aout.MaxText))
			Expect(strings.ToValidUTF8(r.Question, "")).To(Equal(r.Question))
		})
	})

	Describe("the agent card", func() {
		It("reads a v0.3 card and speaks v0.3 JSON-RPC (kagent)", func() {
			legacy := &fakeAgent{tasks: map[a2a.TaskID]*a2a.Task{}}
			legacy.send = func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
				return task("t1", "c1", a2a.TaskStateInputRequired, "Which region?"), nil
			}
			mux := http.NewServeMux()
			var s *httptest.Server
			mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
				c := card(s.URL)
				c.SupportedInterfaces[0].ProtocolVersion = a2av0.Version
				body, err := a2av0.NewStaticAgentCardProducer(c).(a2asrv.AgentCardJSONProducer).CardJSON(r.Context())
				Expect(err).NotTo(HaveOccurred())
				_, _ = w.Write(body)
			})
			mux.Handle("POST /jsonrpc", a2av0.NewJSONRPCHandler(legacy))
			s = httptest.NewTLSServer(mux)
			DeferCleanup(s.Close)
			caFile := filepath.Join(dir, "legacy.pem")
			Expect(os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0o600)).To(Succeed())
			target = gateway.MCPTarget{Name: "kagent", URL: s.URL, CAFile: caFile}
			c, err := connect()
			Expect(err).NotTo(HaveOccurred())
			r, err := c.Send(bgctx, a2aout.Request{Text: "deploy"})
			Expect(err).NotTo(HaveOccurred())
			Expect(r.State).To(Equal(store.TaskStateInputRequired))
			Expect(r.Question).To(Equal("Which region?"))
		})
	})
})
