// SPDX-License-Identifier: Apache-2.0

package mcpgateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/mcpgateway"
)

// fakeActors stands in for the Substrate identifier of the MCP audience: it knows one token per actor.
type fakeActors map[string]actortoken.Actor

const (
	actorJWT       = "koinon_test_not-a-real-actor-jwt"
	unknownJWT     = "koinon_test_not-a-real-unknown-actor-jwt"
	unavailableJWT = "koinon_test_not-a-real-jwt-keys-down"
)

func (f fakeActors) Identify(token string) (actortoken.Actor, error) {
	switch token {
	case actortoken.Placeholder:
		return actortoken.Actor{}, actortoken.ErrPlaceholder
	case unknownJWT:
		return actortoken.Actor{}, actortoken.ErrUnknownActor
	case unavailableJWT:
		return actortoken.Actor{}, actortoken.ErrUnavailable
	}
	if a, ok := f[token]; ok {
		return a, nil
	}
	return actortoken.Actor{}, actortoken.ErrSignature
}

var _ = Describe("the actor JWT on the MCP listener (issue #1489)", func() {
	agent := gateway.AgentRef{Org: "acme", Name: "wiki-reader"}

	setup := func(ctx context.Context, actors gateway.ActorIdentifier) (*httptest.Server, gateway.IssuedKey, *upstreamLog) {
		GinkgoHelper()
		log := &upstreamLog{}
		up := fakeUpstream("wiki", log)
		DeferCleanup(up.Close)
		gw := mcpgateway.New(mcpgateway.Options{Audit: &auditLog{}, Guard: &recordingGuard{}, Actors: actors})
		DeferCleanup(gw.Drain)
		mux := http.NewServeMux()
		mux.Handle("/mcp", gw.Handler())
		front := httptest.NewServer(mux)
		DeferCleanup(func() { front.CloseClientConnections(); front.Close() })
		key := mustKey(agent)
		_, err := gw.ApplyMCP(ctx, gateway.MCPConfig{
			Keys: []gateway.Key{key.Key},
			MCPState: gateway.MCPState{
				MCP:   []gateway.MCPTarget{{Name: "wiki", Org: "acme", URL: up.URL + "/mcp"}},
				Tools: map[gateway.AgentRef][]gateway.ToolGrant{agent: {{Target: "wiki", All: true}}},
			},
		})
		Expect(err).NotTo(HaveOccurred())
		return front, key, log
	}

	post := func(url, authorization string) *http.Response {
		GinkgoHelper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
		req, err := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)
		return resp
	}

	It("serves the agent of a verified actor and never sends the JWT upstream", func(ctx SpecContext) {
		front, _, log := setup(ctx, fakeActors{actorJWT: {Org: "acme", Agent: "wiki-reader", Run: "r1"}})
		s := connect(ctx, front.URL, actorJWT)
		tools, err := s.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(tools.Tools).To(ContainElement(HaveField("Name", "wiki_read_item")))
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "wiki_read_item", Arguments: map[string]any{"text": "x"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.IsError).To(BeFalse())
		log.mu.Lock()
		defer log.mu.Unlock()
		Expect(log.headers).NotTo(BeEmpty())
		for _, h := range log.headers {
			for name, vals := range h {
				for _, v := range vals {
					Expect(v).NotTo(ContainSubstring(actorJWT), name)
				}
			}
		}
	})

	It("refuses the placeholder, an empty bearer, a bad token and no token with a final 401", func(ctx SpecContext) {
		front, _, _ := setup(ctx, fakeActors{actorJWT: {Org: "acme", Agent: "wiki-reader"}})
		for _, auth := range []string{"Bearer " + actortoken.Placeholder, "Bearer ", "Bearer    ", "Bearer koinon_test_not-a-real-bad-jwt", ""} {
			resp := post(front.URL, auth)
			Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized), auth)
			Expect(gateway.DoNotRetry(resp.Header)).To(BeTrue(), auth)
		}
	})

	It("refuses a verified token of an actor Koinon does not know with a final 403", func(ctx SpecContext) {
		front, _, _ := setup(ctx, fakeActors{})
		resp := post(front.URL, "Bearer "+unknownJWT)
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		Expect(gateway.DoNotRetry(resp.Header)).To(BeTrue())
	})

	It("answers 503 without the do-not-retry marker when the issuer keys are not available", func(ctx SpecContext) {
		front, _, _ := setup(ctx, fakeActors{})
		resp := post(front.URL, "Bearer "+unavailableJWT)
		Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
		Expect(gateway.DoNotRetry(resp.Header)).To(BeFalse())
		Expect(resp.Header.Get("Retry-After")).NotTo(BeEmpty())
	})

	It("still serves a gateway key, for Koinon's own callers", func(ctx SpecContext) {
		front, key, _ := setup(ctx, fakeActors{})
		s := connect(ctx, front.URL, key.Material)
		_, err := s.ListTools(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a token that is not a gateway key when no actor check is set", func(ctx SpecContext) {
		front, _, _ := setup(ctx, nil)
		resp := post(front.URL, "Bearer "+actorJWT)
		Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))
	})
})
