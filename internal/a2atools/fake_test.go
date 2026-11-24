// SPDX-License-Identifier: Apache-2.0

package a2atools_test

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/a2aout"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// fakeToken is obviously fake, as the gitleaks rule needs.
const fakeToken = "koinon_test_not-a-real-token"

// fakeAgent is an external A2A agent. It answers as its hooks say.
type fakeAgent struct {
	a2asrv.RequestHandler
	mu    sync.Mutex
	sent  []*a2a.SendMessageRequest
	auth  []string
	send  func(*a2a.SendMessageRequest) (a2a.SendMessageResult, error)
	tasks map[a2a.TaskID]*a2a.Task
	// card changes the card that the agent serves; nil serves the default.
	card func(*a2a.AgentCard)
}

func (f *fakeAgent) SendMessage(_ context.Context, r *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	f.mu.Lock()
	f.sent = append(f.sent, r)
	f.mu.Unlock()
	return f.send(r)
}

func (f *fakeAgent) GetTask(_ context.Context, r *a2a.GetTaskRequest) (*a2a.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[r.ID]
	if !ok {
		return nil, a2a.ErrTaskNotFound
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
	f.tasks[r.ID] = &c
	return &c, nil
}

func (f *fakeAgent) SubscribeToTask(context.Context, *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error] {
	Fail("the client must not subscribe")
	return nil
}

func (f *fakeAgent) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeAgent) put(t *a2a.Task) *a2a.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[t.ID] = t
	return t
}

func task(id, ctx string, st a2a.TaskState, msg string) *a2a.Task {
	t := &a2a.Task{ID: a2a.TaskID(id), ContextID: ctx, Status: a2a.TaskStatus{State: st}}
	if msg != "" {
		t.Status.Message = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(msg))
	}
	return t
}

func withReply(t *a2a.Task, text string) *a2a.Task {
	t.Artifacts = []*a2a.Artifact{{ID: "reply", Parts: a2a.ContentParts{a2a.NewTextPart(text)}}}
	return t
}

// The card text of the fake agent. A spec checks that none of it reaches a model.
const (
	cardName  = "Secret Card Name 7f3a"
	cardSkill = "Skill text that must stay with admins 91bc"
)

func card(base string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name: cardName, Version: "1", DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(base+"/jsonrpc", a2a.TransportProtocolJSONRPC)},
		Skills:              []a2a.AgentSkill{{ID: "chat", Name: "Chat", Description: cardSkill, Tags: []string{"chat"}}},
	}
}

// serve runs the fake agent over TLS and returns the remote agent entry that points to it, with the pin
// of the card it serves now.
func serve(f *fakeAgent) (*httptest.Server, gateway.MCPRemoteAgent) {
	GinkgoHelper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("GET /.well-known/agent-card.json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		mut := f.card
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
	DeferCleanup(srv.Close)
	dir := GinkgoT().TempDir()
	ca := filepath.Join(dir, "ca.pem")
	Expect(os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)).To(Succeed())
	tok := filepath.Join(dir, "token")
	Expect(os.WriteFile(tok, []byte(fakeToken+"\n"), 0o600)).To(Succeed())
	ra := gateway.MCPRemoteAgent{
		Name: "kagent", ID: "ra-1", CardURL: srv.URL, CAFile: ca,
		CredentialHeader: "Authorization", CredentialPrefix: "Bearer ", CredentialFile: tok,
	}
	pin, err := a2aout.FetchCard(context.Background(), a2aout.Options{
		Credentials: mcpgateway.StaticCredentials{}, Target: ra.Target("acme"), Agent: gateway.AgentRef{Org: "acme", Name: "scout"},
		Policy: policy(), RequestTimeout: 5 * time.Second, CardHash: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	Expect(err).NotTo(HaveOccurred())
	ra.CardHash = pin.Hash
	return srv, ra
}

func policy() catalog.URLPolicy {
	return catalog.URLPolicy{Namespace: "koinon", AllowedHosts: []string{"127.0.0.1"}}
}
