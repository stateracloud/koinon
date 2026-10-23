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

	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// agentgatewayImage is the agentgateway release Koinon targets (ADR 0048).
const agentgatewayImage = "cr.agentgateway.dev/agentgateway:v1.6.0"

const (
	pricedModel   = "fake/ledger-model"
	unpricedModel = "fake/unpriced-model"
)

func hostPort(s *httptest.Server) int {
	_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

// The whole path against a real agentgateway: requests through the gateway, its OTLP access-log
// export into the in-process receiver, the ledger, and limits pushed back net of recorded spend.
var _ = Describe("agentgateway v1.6.0 exporting its access log to the ledger", Ordered, func() {
	var (
		e      *env
		run    store.Run
		key    gateway.IssuedKey
		state  gateway.State
		driver *standalone.Driver
		llmURL string
		ctr    *testcontainers.DockerContainer
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
		e = newEnv(ctx, uniq("org"), "", "0.03", 1_000_000)
		run = e.startRun(ctx, e.ticket(ctx), 1_000_000, time.Now().Add(-time.Minute))

		model, err := fakemodel.New(fakemodel.Script{Model: pricedModel, Turns: []fakemodel.Turn{{
			Text: "ok", Usage: fakemodel.Usage{InputTokens: 10, OutputTokens: 10},
		}}}, fakemodel.Script{Model: unpricedModel, Turns: []fakemodel.Turn{{Text: "free?"}}})
		Expect(err).NotTo(HaveOccurred())
		upstream := httptest.NewServer(model)
		DeferCleanup(upstream.Close)
		receiver := httptest.NewServer(ledger.Handler(ing, slog.New(slog.NewTextHandler(GinkgoWriter, nil))))
		DeferCleanup(receiver.Close)

		key, err = gateway.NewKey(e.ref())
		Expect(err).NotTo(HaveOccurred())
		state = gateway.State{Models: gateway.ModelUpstream{
			Provider: "fake",
			BaseURL:  fmt.Sprintf("http://%s:%d/v1", testcontainers.HostInternal, hostPort(upstream)),
			Models: []gateway.Model{
				{Name: pricedModel, Price: &gateway.Price{Input: "50", Output: "50"}},
				{Name: unpricedModel},
			},
		}}
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
			testcontainers.WithHostPortAccess(hostPort(upstream), hostPort(receiver)),
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
		Expect(chat(ctx, key.Material, unpricedModel)).To(Equal(http.StatusNotFound))
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
		Expect(e.run(ctx, run.ID).OutcomeReason.String).To(HavePrefix("agent spend limit reached"))
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
})
