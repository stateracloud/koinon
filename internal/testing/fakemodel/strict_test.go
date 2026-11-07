// SPDX-License-Identifier: Apache-2.0

package fakemodel_test

import (
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

var _ = Describe("strict Messages mode", func() {
	const bindingBeta = "thinking-binding-controls-2026-08-01"

	var (
		srv *fakemodel.Server
		url string
	)
	BeforeEach(func() {
		var ts *httptest.Server
		srv, ts = start(fakemodel.Script{Model: "claude-haiku-5-5", Turns: []fakemodel.Turn{{Text: "ok"}}})
		url = ts.URL
		srv.SetStrictMessages(true)
	})

	body := func(extra map[string]any) map[string]any {
		b := map[string]any{
			"model": "claude-haiku-5-5", "max_tokens": 64,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	send := func(ctx SpecContext, b map[string]any, header ...string) (int, map[string]any) {
		GinkgoHelper()
		resp := post(ctx, url+"/v1/messages", b, append([]string{"anthropic-version", "2023-06-01"}, header...)...)
		var out map[string]any
		decode(resp, &out)
		return resp.StatusCode, out
	}
	message := func(out map[string]any) string {
		GinkgoHelper()
		Expect(out).To(HaveKeyWithValue("type", "error"))
		e := out["error"].(map[string]any)
		Expect(e).To(HaveKeyWithValue("type", "invalid_request_error"))
		return e["message"].(string)
	}

	It("accepts an ordinary request, with and without thinking", func(ctx SpecContext) {
		for _, extra := range []map[string]any{
			nil,
			{"thinking": map[string]any{"type": "adaptive"}},
			{"thinking": map[string]any{"type": "enabled", "budget_tokens": 2048}},
			{"thinking": map[string]any{"type": "disabled"}},
		} {
			code, out := send(ctx, body(extra))
			Expect(code).To(Equal(http.StatusOK), "%v: %v", extra, out)
		}
	})

	It("refuses the rc.7 request: block_binding without its beta, in Anthropic's words", func(ctx SpecContext) {
		b := body(map[string]any{"thinking": map[string]any{
			"type": "adaptive", "block_binding": map[string]any{"prefix_mismatch_behavior": "drop_block"},
		}})
		code, out := send(ctx, b, "anthropic-beta", "interleaved-thinking-2025-05-14")
		Expect(code).To(Equal(http.StatusBadRequest))
		Expect(message(out)).To(Equal("thinking.adaptive.block_binding: Extra inputs are not permitted"))

		By("it accepts the same body with the beta among others")
		code, out = send(ctx, b, "anthropic-beta", "interleaved-thinking-2025-05-14, "+bindingBeta)
		Expect(code).To(Equal(http.StatusOK), "%v", out)
	})

	It("refuses fields that exist only behind a beta, and an unknown one", func(ctx SpecContext) {
		for field, want := range map[string]string{
			"context_management": "context_management: Extra inputs are not permitted",
			"speed":              "speed: Extra inputs are not permitted",
			"brand_new_field":    "brand_new_field: Extra inputs are not permitted",
		} {
			code, out := send(ctx, body(map[string]any{field: "x"}))
			Expect(code).To(Equal(http.StatusBadRequest), field)
			Expect(message(out)).To(Equal(want))
		}
		code, out := send(ctx, body(map[string]any{"speed": "fast"}), "anthropic-beta", "fast-mode-2026-02-01")
		Expect(code).To(Equal(http.StatusOK), "%v", out)

		code, out = send(ctx, body(map[string]any{"thinking": map[string]any{"type": "adaptive", "effort": "high"}}))
		Expect(code).To(Equal(http.StatusBadRequest))
		Expect(message(out)).To(Equal("thinking.adaptive.effort: Extra inputs are not permitted"))
		code, out = send(ctx, body(map[string]any{"thinking": map[string]any{"type": "sometimes"}}))
		Expect(code).To(Equal(http.StatusBadRequest))
		Expect(message(out)).To(ContainSubstring("thinking.type"))
	})

	It("takes display summarized or omitted, and refuses display on disabled thinking", func(ctx SpecContext) {
		for _, d := range []string{"summarized", "omitted"} {
			code, out := send(ctx, body(map[string]any{"thinking": map[string]any{"type": "adaptive", "display": d}}))
			Expect(code).To(Equal(http.StatusOK), "%s: %v", d, out)
		}
		code, out := send(ctx, body(map[string]any{"thinking": map[string]any{"type": "adaptive", "display": "raw"}}))
		Expect(code).To(Equal(http.StatusBadRequest))
		Expect(message(out)).To(ContainSubstring("thinking.adaptive.display"))
		code, out = send(ctx, body(map[string]any{"thinking": map[string]any{"type": "disabled", "display": "summarized"}}))
		Expect(code).To(Equal(http.StatusBadRequest))
		Expect(message(out)).To(Equal("thinking.disabled.display: Extra inputs are not permitted"))
	})

	// old issue 316: of the Claude 5.x families only Haiku can stop thinking.
	DescribeTable("refuses the thinking types a Claude 5.x family does not take",
		func(ctx SpecContext, model string, accepted, refused []string) {
			Expect(srv.Register(fakemodel.Script{Model: model, Turns: []fakemodel.Turn{{Text: "ok"}}})).To(Succeed())
			for _, typ := range accepted {
				b := body(map[string]any{"model": model, "thinking": map[string]any{"type": typ}})
				if typ == "enabled" {
					b["thinking"].(map[string]any)["budget_tokens"] = 2048
				}
				code, out := send(ctx, b)
				Expect(code).To(Equal(http.StatusOK), "%s %s: %v", model, typ, out)
			}
			for _, typ := range refused {
				code, out := send(ctx, body(map[string]any{"model": model, "thinking": map[string]any{"type": typ}}))
				Expect(code).To(Equal(http.StatusBadRequest), "%s %s", model, typ)
				Expect(message(out)).To(Equal("thinking.type: '" + typ + "' is not supported for this model"))
			}
		},
		Entry("Haiku 5.5 takes all three", "claude-haiku-5-5", []string{"adaptive", "enabled", "disabled"}, nil),
		Entry("Haiku 5.5 on OpenRouter's Messages route", "anthropic/claude-haiku-5.5", []string{"adaptive", "disabled"}, nil),
		Entry("Sonnet 5.5 cannot stop", "claude-sonnet-5-5", []string{"adaptive", "enabled"}, []string{"disabled"}),
		Entry("Opus 5.5 thinks adaptively only", "anthropic/claude-opus-5.5", []string{"adaptive"}, []string{"disabled", "enabled"}),
		Entry("Fable 5.1 thinks adaptively only", "claude-fable-5-1", []string{"adaptive"}, []string{"disabled", "enabled"}),
		Entry("a model of no listed family takes every type", "claude-test", []string{"adaptive", "disabled"}, nil),
	)

	Describe("chat completions", func() {
		chat := func(ctx SpecContext, model string, reasoning map[string]any) (int, map[string]any) {
			GinkgoHelper()
			Expect(srv.Register(fakemodel.Script{Model: model, Turns: []fakemodel.Turn{{Text: "ok"}}})).To(Succeed())
			b := map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			if reasoning != nil {
				b["reasoning"] = reasoning
			}
			resp := post(ctx, url+"/v1/chat/completions", b)
			var out map[string]any
			decode(resp, &out)
			return resp.StatusCode, out
		}
		errMessage := func(out map[string]any) string {
			GinkgoHelper()
			return out["error"].(map[string]any)["message"].(string)
		}

		It("takes reasoning effort none for a model that can stop, and refuses it where reasoning is mandatory", func(ctx SpecContext) {
			for _, m := range []string{"openai/gpt-6-luna", "deepseek/deepseek-v4.1-flash"} {
				code, out := chat(ctx, m, map[string]any{"effort": "none"})
				Expect(code).To(Equal(http.StatusOK), "%s: %v", m, out)
			}
			code, out := chat(ctx, "openai/gpt-6-astra", map[string]any{"effort": "none"})
			Expect(code).To(Equal(http.StatusBadRequest))
			Expect(errMessage(out)).To(ContainSubstring("mandatory"))
			code, _ = chat(ctx, "openai/gpt-6-astra", map[string]any{"enabled": false})
			Expect(code).To(Equal(http.StatusBadRequest))
			code, out = chat(ctx, "openai/gpt-6-astra", nil)
			Expect(code).To(Equal(http.StatusOK), "%v", out)
		})

		It("refuses an unknown reasoning field or effort", func(ctx SpecContext) {
			code, out := chat(ctx, "openai/gpt-6-luna", map[string]any{"effort": "extreme"})
			Expect(code).To(Equal(http.StatusBadRequest))
			Expect(errMessage(out)).To(ContainSubstring("reasoning.effort"))
			code, out = chat(ctx, "openai/gpt-6-luna", map[string]any{"budget": 1})
			Expect(code).To(Equal(http.StatusBadRequest))
			Expect(errMessage(out)).To(ContainSubstring("reasoning.budget"))
		})
	})

	It("requires anthropic-version", func(ctx SpecContext) {
		resp := post(ctx, url+"/v1/messages", body(nil))
		var out map[string]any
		decode(resp, &out)
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		Expect(message(out)).To(Equal("anthropic-version: header is required"))
	})

	It("records the refused request for the test to inspect, and leaves chat completions alone", func(ctx SpecContext) {
		_, _ = send(ctx, body(map[string]any{"brand_new_field": 1}))
		reqs := srv.Requests()
		Expect(reqs).To(HaveLen(1))
		Expect(string(reqs[0].Body)).To(ContainSubstring("brand_new_field"))
		resp := post(ctx, url+"/v1/chat/completions", map[string]any{
			"model": "claude-haiku-5-5", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "extra": 1,
		})
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		_ = resp.Body.Close()
	})

	It("is off by default", func(ctx SpecContext) {
		_, ts := start(fakemodel.Script{Model: "claude-haiku-5-5", Turns: []fakemodel.Turn{{Text: "ok"}}})
		resp := post(ctx, ts.URL+"/v1/messages", body(map[string]any{"brand_new_field": 1}))
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		_ = resp.Body.Close()
	})
})
