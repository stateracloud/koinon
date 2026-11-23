// SPDX-License-Identifier: Apache-2.0

package a2aout_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/a2aedge"
	"github.com/stateracloud/koinon/internal/a2aout"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/internal/tokens"
)

// loopBackend is an in-memory a2aedge.Backend with one thread. The spec plays the agent: it calls
// ask, say and finish where a run would write to the task timeline.
type loopBackend struct {
	mu     sync.Mutex
	task   store.Task
	events []store.TaskEvent
	seen   map[string]bool
	// tasks records the task argument of every Deliver call: an answer must carry the open task.
	tasks []pgtype.UUID
}

var loopTask = pgtype.UUID{Bytes: [16]byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}, Valid: true}

func (b *loopBackend) add(kind, payload string, actor store.TaskActorKind) {
	b.events = append(b.events, store.TaskEvent{Seq: int64(len(b.events) + 1), Kind: kind, Payload: []byte(payload), ActorKind: actor})
}

func (b *loopBackend) Agent(context.Context, a2aedge.Scope) (a2aedge.Agent, error) {
	return a2aedge.Agent{Name: "scout"}, nil
}

func (b *loopBackend) Deliver(_ context.Context, _ a2aedge.Scope, task pgtype.UUID, _ bool, _ int64, mid, text string) (services.DirectMessageResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen[mid] {
		return services.DirectMessageResult{Task: b.task}, nil
	}
	b.seen[mid] = true
	b.tasks = append(b.tasks, task)
	payload, _ := json.Marshal(map[string]string{"text": text, "message_id": mid})
	if !task.Valid {
		b.task = store.Task{ID: loopTask, State: store.TaskStateSubmitted, Title: "t"}
	} else if b.task.State == store.TaskStateInputRequired {
		// An answer to the question: the run wakes and works again.
		b.task.State = store.TaskStateWorking
	}
	b.add("message", string(payload), store.TaskActorKindUser)
	e := b.events[len(b.events)-1]
	return services.DirectMessageResult{Task: b.task, Event: &e, Created: !task.Valid}, nil
}

