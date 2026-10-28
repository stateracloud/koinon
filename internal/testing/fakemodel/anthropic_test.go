// SPDX-License-Identifier: Apache-2.0

package fakemodel_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// Mirrors of the Messages API objects
// (https://platform.claude.com/docs/en/api/messages and .../build-with-claude/streaming),
// decoded strictly.

type anUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

type anBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anMessage struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Role         string    `json:"role"`
	Model        string    `json:"model"`
	Content      []anBlock `json:"content"`
	StopReason   *string   `json:"stop_reason"`
	StopSequence *string   `json:"stop_sequence"`
	Usage        anUsage   `json:"usage"`
}

type anEvent struct {
	Type         string     `json:"type"`
	Message      *anMessage `json:"message,omitempty"`
	Index        *int       `json:"index,omitempty"`
	ContentBlock *anBlock   `json:"content_block,omitempty"`
	Delta        *struct {
		Type         string  `json:"type,omitempty"`
		Text         string  `json:"text,omitempty"`
		PartialJSON  *string `json:"partial_json,omitempty"`
		StopReason   *string `json:"stop_reason,omitempty"`
		StopSequence *string `json:"stop_sequence,omitempty"`
	} `json:"delta,omitempty"`
	Usage *anUsage `json:"usage,omitempty"`
}

func anUser(text string) map[string]any { return map[string]any{"role": "user", "content": text} }

func anAfterToolUse() []map[string]any {
	return []map[string]any{
		anUser("What is the weather in Zürich?"),
		{"role": "assistant", "content": []map[string]any{
			{"type": "text", "text": "Let me check."},
			{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": map[string]any{"city": "Zürich"}},
		}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "toolu_1", "content": "sunny, 21 °C"},
		}},
	}
}

