// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// usageCase is one row of the usage matrix: a request in a format and a mode against a scripted
// provider answer, and the record the gateway must keep of it.
type usageCase struct {
	name   string
	format modelformat.Format
	model  string
	stream bool
	script fakemodel.Script
	want   Observed
	// wantReasoning is the thinking split the provider reported; see the #78 case.
	wantReasoning int64
}

// usageSpec is the usage contract of ADR 0097 decision 4: per API format and provider dialect Koinon
// routes (chat completions and Messages; streamed and not; cache read and write; reasoning; a stream
// cut mid-way), the record the gateway keeps matches the fixture. The spend ledger prices that
// record, so a gateway bump that changes a count changes a bill (#47, #78).
func usageSpec(env Env) {
	// The provider answers; the gateway's own log must follow the consistent semantics: Input
	// includes the cache buckets and Output includes reasoning. Observed carries those totals, not
	// the disjoint UsageRecord; a driver that reads its usage record adds the parts back. The ledger
	// prices the disjoint record the driver maps from this log (ADR 0097 decision 3; the driver's
	// decode specs assert that).
	chat := fakemodel.Usage{InputTokens: 100, CachedInputTokens: 30, OutputTokens: 40, ReasoningTokens: 25}
	messages := fakemodel.Usage{InputTokens: 100, CachedInputTokens: 30, CacheWriteTokens: 20, OutputTokens: 40, ReasoningTokens: 25}
	plain := fakemodel.Usage{InputTokens: 100, OutputTokens: 10}
	turn := func(u fakemodel.Usage) []fakemodel.Turn {
		return []fakemodel.Turn{{Text: "a long enough answer to be cut", Usage: u}}
	}

	cases := []usageCase{
		{
			name: "chat completions", format: modelformat.OpenAI, model: "oai-plain",
			script: fakemodel.Script{Model: "oai-plain", Turns: turn(plain)},
			want:   Observed{Format: "openai", Status: 200, Input: 100, Output: 10},
		},
		{
			name: "chat completions, streamed", format: modelformat.OpenAI, model: "oai-plain-stream", stream: true,
			script: fakemodel.Script{Model: "oai-plain-stream", Turns: turn(plain)},
			want:   Observed{Format: "openai", Stream: true, Status: 200, Input: 100, Output: 10},
		},
		{
			name: "chat completions with cache reads and reasoning", format: modelformat.OpenAI, model: "oai-think",
			script: fakemodel.Script{Model: "oai-think", Turns: turn(chat)},
			want:   Observed{Format: "openai", Status: 200, Input: 100, CacheRead: 30, Output: 40}, wantReasoning: 25,
		},
		{
			name: "chat completions with cache reads and reasoning, streamed", format: modelformat.OpenAI, model: "oai-think-stream", stream: true,
			script: fakemodel.Script{Model: "oai-think-stream", Turns: turn(chat)},
			want:   Observed{Format: "openai", Stream: true, Status: 200, Input: 100, CacheRead: 30, Output: 40}, wantReasoning: 25,
		},
		{
			name: "Messages with cache reads and writes", format: modelformat.Anthropic, model: "claude-plain",
			script: fakemodel.Script{Model: "claude-plain", Turns: turn(fakemodel.Usage{InputTokens: 100, CachedInputTokens: 30, CacheWriteTokens: 20, OutputTokens: 10})},
			want:   Observed{Format: "anthropic", Status: 200, Input: 100, CacheRead: 30, CacheWrite: 20, Output: 10},
		},
		{
			name: "Messages with cache reads and writes, streamed", format: modelformat.Anthropic, model: "claude-plain-stream", stream: true,
			script: fakemodel.Script{Model: "claude-plain-stream", Turns: turn(fakemodel.Usage{InputTokens: 100, CachedInputTokens: 30, CacheWriteTokens: 20, OutputTokens: 10})},
			want:   Observed{Format: "anthropic", Stream: true, Status: 200, Input: 100, CacheRead: 30, CacheWrite: 20, Output: 10},
		},
		{
			name: "Messages with thinking", format: modelformat.Anthropic, model: "claude-think",
			script: fakemodel.Script{Model: "claude-think", Turns: turn(messages)},
			want:   Observed{Format: "anthropic", Status: 200, Input: 100, CacheRead: 30, CacheWrite: 20, Output: 40}, wantReasoning: 25,
		},
		{
			name: "Messages with thinking, streamed", format: modelformat.Anthropic, model: "claude-think-stream", stream: true,
			script: fakemodel.Script{Model: "claude-think-stream", Turns: turn(messages)},
			want:   Observed{Format: "anthropic", Stream: true, Status: 200, Input: 100, CacheRead: 30, CacheWrite: 20, Output: 40}, wantReasoning: 25,
		},
	}
	// A stream the provider cuts mid-way: the gateway answered 200 before the cut, so what tells the
	// ledger is the failure flag, with the usage that arrived (chat completions: none; Messages:
	// message_start's, with output 1).
	cut := []usageCase{
		{
			name: "a chat completions stream cut mid-way", format: modelformat.OpenAI, model: "oai-cut", stream: true,
			script: fakemodel.Script{Model: "oai-cut", Faults: fakemodel.Faults{Truncate: true}, Turns: turn(plain)},
			want:   Observed{Format: "openai", Stream: true, Status: 200, Failed: true},
		},
		{
			name: "a Messages stream cut mid-way", format: modelformat.Anthropic, model: "claude-cut", stream: true,
			script: fakemodel.Script{Model: "claude-cut", Faults: fakemodel.Faults{Truncate: true}, Turns: turn(plain)},
			want:   Observed{Format: "anthropic", Stream: true, Status: 200, Failed: true, Input: 100, Output: 1},
		},
	}
	const thinkingSuffix = " (thinking split)"
	all := append(append([]usageCase{}, cases...), cut...)

	Describe(env.Name+": the usage record per API format and dialect (ADR 0097; koinon #47, #78)", Ordered, ContinueOnFailure, func() {
		var (
			gw   *Gateway
			keys = map[string]gateway.IssuedKey{}
		)
		BeforeAll(func(ctx SpecContext) {
			var scripts []fakemodel.Script
			for _, c := range all {
				scripts = append(scripts, c.script)
			}
			fm, err := fakemodel.New(scripts...)
			Expect(err).NotTo(HaveOccurred())
			srv := newServer(fm)
			DeferCleanup(srv.Close)

			url := fmt.Sprintf("http://%s:%d/v1", env.Host, port(srv))
			price := &gateway.Price{Input: "1", Output: "1", CacheRead: "1", CacheWrite: "1"}
			oai := gateway.Provider{Org: "acme", Name: "usage-openai", Type: "openai", BaseURL: url, APIKeyFile: env.KeyFile("provider_key")}
			ant := gateway.Provider{Org: "acme", Name: "usage-claude", Type: "anthropic", BaseURL: url, APIKeyFile: env.KeyFile("provider_key")}
			state := gateway.State{}
			for i, c := range all {
				// A key for the case's record, and a second for its thinking split, so that each spec
				// sends its own request and reads the record of that request alone.
				for j, name := range []string{c.name, c.name + thinkingSuffix} {
					k, err := gateway.NewKey(gateway.AgentRef{Org: "acme", Name: fmt.Sprintf("usage-%d-%d", i, j)})
					Expect(err).NotTo(HaveOccurred())
					keys[name] = k
					state.AddKey(k.Key)
				}
				m := gateway.Model{Name: c.model, Format: c.format, Price: price}
				if c.format == modelformat.OpenAI {
					oai.Models = append(oai.Models, m)
				} else {
					ant.Models = append(ant.Models, m)
				}
			}
			state.Providers = []gateway.Provider{oai, ant}

			gw, err = env.Start(ctx, []int{port(srv)}, map[string]string{"provider_key": "koinon_test_not-a-real-token-upstream"}, "")
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

		// send posts the case's request and drains the answer; a cut stream may fail the read.
		send := func(ctx context.Context, c usageCase, keyName string) {
			var path string
			var body map[string]any
			if c.format == modelformat.OpenAI {
				path = "/v1/chat/completions"
				body = map[string]any{"model": c.model, "stream": c.stream, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			} else {
				path = "/v1/messages"
				body = map[string]any{"model": c.model, "stream": c.stream, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			}
			raw, err := json.Marshal(body)
			Expect(err).NotTo(HaveOccurred())
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+path, bytes.NewReader(raw))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+keys[keyName].Material)
			req.Header.Set("anthropic-version", "2023-06-01")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				Expect(c.script.Faults.Truncate).To(BeTrue(), "the request failed: %v", err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body)
			if !c.script.Faults.Truncate {
				Expect(resp.StatusCode).To(Equal(http.StatusOK), string(b))
			}
		}
		// record is the one record the gateway kept for the case's key.
		record := func(ctx context.Context, keyName string) Observed {
			var recs []Observed
			Eventually(func() int {
				var err error
				recs, err = gw.observed(ctx, keys[keyName].ID)
				Expect(err).NotTo(HaveOccurred())
				return len(recs)
			}).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(BeNumerically(">=", 1))
			Expect(recs).To(HaveLen(1), "one request, one record")
			return recs[0]
		}

		for _, c := range all {
			It("records "+c.name, func(ctx SpecContext) {
				send(ctx, c, c.name)
				got := record(ctx, c.name)
				if c.wantReasoning != 0 {
					// The thinking split is judged on its own below, where the gateway may lack it.
					got.Reasoning = nil
				}
				Expect(got).To(Equal(c.want))
			}, SpecTimeout(time.Minute))
		}

		// Chat completions carry reasoning_tokens inside completion_tokens_details, which
		// agentgateway reads; the Messages API reports thinking_tokens inside
		// output_tokens_details, which agentgateway v1.7.0 ignores (#78).
		for _, c := range cases {
			if c.wantReasoning == 0 {
				continue
			}
			check := func(ctx SpecContext) {
				send(ctx, c, c.name+thinkingSuffix)
				got := record(ctx, c.name+thinkingSuffix)
				expect := func() {
					Expect(got.Reasoning).NotTo(BeNil(), "the gateway recorded no thinking split")
					if got.Reasoning != nil { // a failed Expect inside KnownGap does not stop the check
						Expect(*got.Reasoning).To(Equal(c.wantReasoning))
					}
				}
				if c.format == modelformat.Anthropic {
					KnownGap(env, "#78", expect)
					return
				}
				expect()
			}
			if c.format == modelformat.Anthropic {
				It("records the thinking split of "+c.name+" (expected failure, #78)", check, SpecTimeout(time.Minute))
			} else {
				It("records the reasoning split of "+c.name, check, SpecTimeout(time.Minute))
			}
		}
	})
}
