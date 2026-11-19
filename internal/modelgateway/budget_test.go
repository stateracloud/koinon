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
		upBody   atomic.Value // string: the provider's answer
		upSSE    atomic.Bool  // answer with a stream that ends before its usage
		hold     atomic.Pointer[chan struct{}]
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
		upBody.Store(`{"id":"x","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
		upSSE.Store(false)
		hold.Store(nil)
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if h := hold.Load(); h != nil {
				<-*h
			}
			if upSSE.Load() {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n")) // no usage chunk, no [DONE]
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(upStatus.Load()))
			// 7 + 3 tokens at $1000 per million tokens: $0.01 a call.
			_, _ = w.Write([]byte(upBody.Load().(string)))
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

	It("does not stop an agent without a limit, or an agent with its own room", func(ctx SpecContext) {
		free, err := gateway.NewKey(other)
		Expect(err).NotTo(HaveOccurred())
		roomy := gateway.AgentRef{Org: "acme", Name: "roomy"}
		room, err := gateway.NewKey(roomy)
		Expect(err).NotTo(HaveOccurred())
		cfg.Keys = append(cfg.Keys, free.Key, room.Key)
		cfg.SetLimit(agent, limit("0", "g1"))
		cfg.SetLimit(roomy, limit("1", "g1"))
		apply(ctx)
		for range 3 {
			for _, k := range []string{free.Material, room.Material} {
				code, _, body := call(k)
				Expect(code).To(Equal(http.StatusOK), body)
			}
		}
		code, _, _ := call(key.Material)
		Expect(code).To(Equal(http.StatusTooManyRequests))
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

	It("stops a key whose call cannot be priced", func(ctx SpecContext) {
		upBody.Store(`{"id":"x","choices":[],"usage":{"prompt_tokens":9000000000000000000,"completion_tokens":3}}`)
		cfg.SetLimit(agent, limit("100", "g1"))
		apply(ctx)
		code, _, _ := call(key.Material)
		Expect(code).To(Equal(http.StatusOK))
		code, _, _ = call(key.Material)
		Expect(code).To(Equal(http.StatusTooManyRequests), "an overflow saturates the counter, it does not hide the spend")
	})

	It("charges a bound for a stream that ended before its usage", func(ctx SpecContext) {
		upSSE.Store(true)
		cfg.SetLimit(agent, limit("0.000005", "g1")) // the request's bytes alone cost more
		apply(ctx)
		code, _, _ := call(key.Material)
		Expect(code).To(Equal(http.StatusOK))
		code, _, _ = call(key.Material)
		Expect(code).To(Equal(http.StatusTooManyRequests))
	})

	It("charges a call that crosses a new generation to the generation in force when it ends", func(ctx SpecContext) {
		cfg.SetLimit(agent, limit("0.015", "g1"))
		apply(ctx)
		release := make(chan struct{})
		hold.Store(&release)
		done := make(chan int, 1)
		go func() {
			defer GinkgoRecover()
			code, _, _ := call(key.Material)
			done <- code
		}()
		Eventually(hits.Load).Should(BeEquivalentTo(1)) // the call is at the provider
		cfg.SetLimit(agent, limit("0.015", "g2"))
		apply(ctx)
		close(release)
		Eventually(done).Should(Receive(Equal(http.StatusOK)))
		hold.Store(nil)
		code, _, _ := call(key.Material) // $0.01 spent under g2: this one still passes
		Expect(code).To(Equal(http.StatusOK))
		code, _, _ = call(key.Material)
		Expect(code).To(Equal(http.StatusTooManyRequests), "the in-flight call counted against g2")
	})

	It("charges a bound for a 200 answer that carries no usage", func(ctx SpecContext) {
		upBody.Store(`{"id":"x","choices":[]}`)
		cfg.SetLimit(agent, limit("0.000005", "g1"))
		apply(ctx)
		code, _, _ := call(key.Material)
		Expect(code).To(Equal(http.StatusOK))
		code, _, _ = call(key.Material)
		Expect(code).To(Equal(http.StatusTooManyRequests), "a call without usage is not free")
	})

	It("checks a slow upload against the limit in force when its body ends", func(ctx SpecContext) {
		cfg.SetLimit(agent, limit("1", "g1"))
		apply(ctx)
		pr, pw := io.Pipe()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", pr)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key.Material)
		done := make(chan int, 1)
		go func() {
			defer GinkgoRecover()
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			done <- resp.StatusCode
		}()
		_, err = pw.Write([]byte(`{"model":"m",`))
		Expect(err).NotTo(HaveOccurred())
		cfg.SetLimit(agent, limit("0", "g2")) // the ledger pushes a new generation, at its limit
		apply(ctx)
		_, err = pw.Write([]byte(`"messages":[]}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(pw.Close()).To(Succeed())
		Eventually(done).Should(Receive(Equal(http.StatusTooManyRequests)))
		Expect(hits.Load()).To(BeZero())
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
