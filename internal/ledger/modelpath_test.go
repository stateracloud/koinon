// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
	"github.com/stateracloud/koinon/internal/gateway/usagewire"
	"github.com/stateracloud/koinon/internal/ledger"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/modelgateway"
	"github.com/stateracloud/koinon/internal/prices"
	"github.com/stateracloud/koinon/internal/providerusage"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
	"github.com/stateracloud/koinon/internal/testing/racescale"
	"github.com/stateracloud/koinon/pricing"
)

// Koinon's own model path (ADR 0097 step 2) to a ledger row, as koinon-gateway and koinon-api run
// it: the model gateway reads the usage from the Messages answer as it passes, its usage exporter
// sends the record over OTLP, and the ledger's receiver writes the spend entry. Anthropic reports
// the thinking split as usage.output_tokens_details.thinking_tokens, in the body of a plain answer
// and on the last message_delta of a stream (#78). An answer without it keeps the split unknown
// (reasoning_known false), not a reported 0.
var _ = Describe("the thinking-token split on Koinon's model path (#78)", func() {
	const (
		thinkModel  = "acme/claude-think"
		plainModel  = "acme/claude-plain"
		messagesAPI = "2023-06-01"
	)

	type row struct {
		Input, Output, Reasoning int64
		// Known is reasoning_known; KnownNull says it is NULL, which only rows from before
		// migration 00062 may be.
		Known, KnownNull bool
		CostOK           bool
	}

	// setup starts the fake model, the model gateway with its exporter and the ledger receiver,
	// for one org whose agent has a running run. It returns the gateway's URL and the key material
	// of the agent. The cleanups run in reverse: the gateway drains, then the exporter flushes.
	setup := func(ctx SpecContext, c *env) (string, string) {
		c.model(ctx, "anthropic", thinkModel)
		c.model(ctx, "anthropic", plainModel)
		_, err := prices.New(db, nil, priceList).SetOverride(ctx, c.org, audit.User(c.user), prices.OverrideInput{
			Entry: pricing.Entry{
				ID: uniq("claude-fixture"), Provider: "anthropic", Match: pricing.Match{Glob: "acme/claude-*"},
				Spec:      pricing.Spec{Prices: pricing.Wire{Input: "1", Output: "2"}},
				Tokenizer: pricing.TokenizerBytes, Ratio: "3.0", MaxOutput: 1000000,
			},
			ValidFrom: time.Now().Add(-time.Hour),
		})
		Expect(err).NotTo(HaveOccurred())
		c.startRun(ctx, c.task(ctx), time.Now().Add(-time.Minute))

		// 100 input tokens and 40 output tokens, 25 of which are thinking.
		fm, err := fakemodel.New(
			fakemodel.Script{Model: thinkModel, Turns: []fakemodel.Turn{{
				Text: "thought it through", Usage: fakemodel.Usage{InputTokens: 100, OutputTokens: 40, ReasoningTokens: 25},
			}}},
			fakemodel.Script{Model: plainModel, Turns: []fakemodel.Turn{{
				Text: "no thinking", Usage: fakemodel.Usage{InputTokens: 100, OutputTokens: 40},
			}}},
		)
		Expect(err).NotTo(HaveOccurred())
		upstream := httptest.NewServer(fm)
		DeferCleanup(upstream.Close)

		log := slog.New(slog.NewTextHandler(GinkgoWriter, nil))
		in := &ledger.Ingester{DB: db, Log: log, Prices: ing.Prices, Sources: providerusage.Registry{}}
		receiver := httptest.NewServer(agentgateway.Handler(in, log))
		DeferCleanup(receiver.Close)

		exp := usagewire.NewExporter(usagewire.ExporterOptions{
			URL: receiver.URL, Token: func() (string, error) { return "koinon_test_not-a-real-token", nil },
			Logger: log, Interval: 50 * time.Millisecond,
		})
		ectx, stop := context.WithCancel(context.Background())
		go exp.Run(ectx, 5*time.Second)
		DeferCleanup(func() { stop(); <-exp.Done() })

		keyFile := filepath.Join(GinkgoT().TempDir(), "provider_key")
		Expect(os.WriteFile(keyFile, []byte("koinon_test_not-a-real-token-upstream"), 0o600)).To(Succeed())
		gw := modelgateway.New(modelgateway.Options{Sink: exp.Record, Logger: log})
		mux := http.NewServeMux()
		for _, p := range gateway.RoutePaths() {
			mux.Handle(p, gw.Handler())
		}
		srv := httptest.NewServer(mux)
		DeferCleanup(func() { gw.Drain(); srv.CloseClientConnections(); srv.Close() })

		key, err := gateway.NewKey(c.ref())
		Expect(err).NotTo(HaveOccurred())
		price := &gateway.Price{Input: "1", Output: "2"}
		state := gateway.State{Providers: []gateway.Provider{{
			Org: c.slug, Name: "claude", Type: "anthropic", BaseURL: upstream.URL + "/v1", APIKeyFile: keyFile,
			Models: []gateway.Model{
				{Name: thinkModel, Format: modelformat.Anthropic, Price: price},
				{Name: plainModel, Format: modelformat.Anthropic, Price: price},
			},
		}}}
		state.AddKey(key.Key)
		_, err = gw.ApplyModel(ctx, state.ModelConfig())
		Expect(err).NotTo(HaveOccurred())
		return srv.URL, key.Material
	}

	// send posts one Messages request through the gateway and reads the whole answer.
	send := func(ctx SpecContext, url, material, model string, stream bool) {
		raw, err := json.Marshal(map[string]any{
			"model": model, "stream": stream, "max_tokens": 64,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		Expect(err).NotTo(HaveOccurred())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/messages", bytes.NewReader(raw))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+material)
		req.Header.Set("anthropic-version", messagesAPI)
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		Expect(resp.StatusCode).To(Equal(http.StatusOK), string(b))
	}

	// entry is the one spend entry of the org's model, once the exporter has delivered it.
	entry := func(ctx SpecContext, c *env, model string) row {
		var x row
		Eventually(func() error {
			// 100 input at $1 and 40 output at $2 per 1M tokens: the thinking is billed once, as output.
			return db.QueryRow(ctx, `SELECT input_tokens, output_tokens, reasoning_tokens, reasoning_known IS TRUE,
				reasoning_known IS NULL, cost_usd IS NOT DISTINCT FROM 0.00018
				FROM spend_entries WHERE org_id = $1 AND model = $2`, c.org, model).
				Scan(&x.Input, &x.Output, &x.Reasoning, &x.Known, &x.KnownNull, &x.CostOK)
		}).WithTimeout(racescale.D(30 * time.Second)).WithPolling(100 * time.Millisecond).Should(Succeed())
		return x
	}

	BeforeEach(needDocker)

	for _, stream := range []bool{false, true} {
		name := "a Messages answer"
		if stream {
			name = "a Messages stream (the last message_delta)"
		}
		It("records the thinking tokens of "+name+" in the ledger row", func(ctx SpecContext) {
			c := newEnv(ctx, uniq("org"), "", "100.00")
			url, material := setup(ctx, c)
			send(ctx, url, material, thinkModel, stream)
			Expect(entry(ctx, c, thinkModel)).To(Equal(row{Input: 100, Output: 40, Reasoning: 25, Known: true, CostOK: true}),
				"output_tokens is the provider's total, thinking included; reasoning_tokens is the thinking part")
		}, SpecTimeout(2*time.Minute))

		It("keeps the split unknown, not 0, when "+name+" reports no thinking tokens", func(ctx SpecContext) {
			c := newEnv(ctx, uniq("org"), "", "100.00")
			url, material := setup(ctx, c)
			send(ctx, url, material, plainModel, stream)
			Expect(entry(ctx, c, plainModel)).To(Equal(row{Input: 100, Output: 40, Reasoning: 0, Known: false, CostOK: true}))
		}, SpecTimeout(2*time.Minute))
	}
})
