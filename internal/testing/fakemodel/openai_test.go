// SPDX-License-Identifier: Apache-2.0

package fakemodel_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/testing/fakemodel"
)

// The types below mirror the chat completion and chunk objects of the OpenAI API reference
// (https://platform.openai.com/docs/api-reference/chat/object and .../chat-streaming). They are
// decoded with DisallowUnknownFields, so the server emits only documented fields.

type oaUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type oaToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaMessage struct {
	Role      string       `json:"role"`
	Content   *string      `json:"content"`
	Refusal   *string      `json:"refusal"`
	ToolCalls []oaToolCall `json:"tool_calls"`
}

type oaCompletion struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int       `json:"index"`
		Message      oaMessage `json:"message"`
		Logprobs     any       `json:"logprobs"`
		FinishReason string    `json:"finish_reason"`
	} `json:"choices"`
	Usage oaUsage `json:"usage"`
}

type oaChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string       `json:"role"`
			Content   *string      `json:"content"`
			ToolCalls []oaToolCall `json:"tool_calls"`
		} `json:"delta"`
		Logprobs     any     `json:"logprobs"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
}

func strictUnmarshal(data string, v any) error {
	dec := json.NewDecoder(strings.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

var weather = fakemodel.Script{
	Model: "weather",
	Turns: []fakemodel.Turn{
		{
			ToolCalls: []fakemodel.ToolCall{{Name: "get_weather", Arguments: map[string]any{"city": "Zürich"}}},
			Usage:     fakemodel.Usage{InputTokens: 100, OutputTokens: 20},
		},
		{
			When:  fakemodel.When{ToolResult: "get_weather"},
			Text:  "It is sunny in Zürich today.",
			Usage: fakemodel.Usage{InputTokens: 150, CachedInputTokens: 100, OutputTokens: 8},
		},
	},
}

func userMsg(text string) map[string]any { return map[string]any{"role": "user", "content": text} }

// afterToolCall is a conversation where the model called get_weather and got its result.
func afterToolCall() []map[string]any {
	return []map[string]any{
		userMsg("What is the weather in Zürich?"),
		{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "get_weather", "arguments": `{"city":"Zürich"}`},
		}}},
		{"role": "tool", "tool_call_id": "call_1", "content": "sunny, 21 °C"},
	}
}

var _ = Describe("OpenAI chat completions", func() {
	var (
		srv *fakemodel.Server
		ts  *httptest.Server
		url string
	)

	BeforeEach(func() {
		srv, ts = start(weather)
		url = ts.URL + "/v1/chat/completions"
	})

	Context("non-streaming", func() {
		It("returns the tool-call turn for a user message", func(ctx SpecContext) {
			resp := post(ctx, url, map[string]any{"model": "weather", "messages": []any{userMsg("weather?")}})
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(resp.Header.Get("Content-Type")).To(Equal("application/json"))
			var buf bytes.Buffer
			_, err := buf.ReadFrom(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Body.Close()).To(Succeed())

			var c oaCompletion
			Expect(strictUnmarshal(buf.String(), &c)).To(Succeed())
			Expect(c.Object).To(Equal("chat.completion"))
			Expect(c.Model).To(Equal("weather"))
			Expect(c.ID).To(HavePrefix("chatcmpl-"))
			Expect(c.Choices).To(HaveLen(1))
			ch := c.Choices[0]
			Expect(ch.FinishReason).To(Equal("tool_calls"))
			Expect(ch.Message.Role).To(Equal("assistant"))
			Expect(ch.Message.Content).To(BeNil())
			Expect(ch.Message.ToolCalls).To(HaveLen(1))
			tc := ch.Message.ToolCalls[0]
			Expect(tc.ID).To(Equal("call_fake_0_0"))
			Expect(tc.Type).To(Equal("function"))
			Expect(tc.Function.Name).To(Equal("get_weather"))
			Expect(tc.Function.Arguments).To(MatchJSON(`{"city":"Zürich"}`))
			Expect(c.Usage).To(Equal(oaUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}))
		})

		It("answers with text once the tool result is in the conversation", func(ctx SpecContext) {
			var c oaCompletion
			decode(post(ctx, url, map[string]any{"model": "weather", "messages": afterToolCall()}), &c)
			Expect(c.Choices[0].FinishReason).To(Equal("stop"))
			Expect(*c.Choices[0].Message.Content).To(Equal("It is sunny in Zürich today."))
			Expect(c.Choices[0].Message.ToolCalls).To(BeEmpty())
			Expect(c.Usage.PromptTokens).To(Equal(150))
			Expect(c.Usage.PromptTokensDetails.CachedTokens).To(Equal(100))
			Expect(c.Usage.TotalTokens).To(Equal(158))
		})

		It("records the requests it served", func(ctx SpecContext) {
			Expect(post(ctx, url, map[string]any{"model": "weather", "messages": []any{userMsg("hi")}}).Body.Close()).To(Succeed())
			reqs := srv.Requests()
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].Path).To(Equal("/v1/chat/completions"))
			Expect(reqs[0].Model).To(Equal("weather"))
			Expect(string(reqs[0].Body)).To(ContainSubstring(`"hi"`))
		})
	})

	Context("streaming", func() {
		stream := func(ctx SpecContext, messages any, includeUsage bool) []event {
			body := map[string]any{"model": "weather", "messages": messages, "stream": true}
			if includeUsage {
				body["stream_options"] = map[string]any{"include_usage": true}
			}
			resp := post(ctx, url, body)
			DeferCleanup(resp.Body.Close)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(resp.Header.Get("Content-Type")).To(Equal("text/event-stream"))
			events, err := readSSE(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			Expect(events).NotTo(BeEmpty())
			Expect(events[len(events)-1].data).To(Equal("[DONE]"))
			return events
		}

		chunks := func(events []event) []oaChunk {
			var out []oaChunk
			for _, e := range events[:len(events)-1] {
				Expect(e.name).To(BeEmpty(), "chat completion chunks are data-only events")
				var c oaChunk
				Expect(strictUnmarshal(e.data, &c)).To(Succeed(), e.data)
				Expect(c.Object).To(Equal("chat.completion.chunk"))
				out = append(out, c)
			}
			Expect(out).NotTo(BeEmpty())
			for _, c := range out {
				Expect(c.ID).To(Equal(out[0].ID), "every chunk has the same id")
				Expect(c.Created).To(Equal(out[0].Created))
			}
			return out
		}

		It("streams text deltas and a final usage chunk with include_usage", func(ctx SpecContext) {
			events := stream(ctx, afterToolCall(), true)
			for _, e := range events[:len(events)-2] {
				Expect(e.data).To(ContainSubstring(`"usage":null`), "usage is null except on the last chunk")
			}
			cs := chunks(events)
			last := cs[len(cs)-1]
			Expect(last.Choices).To(BeEmpty())
			Expect(last.Usage).NotTo(BeNil())
			Expect(last.Usage.PromptTokens).To(Equal(150))
			Expect(last.Usage.CompletionTokens).To(Equal(8))

			var text strings.Builder
			var finish []string
			for _, c := range cs[:len(cs)-1] {
				Expect(c.Choices).To(HaveLen(1))
				Expect(c.Usage).To(BeNil())
				if p := c.Choices[0].Delta.Content; p != nil {
					text.WriteString(*p)
				}
				if f := c.Choices[0].FinishReason; f != nil {
					finish = append(finish, *f)
				}
			}
			Expect(cs[0].Choices[0].Delta.Role).To(Equal("assistant"))
			Expect(len(cs)).To(BeNumerically(">", 4), "text arrives in several deltas")
			Expect(text.String()).To(Equal("It is sunny in Zürich today."))
			Expect(finish).To(Equal([]string{"stop"}))
		})

		It("omits usage without include_usage", func(ctx SpecContext) {
			events := stream(ctx, afterToolCall(), false)
			for _, e := range events {
				Expect(e.data).NotTo(ContainSubstring(`"usage"`))
			}
			cs := chunks(events)
			Expect(cs[len(cs)-1].Choices[0].FinishReason).To(HaveValue(Equal("stop")))
		})

		It("streams tool calls with arguments split across deltas", func(ctx SpecContext) {
			cs := chunks(stream(ctx, []any{userMsg("weather?")}, true))
			var args strings.Builder
			var id, name string
			fragments := 0
			for _, c := range cs[:len(cs)-1] {
				for _, tc := range c.Choices[0].Delta.ToolCalls {
					Expect(tc.Index).To(HaveValue(Equal(0)))
					if tc.ID != "" {
						id, name = tc.ID, tc.Function.Name
						Expect(tc.Type).To(Equal("function"))
					}
					if tc.Function.Arguments != "" {
						fragments++
					}
					args.WriteString(tc.Function.Arguments)
				}
			}
			Expect(id).To(Equal("call_fake_0_0"))
			Expect(name).To(Equal("get_weather"))
			Expect(fragments).To(BeNumerically(">=", 2))
			Expect(args.String()).To(MatchJSON(`{"city":"Zürich"}`))
			Expect(cs[len(cs)-2].Choices[0].FinishReason).To(HaveValue(Equal("tool_calls")))
			Expect(cs[len(cs)-1].Usage.TotalTokens).To(Equal(120))
		})
	})

	It("returns 404 with an OpenAI error body for an unknown model", func(ctx SpecContext) {
		resp := post(ctx, url, map[string]any{"model": "gpt-nope", "messages": []any{userMsg("hi")}})
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		var body struct {
			Error struct {
				Message string  `json:"message"`
				Type    string  `json:"type"`
				Param   *string `json:"param"`
				Code    string  `json:"code"`
			} `json:"error"`
		}
		decode(resp, &body)
		Expect(body.Error.Type).To(Equal("invalid_request_error"))
		Expect(body.Error.Code).To(Equal("model_not_found"))
		Expect(body.Error.Message).To(ContainSubstring("gpt-nope"))
	})

	It("returns 400 when no turn matches", func(ctx SpecContext) {
		Expect(srv.Register(fakemodel.Script{Model: "strict", Turns: []fakemodel.Turn{
			{When: fakemodel.When{UserContains: "magic"}, Text: "ok"},
		}})).To(Succeed())
		resp := post(ctx, url, map[string]any{"model": "strict", "messages": []any{userMsg("hello")}})
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		Expect(resp.Body.Close()).To(Succeed())
	})

	It("rejects malformed JSON", func(ctx SpecContext) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{"))
		Expect(err).NotTo(HaveOccurred())
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		Expect(resp.Body.Close()).To(Succeed())
	})

	It("lists models", func(ctx SpecContext) {
		Expect(srv.Register(fakemodel.Script{Model: "another", Turns: []fakemodel.Turn{{Text: "x"}}})).To(Succeed())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/models", nil)
		Expect(err).NotTo(HaveOccurred())
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		var list struct {
			Object string `json:"object"`
			Data   []struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				Created int64  `json:"created"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		decode(resp, &list)
		Expect(list.Object).To(Equal("list"))
		Expect(list.Data).To(HaveLen(2))
		Expect(list.Data[0].ID).To(Equal("another"))
		Expect(list.Data[1].ID).To(Equal("weather"))
		Expect(list.Data[1].Object).To(Equal("model"))
	})
})