var _ = Describe("Anthropic messages", func() {
	var (
		ts  *httptest.Server
		url string
	)

	BeforeEach(func() {
		_, ts = start(weather)
		url = ts.URL + "/v1/messages"
	})

	send := func(ctx SpecContext, messages any, stream bool) *http.Response {
		return post(ctx, url, map[string]any{
			"model": "weather", "max_tokens": 1024, "messages": messages, "stream": stream,
		}, "anthropic-version", "2023-06-01", "x-api-key", "placeholder")
	}

	Context("non-streaming", func() {
		It("returns a tool_use block for a user message", func(ctx SpecContext) {
			resp := send(ctx, []any{anUser("weather?")}, false)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			var raw json.RawMessage
			decode(resp, &raw)
			var m anMessage
			Expect(strictUnmarshal(string(raw), &m)).To(Succeed())
			Expect(m.Type).To(Equal("message"))
			Expect(m.Role).To(Equal("assistant"))
			Expect(m.ID).To(HavePrefix("msg_"))
			Expect(m.StopReason).To(HaveValue(Equal("tool_use")))
			Expect(m.StopSequence).To(BeNil())
			Expect(m.Content).To(HaveLen(1))
			Expect(m.Content[0].Type).To(Equal("tool_use"))
			Expect(m.Content[0].ID).To(Equal("toolu_fake_0_0"))
			Expect(m.Content[0].Name).To(Equal("get_weather"))
			Expect(m.Content[0].Input).To(MatchJSON(`{"city":"Zürich"}`))
			Expect(m.Usage).To(Equal(anUsage{InputTokens: 100, OutputTokens: 20}))
		})

		It("answers with text after the tool result, reporting cache reads separately", func(ctx SpecContext) {
			var m anMessage
			decode(send(ctx, anAfterToolUse(), false), &m)
			Expect(m.StopReason).To(HaveValue(Equal("end_turn")))
			Expect(m.Content).To(Equal([]anBlock{{Type: "text", Text: "It is sunny in Zürich today."}}))
			Expect(m.Usage).To(Equal(anUsage{InputTokens: 50, CacheReadInputTokens: 100, OutputTokens: 8}))
		})
	})

	It("reports cache writes as cache_creation_input_tokens and records the request headers", func(ctx SpecContext) {
		srv, ts := start(fakemodel.Script{Model: "writer", Turns: []fakemodel.Turn{{
			Text: "ok", Usage: fakemodel.Usage{InputTokens: 100, CachedInputTokens: 30, CacheWriteTokens: 50, OutputTokens: 4},
		}}})
		resp := post(ctx, ts.URL+"/v1/messages", map[string]any{
			"model": "writer", "max_tokens": 8, "messages": []any{anUser("hi")},
		}, "anthropic-version", "2023-06-01", "anthropic-workspace-id", "wrkspc_01Example")
		var m anMessage
		decode(resp, &m)
		Expect(m.Usage).To(Equal(anUsage{InputTokens: 20, CacheCreationInputTokens: 50, CacheReadInputTokens: 30, OutputTokens: 4}))

		reqs := srv.Requests()
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].Header.Get("anthropic-version")).To(Equal("2023-06-01"))
		Expect(reqs[0].Header.Get("anthropic-workspace-id")).To(Equal("wrkspc_01Example"))
	})

	Context("streaming", func() {
		stream := func(ctx SpecContext, messages any) []anEvent {
			resp := send(ctx, messages, true)
			DeferCleanup(resp.Body.Close)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(resp.Header.Get("Content-Type")).To(Equal("text/event-stream"))
			events, err := readSSE(resp.Body)
			Expect(err).NotTo(HaveOccurred())
			var out []anEvent
			for _, e := range events {
				var ev anEvent
				Expect(strictUnmarshal(e.data, &ev)).To(Succeed(), e.data)
				Expect(ev.Type).To(Equal(e.name), "the SSE event name matches the data type")
				out = append(out, ev)
			}
			return out
		}

		names := func(events []anEvent) []string {
			var n []string
			for _, e := range events {
				n = append(n, e.Type)
			}
			return n
		}

		It("follows the documented event flow for text", func(ctx SpecContext) {
			events := stream(ctx, anAfterToolUse())
			n := names(events)
			Expect(n[:3]).To(Equal([]string{"message_start", "ping", "content_block_start"}))
			Expect(n[len(n)-3:]).To(Equal([]string{"content_block_stop", "message_delta", "message_stop"}))

			start := events[0].Message
			Expect(start.Content).To(BeEmpty())
			Expect(start.StopReason).To(BeNil())
			Expect(start.Usage.InputTokens).To(Equal(50))
			Expect(start.Usage.CacheReadInputTokens).To(Equal(100))
			Expect(events[2].ContentBlock).To(Equal(&anBlock{Type: "text"}))

			var text strings.Builder
			deltas := 0
			for _, e := range events {
				if e.Type == "content_block_delta" {
					Expect(e.Index).To(HaveValue(Equal(0)))
					Expect(e.Delta.Type).To(Equal("text_delta"))
					text.WriteString(e.Delta.Text)
					deltas++
				}
			}
			Expect(deltas).To(BeNumerically(">", 1))
			Expect(text.String()).To(Equal("It is sunny in Zürich today."))

			md := events[len(events)-2]
			Expect(md.Delta.StopReason).To(HaveValue(Equal("end_turn")))
			Expect(md.Usage.OutputTokens).To(Equal(8), "message_delta usage is cumulative")
		})

		It("streams tool_use blocks as input_json_delta fragments", func(ctx SpecContext) {
			events := stream(ctx, []any{anUser("weather?")})
			Expect(names(events)).To(Equal([]string{
				"message_start", "ping", "content_block_start",
				"content_block_delta", "content_block_delta", "content_block_stop",
				"message_delta", "message_stop",
			}))
			cb := events[2].ContentBlock
			Expect(cb.Type).To(Equal("tool_use"))
			Expect(cb.ID).To(Equal("toolu_fake_0_0"))
			Expect(cb.Name).To(Equal("get_weather"))
			Expect(cb.Input).To(MatchJSON(`{}`))
			var partial strings.Builder
			for _, e := range events[3:5] {
				Expect(e.Delta.Type).To(Equal("input_json_delta"))
				partial.WriteString(*e.Delta.PartialJSON)
			}
			Expect(partial.String()).To(MatchJSON(`{"city":"Zürich"}`))
			Expect(events[6].Delta.StopReason).To(HaveValue(Equal("tool_use")))
			Expect(events[6].Usage.OutputTokens).To(Equal(20))
		})
	})

	It("returns 404 with an Anthropic error body for an unknown model", func(ctx SpecContext) {
		resp := post(ctx, url, map[string]any{"model": "claude-nope", "max_tokens": 1, "messages": []any{anUser("hi")}})
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		decode(resp, &body)
		Expect(body.Type).To(Equal("error"))
		Expect(body.Error.Type).To(Equal("not_found_error"))
		Expect(body.Error.Message).To(ContainSubstring("claude-nope"))
	})

	It("lists models in Anthropic's shape when asked with anthropic-version", func(ctx SpecContext) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/models", nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("anthropic-version", "2023-06-01")
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		var list struct {
			Data []struct {
				Type        string `json:"type"`
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
				CreatedAt   string `json:"created_at"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			FirstID string `json:"first_id"`
			LastID  string `json:"last_id"`
		}
		decode(resp, &list)
		Expect(list.Data).To(HaveLen(1))
		Expect(list.Data[0].Type).To(Equal("model"))
		Expect(list.Data[0].ID).To(Equal("weather"))
		Expect(list.FirstID).To(Equal("weather"))
		Expect(list.HasMore).To(BeFalse())
	})
})
