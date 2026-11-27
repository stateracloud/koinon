// SPDX-License-Identifier: Apache-2.0

package modelgateway_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/actortoken"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/modelgateway"
)

const (
	actorJWT       = "koinon_test_not-a-real-actor-jwt"
	unknownJWT     = "koinon_test_not-a-real-unknown-actor-jwt"
	unavailableJWT = "koinon_test_not-a-real-jwt-keys-down"
)

// fakeActors stands in for the Substrate identifier of the model audience.
type fakeActors map[string]actortoken.Actor

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

var _ = Describe("the actor JWT on the model listener (issue #1489)", func() {
	var (
		gw      *modelgateway.Gateway
		srv     *httptest.Server
		s       *sink
		cfg     gateway.ModelConfig
		mu      sync.Mutex
		upAuths []string
	)
	agent := gateway.AgentRef{Org: "acme", Name: "helper"}

	post := func(authorization string) *http.Response {
		GinkgoHelper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewBufferString(`{"model":"m","messages":[]}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)
		return resp
	}

	BeforeEach(func() {
		upAuths = nil
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			upAuths = append(upAuths, r.Header.Get("Authorization"))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
		}))
		DeferCleanup(up.Close)
		s = &sink{}
		gw = modelgateway.New(modelgateway.Options{Sink: s.put, Actors: fakeActors{
			actorJWT: {Org: "acme", Agent: "helper", Run: "r1"},
		}})
		mux := http.NewServeMux()
		for _, p := range gateway.RoutePaths() {
			mux.Handle(p, gw.Handler())
		}
		srv = httptest.NewServer(mux)
		DeferCleanup(srv.Close)
		cfg = gateway.ModelConfig{}
		cfg.Providers = []gateway.Provider{{Org: "acme", Name: "p", Type: "custom", BaseURL: up.URL + "/v1", Models: []gateway.Model{
			{Name: "m", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "1"}},
		}}}
	})

	It("serves a verified actor on its agent's key and never sends the JWT upstream", func(ctx SpecContext) {
		k, err := gateway.NewKey(agent)
		Expect(err).NotTo(HaveOccurred())
		cfg.Keys = []gateway.Key{k.Key}
		_, err = gw.ApplyModel(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
		resp := post("Bearer " + actorJWT)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		_, _ = io.Copy(io.Discard, resp.Body)
		Eventually(func() int { return len(s.of(k.Key.ID)) }).Should(Equal(1), "the usage counts against the agent's key")
		mu.Lock()
		defer mu.Unlock()
		Expect(upAuths).To(HaveLen(1))
		Expect(upAuths[0]).NotTo(ContainSubstring(actorJWT))
	})

	It("refuses the placeholder, an empty bearer, a bad token and no token with a final 401", func(ctx SpecContext) {
		k, _ := gateway.NewKey(agent)
		cfg.Keys = []gateway.Key{k.Key}
		_, err := gw.ApplyModel(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
		for _, auth := range []string{"Bearer " + actortoken.Placeholder, "Bearer ", "Bearer koinon_test_not-a-real-bad-jwt", ""} {
			resp := post(auth)
			Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized), auth)
			Expect(gateway.DoNotRetry(resp.Header)).To(BeTrue(), auth)
		}
		Expect(s.of(k.ID)).To(BeEmpty())
	})

	It("refuses an unknown actor, and an agent without exactly one key, with a final 403", func(ctx SpecContext) {
		resp := post("Bearer " + unknownJWT)
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))
		Expect(gateway.DoNotRetry(resp.Header)).To(BeTrue())

		_, err := gw.ApplyModel(ctx, cfg) // no key for the agent
		Expect(err).NotTo(HaveOccurred())
		resp = post("Bearer " + actorJWT)
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden))

		k1, _ := gateway.NewKey(agent)
		k2, _ := gateway.NewKey(agent)
		cfg.Keys = []gateway.Key{k1.Key, k2.Key}
		_, err = gw.ApplyModel(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
		resp = post("Bearer " + actorJWT)
		Expect(resp.StatusCode).To(Equal(http.StatusForbidden), "two keys: the gateway does not guess")
	})

	It("answers 503 without the do-not-retry marker when the issuer keys are not available", func() {
		resp := post("Bearer " + unavailableJWT)
		Expect(resp.StatusCode).To(Equal(http.StatusServiceUnavailable))
		Expect(gateway.DoNotRetry(resp.Header)).To(BeFalse())
	})

	It("still serves a gateway key", func(ctx SpecContext) {
		k, _ := gateway.NewKey(agent)
		cfg.Keys = []gateway.Key{k.Key}
		_, err := gw.ApplyModel(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
		resp := post("Bearer " + k.Material)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
	})
})
