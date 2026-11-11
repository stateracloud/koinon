// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/money"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
	"github.com/stateracloud/koinon/internal/thinking"
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
			records func(ctx context.Context, keyID string) ([]gateway.UsageRecord, error)
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
				fakemodel.Script{Model: "claude-haiku-5-5", Turns: []fakemodel.Turn{{Text: "ok", Usage: usage}}},
				fakemodel.Script{Model: "claude-pinned", Turns: []fakemodel.Turn{{Text: "ok", Usage: usage}}},
				fakemodel.Script{Model: "anthropic/claude-haiku-5.5", Turns: []fakemodel.Turn{{Text: "ok", Usage: usage}}},
				fakemodel.Script{Model: "claude-nows", Turns: []fakemodel.Turn{{Text: "ok", Usage: usage}}},
				fakemodel.Script{Model: "gpt-test", Turns: []fakemodel.Turn{{Text: "ok", Usage: fakemodel.Usage{InputTokens: 10, OutputTokens: 10}}}},
				fakemodel.Script{Model: "claude-cut", Faults: fakemodel.Faults{Truncate: true}, Turns: []fakemodel.Turn{{Text: "a long answer that is cut", Usage: usage}}},
				fakemodel.Script{Model: "globex-model", Turns: []fakemodel.Turn{{Text: "ok", Usage: usage}}},
			)
			Expect(err).NotTo(HaveOccurred())
			// The upstream refuses what Anthropic refuses (unknown fields, beta-only fields without their
			// beta), so a body the gateway cannot vouch for fails here the way it does on api.anthropic.com.
			upstrm.SetStrictMessages(true)
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
					Models: []gateway.Model{
						{Name: "claude-test", Format: modelformat.Anthropic, Price: price},
						{Name: "claude-cut", Format: modelformat.Anthropic, Price: price},
						// A Claude-shaped ID the catalog says can think; claude-test has the same shape of
						// provider and no thinking capability.
						{Name: "claude-haiku-5-5", Format: modelformat.Anthropic, Thinking: thinking.Of("anthropic", "claude-haiku-5-5", modelformat.Anthropic), Price: price},
						// No catalog row pins a beta today (old issue 316); this model's capability does, so the pin
						// itself stays covered for the row that will.
						{Name: "claude-pinned", Format: modelformat.Anthropic, Thinking: thinking.Capability{
							Thinks: thinking.Always, On: thinking.AdaptiveSummarized, Betas: []string{thinking.BindingBeta},
						}, Price: price},
					},
				},
				{
					// OpenRouter's Messages route: the catalog says adaptive thinking and pins no beta.
					Org: "acme", Name: "openrouter", Type: "openrouter", BaseURL: url, APIKeyFile: env.KeyFile("provider_key"),
					Models: []gateway.Model{{
						Name: "anthropic/claude-haiku-5.5", Format: modelformat.Anthropic,
						Thinking: thinking.Of("openrouter", "anthropic/claude-haiku-5.5", modelformat.Anthropic), Price: price,
					}},
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
					Models: []gateway.Model{{Name: "globex-model", Format: modelformat.OpenAI, Price: price}},
				},
			}}
			state.AddKey(keyC.Key)
			state.AddKey(keyL.Key)
			state.AddKey(keyG.Key)
			// One request costs about $0.0044, so a limit of $0.006 lets two through.
			state.SetLimit(agentL, &gateway.Limit{Unit: gateway.USD, Amount: money.MustParse("0.006"), Window: 24 * time.Hour})

			gw, err := env.Start(ctx, []int{port(srv)}, map[string]string{"provider_key": providerKey}, "")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(gw.Stop)
			llmURL, logs, records = gw.URL, gw.Logs, gw.usage
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

		It("drops the headers and body fields that let an agent reach beyond its org or the sandbox", func(ctx SpecContext) {
			// An agent is untrusted. anthropic-workspace-id would pick the workspace an unscoped key acts
			// in; anthropic-beta enables the MCP connector (Anthropic calls the URL itself, past the
			// sandbox's egress rules), code execution, web fetch and long context.
			attack := func(model string) fakemodel.Request {
				GinkgoHelper()
				body := msgBody(model, false)
				body["tools"] = []any{
					map[string]any{"name": "lookup", "description": "kept", "input_schema": map[string]any{"type": "object"}},
					map[string]any{"type": "custom", "name": "also_kept", "input_schema": map[string]any{"type": "object"}},
					map[string]any{"type": "web_search_20250305", "name": "web_search"},
					map[string]any{"type": "web_fetch_20250910", "name": "web_fetch"},
					map[string]any{"type": "code_execution_20250522", "name": "code_execution"},
					map[string]any{"type": "mcp_toolset", "mcp_server_name": "evil"},
				}
				code, out := post(ctx, "/v1/messages", keyC.Material, body,
					"anthropic-version", clientDate, "anthropic-workspace-id", "wrkspc_Attacker", "anthropic-beta", "mcp-client-2025-04-04")
				Expect(code).To(Equal(http.StatusOK), out)
				return lastUpstream(model)
			}
			toolNames := func(up fakemodel.Request) []string {
				GinkgoHelper()
				var got struct {
					Tools []struct{ Name string } `json:"tools"`
				}
				Expect(json.Unmarshal(up.Body, &got)).To(Succeed())
				var names []string
				for _, t := range got.Tools {
					names = append(names, t.Name)
				}
				return names
			}

			By("a request naming mcp_servers is refused before it reaches the upstream")
			before := len(upstrm.Requests())
			body := msgBody("claude-test", false)
			body["mcp_servers"] = []any{map[string]any{"type": "url", "url": "https://attacker.example/mcp", "name": "evil"}}
			code, out := post(ctx, "/v1/messages", keyC.Material, body, "anthropic-beta", "mcp-client-2025-04-04")
			Expect(code).To(Equal(http.StatusForbidden), out)
			Expect(upstrm.Requests()).To(HaveLen(before))

			up := attack("claude-test")
			Expect(up.Header.Values("anthropic-workspace-id")).To(Equal([]string{workspace}), "the provider's workspace, not the agent's")
			Expect(up.Header).NotTo(HaveKey("Anthropic-Beta"))
			Expect(toolNames(up)).To(Equal([]string{"lookup", "also_kept"}), "only the tools the harness runs itself")

			up = attack("claude-nows")
			Expect(up.Header).NotTo(HaveKey("Anthropic-Workspace-Id"), "a provider without a workspace sends none, whatever the agent asks")
			Expect(up.Header).NotTo(HaveKey("Anthropic-Beta"))
			Expect(toolNames(up)).To(Equal([]string{"lookup", "also_kept"}))

			By("a request with no tools stays without tools")
			body = msgBody("claude-test", false)
			delete(body, "tools")
			code, out = post(ctx, "/v1/messages", keyC.Material, body, "anthropic-version", clientDate)
			Expect(code).To(Equal(http.StatusOK), out)
			Expect(string(lastUpstream("claude-test").Body)).NotTo(ContainSubstring(`"tools"`))
			Expect(string(lastUpstream("claude-test").Body)).NotTo(ContainSubstring("mcp_servers"), "no field is added to a plain request")
		})

		It("sends the catalog's explicit on and off forms to a strict upstream, without any beta (old issue 316)", func(ctx SpecContext) {
			withThinking := func(model string, th map[string]any) map[string]any {
				b := msgBody(model, false)
				b["thinking"] = th
				return b
			}
			on := map[string]any{"type": "adaptive", "display": "summarized"}
			code, out := post(ctx, "/v1/messages", keyC.Material, withThinking("claude-haiku-5-5", on),
				"anthropic-version", clientDate, "anthropic-beta", "mcp-client-2025-04-04,interleaved-thinking-2025-05-14")
			Expect(code).To(Equal(http.StatusOK), out)
			up := lastUpstream("claude-haiku-5-5")
			Expect(up.Header).NotTo(HaveKey("Anthropic-Beta"), "the row pins none and the client's never passes")
			Expect(string(up.Body)).To(And(ContainSubstring(`"adaptive"`), ContainSubstring(`"summarized"`)))

			By("thinking off is sent explicitly, and Haiku 5.5 takes it")
			code, out = post(ctx, "/v1/messages", keyC.Material, withThinking("claude-haiku-5-5", map[string]any{"type": "disabled"}), "anthropic-version", clientDate)
			Expect(code).To(Equal(http.StatusOK), out)
			Expect(string(lastUpstream("claude-haiku-5-5").Body)).To(ContainSubstring(`"disabled"`))

			By("OpenCode's own block_binding is refused as Anthropic refuses it: no beta covers it")
			code, out = post(ctx, "/v1/messages", keyC.Material, withThinking("claude-haiku-5-5", map[string]any{
				"type": "adaptive", "block_binding": map[string]any{"prefix_mismatch_behavior": "drop_block"},
			}), "anthropic-version", clientDate, "anthropic-beta", thinking.BindingBeta)
			Expect(code).To(Equal(http.StatusBadRequest), out)
			Expect(out).To(ContainSubstring("thinking.adaptive.block_binding: Extra inputs are not permitted"))
		})

		It("pins a row's beta, so a beta-only field reaches a strict upstream (rc.7, old issue 240)", func(ctx SpecContext) {
			adaptive := func(model string) map[string]any {
				b := msgBody(model, false)
				b["thinking"] = map[string]any{"type": "adaptive", "block_binding": map[string]any{"prefix_mismatch_behavior": "drop_block"}}
				return b
			}
			code, out := post(ctx, "/v1/messages", keyC.Material, adaptive("claude-pinned"), "anthropic-version", clientDate)
			Expect(code).To(Equal(http.StatusOK), out)
			up := lastUpstream("claude-pinned")
			Expect(up.Header.Values("anthropic-beta")).To(Equal([]string{thinking.BindingBeta}))

			By("the client's own beta never passes: the pinned value replaces it")
			code, out = post(ctx, "/v1/messages", keyC.Material, adaptive("claude-pinned"),
				"anthropic-version", clientDate, "anthropic-beta", "mcp-client-2025-04-04,interleaved-thinking-2025-05-14")
			Expect(code).To(Equal(http.StatusOK), out)
			Expect(lastUpstream("claude-pinned").Header.Values("anthropic-beta")).To(Equal([]string{thinking.BindingBeta}))

			By("a model the catalog says cannot think gets no beta, and a thinking body is refused as Anthropic refuses it")
			code, out = post(ctx, "/v1/messages", keyC.Material, adaptive("claude-test"), "anthropic-version", clientDate)
			Expect(code).To(Equal(http.StatusBadRequest), out)
			Expect(out).To(ContainSubstring("thinking.adaptive.block_binding: Extra inputs are not permitted"))
			Expect(lastUpstream("claude-test").Header).NotTo(HaveKey("Anthropic-Beta"))
		})

		It("drops the client's anthropic-beta for a model that thinks without a pinned beta (OpenRouter, old issue 240)", func(ctx SpecContext) {
			// The catalog says the model thinks and lists no beta, so nothing replaces the client's
			// header and the gateway must remove it: otherwise OpenCode's own betas reach the upstream
			// and hide a request that only works because of them (rc.7).
			const model = "anthropic/claude-haiku-5.5"
			Expect(thinking.Of("openrouter", model, modelformat.Anthropic)).To(
				And(HaveField("On", thinking.AdaptiveSummarized), HaveField("Betas", BeEmpty())))
			body := msgBody(model, false)
			body["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
			code, out := post(ctx, "/v1/messages", keyC.Material, body, "anthropic-version", clientDate,
				"anthropic-beta", "interleaved-thinking-2025-05-14,"+thinking.BindingBeta)
			Expect(code).To(Equal(http.StatusOK), out)
			up := lastUpstream(model)
			Expect(up.Header).NotTo(HaveKey("Anthropic-Beta"))
			Expect(string(up.Body)).To(ContainSubstring(`"adaptive"`))

			By("a beta-only field is then refused by the strict upstream, as it would be if the client's beta leaked or were missing")
			body["thinking"] = map[string]any{"type": "adaptive", "block_binding": map[string]any{"prefix_mismatch_behavior": "drop_block"}}
			code, out = post(ctx, "/v1/messages", keyC.Material, body, "anthropic-version", clientDate,
				"anthropic-beta", thinking.BindingBeta)
			Expect(code).To(Equal(http.StatusBadRequest), out)
			Expect(lastUpstream(model).Header).NotTo(HaveKey("Anthropic-Beta"), "the client's beta never passes")
		})

		It("keeps the tools of a chat-completions request sent to an anthropic model", func(ctx SpecContext) {
			// The tools filter is a per-model body field; it must not touch the type "function" tools of
			// another route.
			code, out := post(ctx, "/v1/chat/completions", keyC.Material, map[string]any{
				"model": "claude-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}},
			})
			Expect(code).To(Equal(http.StatusOK), out)
			Expect(string(lastUpstream("claude-test").Body)).To(ContainSubstring(`"lookup"`), "the function tool is kept")
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

		It("refuses a request with the agent's key in any header but Authorization, and sends the upstream only the headers a format needs (old issue 262)", func(ctx SpecContext) {
			// An agent is untrusted and holds its gateway key, so it can put the key in any header or in
			// the query. The upstream is a third party that logs what it receives. Both modes refuse a key
			// in a header but Authorization, including one the allowlist keeps (a client chooses its
			// User-Agent); standalone mode also passes only gateway.ForwardedHeaders, whatever else the
			// client sent.
			keyed := [][2]string{
				{"X-Canary", keyC.Material},
				{"X-Embedded", "prefix-" + keyC.Material + "-suffix"},
				{"X-Api-Key", keyC.Material},
				{"Cookie", "session=" + keyC.Material},
				{"Proxy-Authorization", "Basic " + keyC.Material},
				{"User-Agent", "harness/1.0 (" + keyC.Material + ")"},
				{"Accept", "application/json, " + keyC.Material},
			}
			allowed := map[string]bool{}
			for _, h := range gateway.ForwardedHeaders {
				allowed[http.CanonicalHeaderKey(h)] = true
			}
			// What the gateway and the HTTP stack add themselves.
			for _, h := range []string{
				"Host", "Authorization", "Anthropic-Version", "Anthropic-Workspace-Id", "Anthropic-Beta",
				gateway.OrgHeader, "Traceparent", "Tracestate", "Content-Length", "Accept-Encoding", "Connection", "Transfer-Encoding",
			} {
				allowed[http.CanonicalHeaderKey(h)] = true
			}
			for _, tc := range []struct{ model, path string }{
				{"claude-test", "/v1/messages"},
				{"gpt-test", "/v1/chat/completions"},
			} {
				body := map[string]any{"model": tc.model, "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
				before := len(upstrm.Requests())
				for _, h := range keyed {
					code, out := post(ctx, tc.path, keyC.Material, body, h[0], h[1])
					Expect(code).To(Equal(http.StatusForbidden), tc.model+": the key in "+h[0]+" is refused: "+out)
				}
				Expect(upstrm.Requests()).To(HaveLen(before), tc.model+": no request with a key in a header reached the upstream")

				// Headers of the agent's own choosing without a key pass in Kubernetes mode and are
				// dropped in standalone mode.
				code, out := post(ctx, tc.path, keyC.Material, body,
					"User-Agent", "koinon-test", "Accept", "application/json", "X-Forwarded-For", "203.0.113.9", "X-Custom", "hello")
				Expect(code).To(Equal(http.StatusOK), tc.model+": "+out)
				up := lastUpstream(tc.model)
				for name, values := range up.Header {
					if env.HeaderAllowlist {
						Expect(allowed).To(HaveKey(name), tc.model+": a header outside the allowlist reached the upstream")
					}
					for _, v := range values {
						Expect(v).NotTo(ContainSubstring(gateway.KeyPrefix), tc.model+": header "+name)
					}
				}
				Expect(up.Header.Get("Authorization")).To(Equal("Bearer "+providerKey), "the provider's key replaces the agent's")
				Expect(up.Header.Get("Content-Type")).To(Equal("application/json"), "a header the format needs still passes")
				Expect(up.Header.Get("User-Agent")).To(Equal("koinon-test"), "an allowlisted header without a key still passes")

				By(tc.model + ": a request with the key in the query is refused before it reaches the upstream")
				before = len(upstrm.Requests())
				code, out = post(ctx, tc.path+"?canary="+keyC.Material, keyC.Material, body)
				Expect(code).To(Equal(http.StatusForbidden), out)
				code, out = post(ctx, tc.path+"?canary="+strings.Replace(keyC.Material, "_", "%5F", 1), keyC.Material, body)
				Expect(code).To(Equal(http.StatusForbidden), "a percent-encoded key: "+out)
				Expect(upstrm.Requests()).To(HaveLen(before))
			}
		})

		It("keeps another org's key off the model", func(ctx SpecContext) {
			before := len(upstrm.Requests())
			code, body := messages(ctx, keyG.Material, "claude-test", false)
			// Both modes refuse with 403 today (allowedModels in standalone, the org authorization rule
			// in Kubernetes mode); like the chat-completions specs, accept a 404 too.
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

		// usageOf returns the usage records the gateway kept of keyC's requests.
		usageOf := func(ctx context.Context) []gateway.UsageRecord {
			recs, err := records(ctx, keyC.ID)
			Expect(err).NotTo(HaveOccurred())
			return recs
		}

		It("keeps the usage record of a Messages stream that was cut: its format, that it streamed, and that it failed", func(ctx SpecContext) {
			Expect(records).NotTo(BeNil(), "the driver must expose the gateway's usage records")
			// The upstream drops the connection after half of the events. The client's read fails or
			// ends early; what matters is the record.
			before := len(usageOf(ctx))
			raw, err := json.Marshal(msgBody("claude-cut", true))
			Expect(err).NotTo(HaveOccurred())
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, llmURL+"/v1/messages", bytes.NewReader(raw))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+keyC.Material)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}

			var recs []gateway.UsageRecord
			Eventually(func() int {
				recs = usageOf(ctx)
				return len(recs)
			}, 30*time.Second, time.Second).Should(BeNumerically(">", before))
			rec := recs[len(recs)-1]
			// The gateway answered 200 before the cut, so the status says nothing; what tells the ledger is
			// the error reason, set only when the gateway failed the response, with the usage of
			// message_start alone (the final output count of 10 never arrived).
			Expect(rec.Format).To(Equal(modelformat.Anthropic))
			Expect(rec.Stream).To(BeTrue())
			Expect(rec.Status).To(BeEquivalentTo(http.StatusOK))
			Expect(rec.Error).NotTo(BeEmpty(), "a cut stream is a failed response")
			Expect(rec.OutputTokens).To(BeEquivalentTo(1))
			Expect(rec.InputTokens).To(BeEquivalentTo(100))
		})

		It("keeps the cache tokens in their own buckets, apart from the input", func(ctx SpecContext) {
			Expect(records).NotTo(BeNil(), "the driver must expose the gateway's usage records")
			code, body := messages(ctx, keyC.Material, "claude-test", true)
			Expect(code).To(Equal(http.StatusOK), body)
			code, body = messages(ctx, keyC.Material, "claude-test", false)
			Expect(code).To(Equal(http.StatusOK), body)

			// The upstream reports input_tokens 50 beside 30 cache reads and 20 writes: the usage
			// record keeps the three apart (disjoint buckets, ADR 0097 decision 3).
			finished := func() (n int) {
				for _, rec := range usageOf(ctx) {
					if rec.Format != modelformat.Anthropic || rec.OutputTokens != 10 {
						continue // not the usage of a finished response (the cut stream has output 1; a chat-completions record has format openai)
					}
					n++
					Expect(rec.Error).To(BeEmpty(), "a normal end carries no error")
					Expect(rec.InputTokens).To(BeEquivalentTo(50))
					Expect(rec.CacheReadTokens).To(BeEquivalentTo(30))
					Expect(rec.CacheWriteTokens).To(BeEquivalentTo(20))
				}
				return n
			}
			Eventually(finished, 30*time.Second, time.Second).Should(BeNumerically(">=", 2),
				"the streamed and the unstreamed response are both recorded")
		}, SpecTimeout(2*time.Minute))
	})
}
