// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// budgetOvershootSpec is the budget stop of ADR 0097 decision 4: a zero limit refuses, and calls
// that arrive together near the limit overshoot it by the responses still in flight. The overshoot
// is measured and written to the report, not asserted away; the bound the spec holds is the one
// gateway.Limit documents: one response per concurrent call.
func budgetOvershootSpec(env Env) {
	if !env.GatewayBudgets {
		return // Kubernetes mode: the ledger revokes the key, which that driver's own suite tests.
	}
	const (
		model      = "budget-model"
		concurrent = 12
		// Each response is 10 + 10 tokens at $50 per million: $0.001.
		perRequest = "0.001"
	)
	Describe(env.Name+": the budget stop with calls in flight (ADR 0041, ADR 0097)", Ordered, ContinueOnFailure, func() {
		var (
			gw           *Gateway
			zero, nearly gateway.IssuedKey
		)
		BeforeAll(func(ctx SpecContext) {
			fm, err := fakemodel.New(fakemodel.Script{
				Model: model, Faults: fakemodel.Faults{Delay: 1500 * time.Millisecond},
				Turns: []fakemodel.Turn{{Text: "ok", Usage: fakemodel.Usage{InputTokens: 10, OutputTokens: 10}}},
			})
			Expect(err).NotTo(HaveOccurred())
			srv := newServer(fm)
			DeferCleanup(srv.Close)

			state := gateway.State{Providers: []gateway.Provider{{
				Org: "acme", Name: "budget", Type: "openai", BaseURL: fmt.Sprintf("http://%s:%d/v1", env.Host, port(srv)),
				Models: []gateway.Model{{Name: model, Format: modelformat.OpenAI, Price: &gateway.Price{Input: "50", Output: "50"}}},
			}}}
			zeroAgent := gateway.AgentRef{Org: "acme", Name: "zero-limit"}
			nearAgent := gateway.AgentRef{Org: "acme", Name: "near-limit"}
			zero, err = gateway.NewKey(zeroAgent)
			Expect(err).NotTo(HaveOccurred())
			nearly, err = gateway.NewKey(nearAgent)
			Expect(err).NotTo(HaveOccurred())
			state.AddKey(zero.Key)
			state.AddKey(nearly.Key)
			state.SetLimit(zeroAgent, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("0"), Window: 24 * time.Hour})
			// Room for two responses.
			state.SetLimit(nearAgent, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("0.0025"), Window: 24 * time.Hour})

			gw, err = env.Start(ctx, []int{port(srv)}, nil, "")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(5*time.Minute))

		AfterEach(func(ctx SpecContext) {
			if CurrentSpecReport().Failed() && gw != nil && gw.Logs != nil {
				AddReportEntry("gateway logs", gw.Logs(ctx))
			}
		})

		call := func(ctx context.Context, key string) (int, string) {
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewBufferString(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b)
		}

		It("refuses every call of a key whose limit is zero", func(ctx SpecContext) {
			code, body := call(ctx, zero.Material)
			Expect(code).To(Equal(http.StatusTooManyRequests), body)
			Expect(body).To(ContainSubstring("budget_exceeded"))
		}, SpecTimeout(time.Minute))

		It("lets calls that arrive together past the limit by the responses in flight, and stops the next one", func(ctx SpecContext) {
			codes := make([]int, concurrent)
			var wg sync.WaitGroup
			for i := range concurrent {
				wg.Go(func() { codes[i], _ = call(ctx, nearly.Material) })
			}
			wg.Wait()
			served, refused := 0, 0
			for _, c := range codes {
				switch c {
				case http.StatusOK:
					served++
				case http.StatusTooManyRequests:
					refused++
				default:
					Fail(fmt.Sprintf("unexpected status %d in %v", c, codes))
				}
			}
			const allowed = 2 // $0.0025 at $0.001 a response
			overshoot := max(served-allowed, 0)
			AddReportEntry("budget overshoot", fmt.Sprintf(
				"limit $0.0025 at $%s a response: %d concurrent calls, %d served, %d refused, overshoot %d responses ($%.4f)",
				perRequest, concurrent, served, refused, overshoot, float64(overshoot)*0.001))

			// The limit is a floor on service and a ceiling of one response per concurrent call.
			Expect(served).To(BeNumerically(">=", allowed), "the key's own budget is served")
			Expect(served).To(BeNumerically("<=", concurrent))
			Expect(served + refused).To(Equal(concurrent))

			// Once the responses are settled the key is stopped.
			Eventually(func() int {
				code, _ := call(ctx, nearly.Material)
				return code
			}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Equal(http.StatusTooManyRequests))
		}, SpecTimeout(2*time.Minute))
	})
}
