// SPDX-License-Identifier: Apache-2.0

package modelgateway_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/modelgateway"
	"github.com/stateracloud/koinon/internal/money"
)

// The budget stop and the retry markers of the model path (#279 slice 2).
var _ = Describe("the budget stop of the model path", func() {
	var (
		gw       *modelgateway.Gateway
		srv      *httptest.Server
		cfg      gateway.ModelConfig
		now      time.Time
		clock    sync.Mutex
		hits     atomic.Int64
		upStatus atomic.Int64
		key      gateway.IssuedKey
	)
	agent := gateway.AgentRef{Org: "acme", Name: "helper"}
	other := gateway.AgentRef{Org: "acme", Name: "other"}

	post := func(token string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewBufferString(`{"model":"m","messages":[]}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resp.Body.Close)
		return resp
	}
	// call sends one request and reads the whole body, which ends the call and charges it.
	call := func(token string) (int, http.Header, string) {
		resp := post(token)
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(b)
	}
	advance := func(d time.Duration) {
		clock.Lock()
		defer clock.Unlock()
		now = now.Add(d)
	}
	apply := func(ctx SpecContext) {
		GinkgoHelper()
		_, err := gw.ApplyModel(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
	}
	limit := func(amount string, gen string) *gateway.Limit {
		return &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse(amount), Window: 24 * time.Hour, Generation: gen}
	}

	BeforeEach(func() {
		hits.Store(0)
		upStatus.Store(http.StatusOK)
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(upStatus.Load()))
			// 7 + 3 tokens at $1000 per million tokens: $0.01 a call.
			_, _ = w.Write([]byte(`{"id":"x","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
		}))
		DeferCleanup(up.Close)
		now = time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC)
		gw = modelgateway.New(modelgateway.Options{Now: func() time.Time {
			clock.Lock()
			defer clock.Unlock()
			return now
		}})
		srv = httptest.NewServer(gw.Handler())
		DeferCleanup(srv.Close)
		var err error
		key, err = gateway.NewKey(agent)
		Expect(err).NotTo(HaveOccurred())
		cfg = gateway.ModelConfig{}
		cfg.Keys = []gateway.Key{key.Key}
		cfg.Providers = []gateway.Provider{{Org: "acme", Name: "p", Type: "custom", BaseURL: up.URL + "/v1", Models: []gateway.Model{
			{Name: "m", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1000", Output: "1000"}},
		}}}
	})

	It("refuses a zero limit at once, without calling the provider", func(ctx SpecContext) {
		cfg.SetLimit(agent, limit("0", "g1"))
		apply(ctx)
		code, h, body := call(key.Material)
		Expect(code).To(Equal(http.StatusTooManyRequests))
		Expect(body).To(ContainSubstring("budget_exceeded"))
		Expect(gateway.DoNotRetry(h)).To(BeTrue(), "the budget refusal is final")
		Expect(h.Get("Retry-After")).To(Equal("48600"), "the seconds until midnight UTC")
		Expect(hits.Load()).To(BeZero())
	})

	It("charges each call after it ends and stops the call that follows the limit", func(ctx SpecContext) {
		cfg.SetLimit(agent, limit("0.015", "g1"))
		apply(ctx)
		for i := range 2 { // $0.01 and $0.02: the second overshoots by less than one response
			code, _, body := call(key.Material)
			Expect(code).To(Equal(http.StatusOK), "call %d: %s", i, body)
		}
		code, h, body := call(key.Material)
		Expect(code).To(Equal(http.StatusTooManyRequests), body)
		Expect(gateway.DoNotRetry(h)).To(BeTrue())
		Expect(hits.Load()).To(BeEquivalentTo(2))
	})

	It("does not stop an agent without a limit, or another agent", func(ctx SpecContext) {
		ok, err := gateway.NewKey(other)
		Expect(err).NotTo(HaveOccurred())
		cfg.Keys = append(cfg.Keys, ok.Key)
		cfg.SetLimit(agent, limit("0", "g1"))
		apply(ctx)
		for range 3 {
			code, _, body := call(ok.Material)
			Expect(code).To(Equal(http.StatusOK), body)
		}
	})

	It("starts a fresh count in the next window, and sends the wait the window leaves", func(ctx SpecContext) {
		cfg.SetLimit(agent, limit("0.005", "g1"))
		apply(ctx)
		code, _, _ := call(key.Material)
		Expect(code).To(Equal(http.StatusOK))
		_, h, _ := call(key.Material)
		Expect(h.Get("Retry-After")).To(Equal("48600"))
		advance(13*time.Hour + 30*time.Minute) // 00:00 UTC of the next day
		code, _, body := call(key.Material)
		Expect(code).To(Equal(http.StatusOK), body)
	})

	It("keeps the count over a re-apply of the same generation and resets it for a new one", func(ctx SpecContext) {
		cfg.SetLimit(agent, limit("0.005", "g1"))
		apply(ctx)
		code, _, _ := call(key.Material)
		Expect(code).To(Equal(http.StatusOK))
		apply(ctx) // the same config again
		code, _, _ = call(key.Material)
		Expect(code).To(Equal(http.StatusTooManyRequests), "the spend survives an apply that changes nothing")
		cfg.SetLimit(agent, limit("0.005", "g2")) // the ledger pushed a new net amount
		apply(ctx)
		code, _, body := call(key.Material)
		Expect(code).To(Equal(http.StatusOK), body)
	})

	It("does not charge an error answer of the provider", func(ctx SpecContext) {
		cfg.SetLimit(agent, limit("0.005", "g1"))
		apply(ctx)
		upStatus.Store(http.StatusBadGateway)
		code, _, _ := call(key.Material)
		Expect(code).To(Equal(http.StatusBadGateway))
		upStatus.Store(http.StatusOK)
		code, _, body := call(key.Material)
		Expect(code).To(Equal(http.StatusOK), body+": an error answer is not charged")
	})

	It("answers an unknown key with 401, never with a budget refusal, while its config is not applied", func(ctx SpecContext) {
		fresh, err := gateway.NewKey(other)
		Expect(err).NotTo(HaveOccurred())
		cfg.SetLimit(agent, limit("0", "g1"))
		cfg.SetLimit(other, limit("0", "g1"))
		apply(ctx) // fresh is not in the config yet
		code, h, body := call(fresh.Material)
		Expect(code).To(Equal(http.StatusUnauthorized))
		Expect(body).NotTo(ContainSubstring("budget"))
		Expect(gateway.DoNotRetry(h)).To(BeTrue())
		cfg.Keys = append(cfg.Keys, fresh.Key)
		apply(ctx)
		code, _, body = call(fresh.Material)
		Expect(code).To(Equal(http.StatusTooManyRequests), "once loaded, the key meets its own limit: "+body)
	})

	Describe("the retry markers", func() {
		It("marks what the gateway refuses and the provider's 403, and leaves the provider's other failures retryable", func(ctx SpecContext) {
			apply(ctx)
			for status, marked := range map[int]bool{
				http.StatusForbidden:           true,
				http.StatusTooManyRequests:     false, // a provider's own 429 stays retryable (ADR 0056)
				http.StatusInternalServerError: false,
				http.StatusBadGateway:          false,
				http.StatusBadRequest:          false,
			} {
				upStatus.Store(int64(status))
				code, h, _ := call(key.Material)
				Expect(code).To(Equal(status))
				Expect(gateway.DoNotRetry(h)).To(Equal(marked), "status %d", status)
			}
		})

		It("does not mark a failure of the gateway's own transport", func(ctx SpecContext) {
			cfg.Providers[0].BaseURL = "http://127.0.0.1:1/v1" // nothing listens
			apply(ctx)
			code, h, _ := call(key.Material)
			Expect(code).To(Equal(http.StatusBadGateway))
			Expect(gateway.DoNotRetry(h)).To(BeFalse())
			Expect(h.Get("Retry-After")).To(BeEmpty())
		})
	})
})
