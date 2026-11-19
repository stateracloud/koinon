// SPDX-License-Identifier: Apache-2.0

package a2aedge_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/a2aedge"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/tokens"
)

var _ = Describe("Server", func() {
	var (
		f    *fake
		srv  *httptest.Server
		role store.OrgRole
	)
	const agentID = "0a0b0c0d-0000-0000-0000-000000000001"

	BeforeEach(func() {
		f = newFake()
		role = store.OrgRoleMember
		// The gate stands in for the admin API token check: it sets a principal.
		gate := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer koinon_test_not-a-real-token" {
					http.Error(w, "no", http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, tokens.Principal{Role: role, Org: pgtype.UUID{Valid: true}})))
			})
		}
		s := a2aedge.New(a2aedge.Options{
			Backend: f, PublicURL: "https://koinon.example/", Gate: gate,
			Principal: func(ctx context.Context) tokens.Principal { p, _ := ctx.Value(ctxKey{}).(tokens.Principal); return p },
			Wait:      time.Second, Poll: 5 * time.Millisecond,
		})
		mux := http.NewServeMux()
		s.Mount(mux)
		srv = httptest.NewServer(mux)
		DeferCleanup(srv.Close)
	})

	do := func(method, path, token, body string) (int, string) {
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	const tok = "koinon_test_not-a-real-token"

	It("refuses a call without a token", func() {
		code, _ := do("GET", "/a2a/v1/agents/"+agentID+"/.well-known/agent-card.json", "", "")
		Expect(code).To(Equal(http.StatusUnauthorized))
	})

	It("refuses a token below the member role", func() {
		role = store.OrgRoleViewer
		code, _ := do("GET", "/a2a/v1/agents/"+agentID+"/.well-known/agent-card.json", tok, "")
		Expect(code).To(Equal(http.StatusForbidden))
	})

	It("answers not found for a path that is not an agent ID", func() {
		code, _ := do("GET", "/a2a/v1/agents/not-a-uuid/.well-known/agent-card.json", tok, "")
		Expect(code).To(Equal(http.StatusNotFound))
	})

	It("serves a private card with both bindings and a bearer scheme", func() {
		code, body := do("GET", "/a2a/v1/agents/"+agentID+"/.well-known/agent-card.json", tok, "")
		Expect(code).To(Equal(http.StatusOK))
		var card map[string]any
		Expect(json.Unmarshal([]byte(body), &card)).To(Succeed())
		Expect(card["name"]).To(Equal("scout"))
		Expect(body).To(ContainSubstring("https://koinon.example/a2a/v1/agents/" + agentID + "/jsonrpc"))
		Expect(body).To(ContainSubstring("v0.3/jsonrpc"))
		Expect(strings.ToLower(body)).To(ContainSubstring("bearer"))
	})

	It("runs a v1.0 JSON-RPC round trip through input-required", func() {
		go func() {
			time.Sleep(30 * time.Millisecond)
			f.mu.Lock()
			f.add("question.asked", `{"question":"which region?"}`).ActorKind = store.TaskActorKindAgent
			f.mu.Unlock()
			f.move(store.TaskStateInputRequired)
		}()
		req := `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"deploy"}]}}}`
		code, body := do("POST", "/a2a/v1/agents/"+agentID+"/jsonrpc", tok, req)
		Expect(code).To(Equal(http.StatusOK))
		Expect(body).To(ContainSubstring("TASK_STATE_INPUT_REQUIRED"))
		Expect(body).To(ContainSubstring(taskID))
	})
})

type ctxKey struct{}
