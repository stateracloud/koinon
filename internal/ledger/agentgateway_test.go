// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/moby/moby/api/types/container"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/providerusage"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
	"github.com/stateracloud/koinon/pricing"
)

// agentgatewayImage is the agentgateway release Koinon targets (ADR 0048).
const agentgatewayImage = "cr.agentgateway.dev/agentgateway:v1.7.0"

const (
	pricedModel   = "fake/ledger-model"
	unpricedModel = "fake/unpriced-model"
	slowModel     = "fake/slow-model"
	cutModel      = "fake/cut-model"
)

func hostPort(s *httptest.Server) int {
	_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

// The whole path against a real agentgateway: requests through the gateway, its OTLP access-log
// export into the in-process receiver, the ledger, and limits pushed back net of recorded spend.
var _ = Describe("agentgateway v1.7.0 exporting its access log to the ledger", Ordered, func() {
	var (
		e        *env
		run      store.Run
		key      gateway.IssuedKey
		state    gateway.State
		driver   *standalone.Driver
		llmURL   string
		ctr      *testcontainers.DockerContainer
		slowPort int
		cutPort  int
	)

	chat := func(ctx context.Context, material, model string) int {
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, llmURL+"/v1/chat/completions", bytes.NewBufferString(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+material)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	recorded := func(ctx context.Context) (int, string) {
		var n int
		var cost string
		Expect(db.QueryRow(ctx, `SELECT count(*), coalesce(sum(cost_usd), 0)::text FROM spend_entries WHERE org_id = $1`,
			e.org).Scan(&n, &cost)).To(Succeed())
		return n, ledger.FormatUSD(cost)
	}

	// push applies the state with the agent's limit as koinon-reconciler computes it.
	push := func(ctx context.Context) gateway.Limit {
		l, err := ledger.AgentLimit(ctx, q, e.agent, time.Now())
		Expect(err).NotTo(HaveOccurred())
		state.SetLimit(e.ref(), l)
		_, err = driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
		return *l
	}

	BeforeAll(func(ctx SpecContext) {
		needDocker()
		// $0.03 per day; each request is 10 + 10 tokens at $50/1M, so $0.001.
		e = newEnv(ctx, uniq("org"), "", "0.03")
		run = e.startRun(ctx, e.task(ctx), time.Now().Add(-time.Minute))

		model, err := fakemodel.New(fakemodel.Script{Model: pricedModel, Turns: []fakemodel.Turn{{
			Text: "ok", Usage: fakemodel.Usage{InputTokens: 10, OutputTokens: 10},
		}}}, fakemodel.Script{Model: unpricedModel, Turns: []fakemodel.Turn{{Text: "free?"}}})
		Expect(err).NotTo(HaveOccurred())
		upstream := httptest.NewServer(model)
		DeferCleanup(upstream.Close)
		// An upstream that answers after the client has given up.
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
		DeferCleanup(slow.Close)
		slowPort = hostPort(slow)
		// An upstream that streams part of a reply and then waits for the client to give up.
		cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Generation-Id", "gen-cut-1") // as OpenRouter sends it
			chunk := func(delta string) {
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"gen-cut-1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":%s}]}\n\n", delta)
				w.(http.Flusher).Flush()
			}
			for range 5 {
				chunk(`{"content":"hello "}`)
			}
			chunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"grep","arguments":"{\"q\":1}"}}]}`)
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		}))
		DeferCleanup(cut.Close)
		cutPort = hostPort(cut)
		// The fake provider type gets a usage source, as OpenRouter has, so a cut stream is provisional.
		settling := &ledger.Ingester{DB: db, Log: ing.Log, Prices: ing.Prices, Sources: providerusage.Registry{"fake": &fakeSource{}}}
		receiver := httptest.NewServer(ledger.Handler(settling, slog.New(slog.NewTextHandler(GinkgoWriter, nil))))
		DeferCleanup(receiver.Close)

		key, err = gateway.NewKey(e.ref())
		Expect(err).NotTo(HaveOccurred())
		state = gateway.State{Providers: []gateway.Provider{{
			Org: e.ref().Org, Name: "upstream", Type: "fake",
			BaseURL: fmt.Sprintf("http://%s:%d/v1", testcontainers.HostInternal, hostPort(upstream)),
			Models: []gateway.Model{
				{Name: pricedModel, Format: modelformat.OpenAI, Price: &gateway.Price{Input: "50", Output: "50"}},
				{Name: unpricedModel, Format: modelformat.OpenAI},
			},
		}}}
		state.AddKey(key.Key)
		l, err := ledger.AgentLimit(ctx, q, e.agent, time.Now())
		Expect(err).NotTo(HaveOccurred())
		state.SetLimit(e.ref(), l)

		options := standalone.Options{
			DatabaseURL:   "sqlite:///tmp/agentgateway.db?mode=rwc",
			AccessLogOTLP: &standalone.OTLPExport{HostPort: fmt.Sprintf("%s:%d", testcontainers.HostInternal, hostPort(receiver))},
		}
		cfgDir := GinkgoT().TempDir()
		Expect(os.Chmod(cfgDir, 0o755)).To(Succeed())
		file := standalone.FileDeliverer{Path: filepath.Join(cfgDir, "config.yaml")}
		first, _, err := standalone.Render(state, options)
		Expect(err).NotTo(HaveOccurred())
		Expect(file.Deliver(ctx, first, "")).To(Succeed())

		ctr, err = testcontainers.Run(ctx, agentgatewayImage,
			testcontainers.WithCmd("-f", "/cfg/config.yaml"),
			testcontainers.WithExposedPorts("4000/tcp", "15020/tcp", "15021/tcp"),
			testcontainers.WithHostPortAccess(hostPort(upstream), hostPort(receiver), slowPort, cutPort),
			testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
				hc.Binds = append(hc.Binds, cfgDir+":/cfg:ro")
			}),
			testcontainers.WithWaitStrategyAndDeadline(time.Minute, wait.ForHTTP("/healthz/ready").WithPort("15021/tcp")),
		)
		DeferCleanup(func(ctx context.Context) error { return testcontainers.TerminateContainer(ctr) })
		Expect(err).NotTo(HaveOccurred())

		llmURL, err = ctr.PortEndpoint(ctx, "4000/tcp", "http")
		Expect(err).NotTo(HaveOccurred())
		statsURL, err := ctr.PortEndpoint(ctx, "15020/tcp", "http")
		Expect(err).NotTo(HaveOccurred())
		driver = &standalone.Driver{
			Options: options, Deliverer: file,
			Waiter: standalone.ProbeWaiter{GatewayURL: llmURL, StatsURL: statsURL, Interval: 200 * time.Millisecond},
		}
		_, err = driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())
	}, NodeTimeout(3*time.Minute))

	AfterEach(func(ctx SpecContext) {
		if CurrentSpecReport().Failed() && ctr != nil {
			if logs, err := ctr.Logs(ctx); err == nil {
				b, _ := io.ReadAll(logs)
				AddReportEntry("agentgateway logs", string(b))
			}
		}
	})

	It("records every charged request with tokens and USD, attributed to the run", func(ctx SpecContext) {
		for range 3 {
			Expect(chat(ctx, key.Material, pricedModel)).To(Equal(http.StatusOK))
		}
		Eventually(func() []any { n, c := recorded(ctx); return []any{n, c} }).
			WithTimeout(20 * time.Second).WithPolling(200 * time.Millisecond).
			Should(Equal([]any{3, "0.003"}))
		Expect(e.run(ctx, run.ID).TokensUsed).To(Equal(int64(60)))

		var runID string
		Expect(db.QueryRow(ctx, `SELECT DISTINCT run_id::text FROM spend_entries WHERE org_id = $1`, e.org).Scan(&runID)).To(Succeed())
		Expect(runID).To(Equal(run.ID.String()))
	}, SpecTimeout(time.Minute))

	It("does not serve a model without a price", func(ctx SpecContext) {
		// The key's allowedModels leaves an unpriced model out, so the key may not ask for it (403);
		// without the list agentgateway answers 404 model_not_found. Either way it is never charged.
		Expect(chat(ctx, key.Material, unpricedModel)).To(BeElementOf(http.StatusForbidden, http.StatusNotFound))
	})

	It("re-pushes the limit net of recorded spend without counting that spend twice", func(ctx SpecContext) {
		l := push(ctx)
		Expect(l.Amount).To(BeNumerically("~", 0.027, 1e-12))

		// The same key gets a fresh gateway counter with the remainder: 27 more requests pass,
		// not 24 (remainder minus the gateway's own count) and not 30 (the full limit).
		passed := 0
		for range 40 {
			code := chat(ctx, key.Material, pricedModel)
			if code == http.StatusTooManyRequests {
				break
			}
			Expect(code).To(Equal(http.StatusOK))
			passed++
		}
		// ADR 0041: at most one response of overshoot.
		Expect(passed).To(BeNumerically(">=", 27))
		Expect(passed).To(BeNumerically("<=", 28))

		// The ledger catches up and fails the run with a clear reason.
		Eventually(func() store.RunState { return e.run(ctx, run.ID).State }).
			WithTimeout(20 * time.Second).WithPolling(200 * time.Millisecond).
			Should(Equal(store.RunStateCanceled))
		Expect(e.run(ctx, run.ID).OutcomeReason.String).To(HavePrefix("agent daily limit reached"))
	}, SpecTimeout(2*time.Minute))

	It("keeps the agent blocked after its key is rotated", func(ctx SpecContext) {
		Eventually(func() string { _, c := recorded(ctx); return c }).
			WithTimeout(20 * time.Second).WithPolling(200 * time.Millisecond).
			Should(Or(Equal("0.03"), Equal("0.031")))

		// A new key starts with an empty gateway counter; without netting it would get the full
		// limit again.
		Expect(state.RevokeKey(key.ID)).To(BeTrue())
		rotated, err := driver.IssueKey(ctx, e.ref())
		Expect(err).NotTo(HaveOccurred())
		state.AddKey(rotated.Key)
		l := push(ctx)
		Expect(l.Amount).To(BeZero())

		Expect(chat(ctx, key.Material, pricedModel)).To(Equal(http.StatusUnauthorized))
		Expect(chat(ctx, rotated.Material, pricedModel)).To(Equal(http.StatusTooManyRequests))
	}, SpecTimeout(time.Minute))

	// agentgateway logs a request whose client left before the response headers with the model
	// and the key but no status, no usage and no cost (verified against v1.7.0). The provider may
	// have read the prompt, so the ledger charges the input estimate, marked usage_missing; no
	// output was streamed, so none is charged.
	It("charges a request whose client disconnected before the headers, as an estimate", func(ctx SpecContext) {
		d := newEnv(ctx, uniq("org"), "", "100.00")
		d.model(ctx, "fake", slowModel)
		_, err := prices.New(db, nil, priceList).SetOverride(ctx, d.org, audit.User(d.user), prices.OverrideInput{
			Entry: pricing.Entry{
				ID: "slow", Provider: "fake", Match: pricing.Match{Glob: slowModel}, Spec: pricing.Spec{Prices: pricing.Wire{Input: "50", Output: "50"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 4096,
			},
			ValidFrom: time.Now().Add(-time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())
		dkey, err := gateway.NewKey(d.ref())
		Expect(err).NotTo(HaveOccurred())
		state.Providers = append(state.Providers, gateway.Provider{
			Org: d.ref().Org, Name: "slow", Type: "fake",
			BaseURL: fmt.Sprintf("http://%s:%d/v1", testcontainers.HostInternal, slowPort),
			Models:  []gateway.Model{{Name: slowModel, Format: modelformat.OpenAI, Price: &gateway.Price{Input: "50", Output: "50"}}},
		})
		state.AddKey(dkey.Key)
		_, err = driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())

		short, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
		defer cancel()
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, slowModel)
		req, err := http.NewRequestWithContext(short, http.MethodPost, llmURL+"/v1/chat/completions", bytes.NewBufferString(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+dkey.Material)
		_, err = http.DefaultClient.Do(req)
		Expect(err).To(HaveOccurred(), "the client gave up before the headers")

		var row spendRow
		Eventually(func() error {
			return db.QueryRow(ctx, `SELECT input_tokens, output_tokens, cache_read_tokens, cost_usd::text, coalesce(gateway_cost_usd::text, ''),
				coalesce(price_ref, ''), estimated, cost_flagged, coalesce(estimate, '{}'::jsonb)
				FROM spend_entries WHERE org_id = $1`, d.org).Scan(
				&row.InputTokens, &row.OutputTokens, &row.CacheReadTokens, &row.CostUsd, &row.GatewayCostUsd, &row.PriceRef, &row.Estimated, &row.CostFlagged, &row.Estimate)
		}).WithTimeout(20 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		Expect(row.Estimated).To(BeTrue())
		Expect(row.CostFlagged).To(BeTrue())
		Expect(string(row.Estimate)).To(ContainSubstring(`"usage_missing": true`))
		Expect(ledger.FormatUSD(row.CostUsd)).To(Equal("0.0012"), "charged its input, not dropped")
		Expect(row.OutputTokens).To(BeZero(), "nothing was streamed: no output, never max_tokens")
	}, SpecTimeout(time.Minute))

	// agentgateway v1.7.0 logs a cut chat-completion stream with the usage it saw, which is none
	// (usage comes in the final chunk only), and logs the x-generation-id response header through
	// koinon.response_id without recording the body (verified here against the real binary). The
	// ledger charges the logged usage (here the input estimate), never max_tokens, and holds the
	// max_tokens bound back from the key while the provider's figure is pending.
	It("charges a stream cut short the usage the gateway logged, provisionally, with the provider's response ID", func(ctx SpecContext) {
		c := newEnv(ctx, uniq("org"), "", "100.00")
		c.model(ctx, "fake", cutModel)
		_, err := prices.New(db, nil, priceList).SetOverride(ctx, c.org, audit.User(c.user), prices.OverrideInput{
			Entry: pricing.Entry{
				ID: "cut", Provider: "fake", Match: pricing.Match{Glob: cutModel}, Spec: pricing.Spec{Prices: pricing.Wire{Input: "50", Output: "50"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 128000,
			},
			ValidFrom: time.Now().Add(-time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())
		ckey, err := gateway.NewKey(c.ref())
		Expect(err).NotTo(HaveOccurred())
		state.Providers = append(state.Providers, gateway.Provider{
			Org: c.ref().Org, Name: "cut", Type: "fake",
			BaseURL: fmt.Sprintf("http://%s:%d/v1", testcontainers.HostInternal, cutPort),
			Models:  []gateway.Model{{Name: cutModel, Format: modelformat.OpenAI, Price: &gateway.Price{Input: "50", Output: "50"}}},
		})
		state.AddKey(ckey.Key)
		_, err = driver.Apply(ctx, state)
		Expect(err).NotTo(HaveOccurred())

		short, cancel := context.WithCancel(ctx)
		body := fmt.Sprintf(`{"model":%q,"stream":true,"max_tokens":32000,"messages":[{"role":"user","content":"hi"}]}`, cutModel)
		req, err := http.NewRequestWithContext(short, http.MethodPost, llmURL+"/v1/chat/completions", bytes.NewBufferString(body))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+ckey.Material)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		got := 0
		buf := make([]byte, 4096)
		for got < 6 {
			n, err := resp.Body.Read(buf)
			got += bytes.Count(buf[:n], []byte("data: "))
			Expect(err).NotTo(HaveOccurred())
		}
		cancel() // the run is canceled mid-response
		_ = resp.Body.Close()

		var in, out int64
		var responseID, reserve string
		var provisional, estimated bool
		Eventually(func() error {
			return db.QueryRow(ctx, `SELECT input_tokens, output_tokens, coalesce(response_id, ''), reserve_usd::text, provisional, estimated
				FROM spend_entries WHERE org_id = $1`, c.org).Scan(&in, &out, &responseID, &reserve, &provisional, &estimated)
		}).WithTimeout(20 * time.Second).WithPolling(200 * time.Millisecond).Should(Succeed())
		Expect(out).To(BeZero(), "no output usage was logged: none is charged, not 32000")
		Expect(in).To(BeNumerically(">", 0), "the input is estimated from the request bytes")
		Expect(estimated).To(BeTrue())
		Expect(responseID).To(Equal("gen-cut-1"), "the response header, logged without the body")
		Expect(provisional).To(BeTrue(), "its provider can report what it billed")
		// 32000 output tokens at $50 per million is held back from the key, not charged.
		Expect(ledger.FormatUSD(reserve)).To(Equal("1.60"))
		var settlements int
		Expect(db.QueryRow(ctx, `SELECT count(*) FROM spend_settlements WHERE org_id = $1`, c.org).Scan(&settlements)).To(Succeed())
		Expect(settlements).To(Equal(1))
	}, SpecTimeout(time.Minute))
})