func (b *loopBackend) Thread(_ context.Context, _ a2aedge.Scope, id pgtype.UUID, after int64) (store.Task, []store.TaskEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id != loopTask {
		return store.Task{}, nil, a2aedge.ErrNotFound
	}
	var out []store.TaskEvent
	for _, e := range b.events {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return b.task, out, nil
}

func (b *loopBackend) Cancel(context.Context, a2aedge.Scope, pgtype.UUID) (store.Task, error) {
	return store.Task{}, a2aedge.ErrRefused
}

// ask parks the run on a question, as ask_user does: a question event, a park event, the state.
func (b *loopBackend) ask(q string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.add("question.asked", `{"question":"`+q+`"}`, store.TaskActorKindAgent)
	b.add("run.parked", `{}`, "")
	b.task.State = store.TaskStateInputRequired
}

// finish writes the last reply and completes the task.
func (b *loopBackend) finish(reply string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	payload, _ := json.Marshal(map[string]string{"text": reply})
	b.add("message", string(payload), store.TaskActorKindAgent)
	b.add("status.changed", `{"to":"completed"}`, "")
	b.task.State = store.TaskStateCompleted
}

// A Koinon agent calls a Koinon edge with the real outbound client. The edge is the same A2A surface
// a kagent agent sees, so this is the part of the kagent exchange that runs without a cluster
// (issue 131). The exchange with a real kagent agent needs kind and stays in test/e2e.
var _ = Describe("Outbound client against the inbound edge", func() {
	const (
		token   = "koinon_test_not-a-real-token"
		agentID = "0a0b0c0d-0000-0000-0000-000000000001"
	)
	var (
		be      *loopBackend
		ca      string
		refused int
		srv     *httptest.Server
		cli     *a2aout.Client
		ctx     = context.Background()
	)

	BeforeEach(func() {
		be = &loopBackend{seen: map[string]bool{}}
		refused = 0
		type ctxKey struct{}
		gate := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+token {
					refused++
					http.Error(w, "no", http.StatusUnauthorized)
					return
				}
				p := tokens.Principal{Role: store.OrgRoleMember, Org: pgtype.UUID{Valid: true}}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
			})
		}
		mux := http.NewServeMux()
		srv = httptest.NewUnstartedServer(mux)
		srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
		srv.StartTLS()
		DeferCleanup(srv.Close)
		a2aedge.New(a2aedge.Options{
			Backend: be, PublicURL: srv.URL, Gate: gate,
			Principal: func(c context.Context) tokens.Principal { p, _ := c.Value(ctxKey{}).(tokens.Principal); return p },
			Wait:      time.Second, Poll: 5 * time.Millisecond,
		}).Mount(mux)

		dir := GinkgoT().TempDir()
		ca = filepath.Join(dir, "ca.pem")
		Expect(os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)).To(Succeed())
		cred := filepath.Join(dir, "token")
		Expect(os.WriteFile(cred, []byte(token+"\n"), 0o600)).To(Succeed())
		o := a2aout.Options{
			Credentials: mcpgateway.StaticCredentials{},
			Target: gateway.MCPTarget{
				Name: "koinon-peer", URL: srv.URL + a2aedge.BasePath + "/agents/" + agentID, CAFile: ca,
				CredentialHeader: "Authorization", CredentialPrefix: "Bearer ", CredentialFile: cred,
			},
			Agent:        gateway.AgentRef{Org: "acme", Name: "caller"},
			Policy:       catalog.URLPolicy{Namespace: "koinon", AllowedHosts: []string{"127.0.0.1"}},
			PollInterval: 5 * time.Millisecond, RequestTimeout: 5 * time.Second,
		}
		pin, err := a2aout.FetchCard(ctx, o)
		Expect(err).NotTo(HaveOccurred())
		o.CardHash = pin.Hash
		cli, err = a2aout.New(ctx, o)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = cli.Close() })
	})

	It("sends a task, sees input-required with the question, answers it, and reads the reply", func() {
		r, err := cli.Send(ctx, a2aout.Request{Text: "deploy the build"})
		Expect(err).NotTo(HaveOccurred())
		Expect(r.TaskID).NotTo(BeEmpty())

		be.ask("which region?")
		r, err = cli.Await(ctx, r.TaskID)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.State).To(Equal(store.TaskStateInputRequired))
		Expect(r.External).To(Equal(a2aedge.State(store.TaskStateInputRequired)))
		Expect(r.Question).To(Equal("which region?"))

		a, err := cli.Send(ctx, a2aout.Request{Text: "eu-west", AnswerTask: r.TaskID, ContextID: r.ContextID})
		Expect(err).NotTo(HaveOccurred())
		Expect(a.TaskID).To(Equal(r.TaskID))
		// The edge routed the answer to the open task: the second message carries it, not a new task.
		Expect(be.tasks).To(HaveLen(2))
		Expect(be.tasks[0].Valid).To(BeFalse())
		Expect(be.tasks[1]).To(Equal(loopTask))

		be.finish("deployed to eu-west")
		done, err := cli.Await(ctx, r.TaskID)
		Expect(err).NotTo(HaveOccurred())
		Expect(done.State).To(Equal(store.TaskStateCompleted))
		Expect(done.Reply).To(Equal("deployed to eu-west"))
	})

	It("refuses a client that has no token", func() {
		o := a2aout.Options{
			Credentials: mcpgateway.StaticCredentials{},
			Target:      gateway.MCPTarget{Name: "koinon-peer", URL: srv.URL + a2aedge.BasePath + "/agents/" + agentID, CAFile: ca},
			Agent:       gateway.AgentRef{Org: "acme", Name: "caller"},
			Policy:      catalog.URLPolicy{Namespace: "koinon", AllowedHosts: []string{"127.0.0.1"}},
		}
		_, err := a2aout.FetchCard(ctx, o)
		Expect(err).To(HaveOccurred())
		// The gate refused the request: the failure is not a TLS error.
		Expect(refused).To(Equal(1))
	})
})
