// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// anthropicSpec is ADR 0069 decision 8: a model in the Messages format, served beside an openai one in
// the same org, against a fake model server that records what the gateway sent it.
func anthropicSpec(env Env) {
	Describe(env.Name+": models in the Messages format (ADR 0069)", Ordered, func() {
		const (
			providerKey = "koinon_test_not-a-real-token-upstream"
			workspace   = "wrkspc_01ExampleExample"
			clientDate  = "2099-01-01"
		)
		var (
			llmURL  string
			logs    func(context.Context) string
			upstrm  *fakemodel.Server
			keyC    gateway.IssuedKey // acme, unlimited
			keyL    gateway.IssuedKey // acme, over a small budget
			keyG    gateway.IssuedKey // globex
			agentC  = gateway.AgentRef{Org: "acme", Name: "claude-agent"}
			agentL  = gateway.AgentRef{Org: "acme", Name: "claude-limited"}
			usage   = fakemodel.Usage{InputTokens: 100, CachedInputTokens: 30, CacheWriteTokens: 20, OutputTokens: 10}
			marker  = map[string]any{"type": "ephemeral"}
			msgBody = func(model string, stream bool) map[string]any {
				block := func(text string) map[string]any {
					return map[string]any{"type": "text", "text": text, "cache_control": marker}
				}
				return map[string]any{
					"model": model, "max_tokens": 64, "stream": stream,
					"system": []any{block("You are a helpful agent.")},
					"tools": []any{map[string]any{
						"name": "lookup", "description": "look something up", "cache_control": marker,
						"input_schema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
					}},
					"messages": []any{
						map[string]any{"role": "user", "content": []any{block("first question")}},
						map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "answer"}}},
						map[string]any{"role": "user", "content": []any{block("second question")}},
					},
				}
			}
		)

		// post sends a request to the gateway and returns the status and body.
		post := func(ctx context.Context, path, key string, body map[string]any, header ...string) (int, string) {
			GinkgoHelper()
			raw, err := json.Marshal(body)
			Expect(err).NotTo(HaveOccurred())
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, llmURL+path, bytes.NewReader(raw))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			for i := 0; i+1 < len(header); i += 2 {
				req.Header.Set(header[i], header[i+1])
			}
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b)
		}
		// messages posts a Messages request as a client that sends its own, different version.
		messages := func(ctx context.Context, key, model string, stream bool) (int, string) {
			GinkgoHelper()
			return post(ctx, "/v1/messages", key, msgBody(model, stream), "anthropic-version", clientDate)
		}
		// lastUpstream returns the newest request the fake server got for model.
		lastUpstream := func(model string) fakemodel.Request {
			GinkgoHelper()
			reqs := upstrm.Requests()
			for i := len(reqs) - 1; i >= 0; i-- {
				if reqs[i].Model == model {
					return reqs[i]
				}
			}
			Fail("the upstream got no request for " + model)
			return fakemodel.Request{}
		}

		BeforeAll(func(ctx SpecContext) {
			var err error
			upstrm, err = fakemodel.New(
				fakemodel.Script{Model: "claude-test", Turns: []fakemodel.Turn{{Text: "ok", Usage: usage}}},
				fakemodel.Script{Model: "claude-nows", Turns: []fakemodel.Turn{{Text: "ok", Usage: usage}}},
				fakemodel.Script{Model: "gpt-test", Turns: []fakemodel.Turn{{Text: "ok", Usage: fakemodel.Usage{InputTokens: 10, OutputTokens: 10}}}},
				fakemodel.Script{Model: "globex-model", Turns: []fakemodel.Turn{{Text: "ok", Usage: usage}}},
			)
			Expect(err).NotTo(HaveOccurred())
			srv := newServer(upstrm)
			DeferCleanup(srv.Close)

			for ref, dst := range map[*gateway.IssuedKey]gateway.AgentRef{&keyC: agentC, &keyL: agentL, &keyG: agentG} {
				*ref, err = gateway.NewKey(dst)
				Expect(err).NotTo(HaveOccurred())
			}
			url := fmt.Sprintf("http://%s:%d/v1", env.Host, port(srv))
			price := &gateway.Price{Input: "50", Output: "50", CacheRead: "5", CacheWrite: "60"}
			state := gateway.State{Providers: []gateway.Provider{
				{
					Org: "acme", Name: "claude", Type: "anthropic", BaseURL: url, APIKeyFile: env.KeyFile("provider_key"),
					AnthropicWorkspaceID: workspace,
					Models:               []gateway.Model{{Name: "claude-test", Format: modelformat.Anthropic, Price: price}},
				},
				{
					Org: "acme", Name: "claude-unscoped", Type: "anthropic", BaseURL: url, APIKeyFile: env.KeyFile("provider_key"),
					Models: []gateway.Model{{Name: "claude-nows", Format: modelformat.Anthropic, Price: price}},
				},
				{
					Org: "acme", Name: "openai", Type: "openai", BaseURL: url, APIKeyFile: env.KeyFile("provider_key"),
					Models: []gateway.Model{{Name: "gpt-test", Format: modelformat.OpenAI, Price: &gateway.Price{Input: "1", Output: "1"}}},
				},
				{
					Org: "globex", Name: "other", Type: "openai", BaseURL: url,
					Models: []gateway.Model{{Name: "globex-model", Price: price}},
				},
			}}
			state.AddKey(keyC.Key)
			state.AddKey(keyL.Key)
			state.AddKey(keyG.Key)
			// One request costs about $0.0044, so a limit of $0.006 lets two through.
			state.SetLimit(agentL, &gateway.Limit{Unit: gateway.USD, Amount: 0.006, Window: 24 * time.Hour})

			gw, err := env.Start(ctx, []int{port(srv)}, map[string]string{"provider_key": providerKey})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			llmURL, logs = gw.URL, gw.Logs
			_, err = gw.Driver.Apply(ctx, state)
			Expect(err).NotTo(HaveOccurred())
		}, NodeTimeout(5*time.Minute))

		AfterEach(func(ctx SpecContext) {
			if CurrentSpecReport().Failed() && logs != nil {
				AddReportEntry("gateway logs", logs(ctx))
			}
		})

		It("passes cache_control on system, tool and message blocks to the upstream unchanged", func(ctx SpecContext) {
			code, body := messages(ctx, keyC.Material, "claude-test", false)
			Expect(code).To(Equal(http.StatusOK), body)

			sent, err := json.Marshal(msgBody("claude-test", false))
			Expect(err).NotTo(HaveOccurred())
			var want, got map[string]json.RawMessage
			Expect(json.Unmarshal(sent, &want)).To(Succeed())
			Expect(json.Unmarshal(lastUpstream("claude-test").Body, &got)).To(Succeed())
			// agentgateway re-serialises the body, so key order differs (its typed fields come first and
			// cache_control comes last): the values are what must arrive unchanged.
			for _, field := range []string{"system", "tools", "messages"} {
				Expect(got[field]).To(MatchJSON(want[field]), field+" reaches the upstream as sent")
			}
			Expect(string(got["system"])).To(ContainSubstring(`"cache_control":{"type":"ephemeral"}`))
			Expect(string(got["tools"])).To(ContainSubstring(`"cache_control":{"type":"ephemeral"}`))
			Expect(strings.Count(string(got["messages"]), `"cache_control":{"type":"ephemeral"}`)).To(Equal(2))
		})

		It("sends the upstream the provider key, the pinned version and the workspace, never the agent's key", func(ctx SpecContext) {
			code, body := messages(ctx, keyC.Material, "claude-test", false)
			Expect(code).To(Equal(http.StatusOK), body)
			up := lastUpstream("claude-test")
			Expect(up.Path).To(Equal("/v1/messages"))
			Expect(up.Header.Get("Authorization")).To(Equal("Bearer " + providerKey))
			Expect(up.Header.Values("anthropic-version")).To(Equal([]string{gateway.AnthropicVersion}), "the pin wins over the client's "+clientDate)
			Expect(up.Header.Values("anthropic-workspace-id")).To(Equal([]string{workspace}))
			for name, values := range up.Header {
				for _, v := range values {
					Expect(v).NotTo(ContainSubstring(keyC.Material), "header "+name)
				}
			}
			Expect(string(up.Body)).NotTo(ContainSubstring(keyC.Material))

			By("a provider without a workspace ID sends none, and still pins the version")
			code, body = messages(ctx, keyC.Material, "claude-nows", false)
			Expect(code).To(Equal(http.StatusOK), body)
			up = lastUpstream("claude-nows")
			Expect(up.Header.Values("anthropic-version")).To(Equal([]string{gateway.AnthropicVersion}))
			Expect(up.Header).NotTo(HaveKey("Anthropic-Workspace-Id"))
		})

		It("adds no stream_options to a streamed Messages request", func(ctx SpecContext) {
			code, body := messages(ctx, keyC.Material, "claude-test", true)
			Expect(code).To(Equal(http.StatusOK), body)
			Expect(body).To(ContainSubstring("message_stop"))
			Expect(string(lastUpstream("claude-test").Body)).NotTo(ContainSubstring("stream_options"))
		})

		It("serves an openai model of the same org beside it, still asking for streamed usage", func(ctx SpecContext) {
			code, body := post(ctx, "/v1/chat/completions", keyC.Material, map[string]any{
				"model": "gpt-test", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
			})
			Expect(code).To(Equal(http.StatusOK), body)
			up := lastUpstream("gpt-test")
			Expect(up.Path).To(Equal("/v1/chat/completions"))
			Expect(string(up.Body)).To(ContainSubstring(`"stream_options":{"include_usage":true}`))
			Expect(up.Header.Get("Authorization")).To(Equal("Bearer " + providerKey))
			Expect(up.Header).NotTo(HaveKey("Anthropic-Version"), "an openai model gets no Anthropic headers")
		})

		It("keeps another org's key off the model", func(ctx SpecContext) {
			before := len(upstrm.Requests())
			code, body := messages(ctx, keyG.Material, "claude-test", false)
			// Standalone refuses with 403 (the key's allowedModels), Kubernetes mode with 404 (no model
			// object matches the org); the spec for chat completions accepts both.
			Expect(code).To(BeElementOf(http.StatusForbidden, http.StatusNotFound), body)
			Expect(upstrm.Requests()).To(HaveLen(before))
		})

		if env.GatewayBudgets {
			It("blocks a key over its budget with 429", func(ctx SpecContext) {
				var codes []int
				for range 5 {
					code, _ := messages(ctx, keyL.Material, "claude-test", false)
					codes = append(codes, code)
					if code == http.StatusTooManyRequests {
						break
					}
				}
				Expect(codes).To(Equal([]int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests}))
			})
		}

		It("logs the cache tokens, with the input including both", func(ctx SpecContext) {
			Expect(logs).NotTo(BeNil(), "the driver must expose the gateway's log")
			code, body := messages(ctx, keyC.Material, "claude-test", true)
			Expect(code).To(Equal(http.StatusOK), body)
			code, body = messages(ctx, keyC.Material, "claude-test", false)
			Expect(code).To(Equal(http.StatusOK), body)

			// The upstream reports input_tokens 50 beside 30 cache reads and 20 writes; the log carries
			// agentgateway's consistent semantics, where the input includes both.
			want := map[string]any{
				gateway.LogFieldInput: float64(100), gateway.LogFieldCacheRead: float64(30),
				gateway.LogFieldCacheWrite: float64(20), gateway.LogFieldOutput: float64(10),
			}
			Eventually(func() []map[string]any { return accessLog(logs(ctx), keyC.ID) }, 30*time.Second, time.Second).
				Should(ContainElement(HaveKeyWithValue(gateway.LogFieldInput, want[gateway.LogFieldInput])))
			for _, rec := range accessLog(logs(ctx), keyC.ID) {
				if rec[gateway.LogFieldInput] != want[gateway.LogFieldInput] {
					continue
				}
				for k, v := range want {
					Expect(rec).To(HaveKeyWithValue(k, v), k)
				}
			}
		}, SpecTimeout(2*time.Minute))
	})
}

// logfmtField matches a koinon.* field of a logfmt access-log line.
var logfmtField = regexp.MustCompile(`\b(koinon\.[a-z_]+)=("[^"]*"|\S+)`)

// accessLog returns the access-log records of the gateway log that carry the key ID. Standalone
// agentgateway logs JSON lines (config.logging.format json); the Kubernetes proxy logs logfmt
// (a JSON number reads as float64 in both).
func accessLog(log, keyID string) []map[string]any {
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(log))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, keyID) {
			continue
		}
		rec := map[string]any{}
		if strings.HasPrefix(line, "{") {
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
		} else {
			for _, m := range logfmtField.FindAllStringSubmatch(line, -1) {
				if n, err := strconv.ParseFloat(m[2], 64); err == nil {
					rec[m[1]] = n
				} else {
					rec[m[1]] = strings.Trim(m[2], `"`)
				}
			}
		}
		if rec[gateway.LogFieldKey] == keyID {
			out = append(out, rec)
		}
	}
	return out
}
