// SPDX-License-Identifier: Apache-2.0

package fakemodel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type openAIRequest struct {
	Model         string          `json:"model"`
	Messages      []openAIMessage `json:"messages"`
	Stream        bool            `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type openAIMessage struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
	// Name is set on legacy role "function" messages.
	Name string `json:"name"`
}

// contentText returns the message content as plain text: a string, or the text parts of an array.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func (req openAIRequest) conversation() conversation {
	var c conversation
	names := map[string]string{}
	for _, m := range req.Messages {
		switch m.Role {
		case "assistant":
			c.assistantTurns++
			for _, tc := range m.ToolCalls {
				names[tc.ID] = tc.Function.Name
			}
		case "user":
			if t := contentText(m.Content); t != "" {
				c.lastUserText = t
			}
		}
	}
	// Trailing tool (or legacy function) messages are the results the model is answering.
trailing:
	for i := len(req.Messages) - 1; i >= 0; i-- {
		switch m := req.Messages[i]; m.Role {
		case "tool":
			c.toolResults = append(c.toolResults, names[m.ToolCallID])
		case "function":
			c.toolResults = append(c.toolResults, m.Name)
		default:
			break trailing
		}
	}
	return c
}

func writeOpenAIError(w http.ResponseWriter, err error) {
	e, ok := err.(*apiError) //nolint:errorlint // apiError is never wrapped
	if !ok {
		e = &apiError{kind: errBadRequest, message: err.Error()}
	}
	typ, code := "invalid_request_error", any(nil)
	switch e.kind {
	case errNotFound:
		code = "model_not_found"
	case errUnavailable:
		typ = "server_error"
	}
	writeJSON(w, e.status(), map[string]any{"error": map[string]any{
		"message": e.message, "type": typ, "param": nil, "code": code,
	}})
}

func openAIUsage(u Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":         u.InputTokens,
		"completion_tokens":     u.OutputTokens,
		"total_tokens":          u.InputTokens + u.OutputTokens,
		"prompt_tokens_details": map[string]any{"cached_tokens": u.CachedInputTokens},
	}
}

func (r reply) finishReason() string {
	if len(r.turn.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func (r reply) completionID() string { return fmt.Sprintf("chatcmpl-fake-%d", r.seq) }

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req openAIRequest
	body, err := s.readBody(w, r, &req)
	if err != nil {
		writeOpenAIError(w, err)
		return
	}
	rep, err := s.plan(r.URL.Path, req.Model, body, req.conversation())
	if err != nil {
		writeOpenAIError(w, err)
		return
	}
	if !wait(r, rep.faults.Delay) {
		return
	}
	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		writeSSE(w, rep.openAIChunks(includeUsage), rep.faults.Truncate)
		return
	}
	writeBody(w, rep.openAICompletion(), rep.faults.Truncate)
}

func (r reply) openAICompletion() map[string]any {
	msg := map[string]any{"role": "assistant", "content": nil, "refusal": nil}
	if r.turn.Text != "" {
		msg["content"] = r.turn.Text
	}
	if len(r.turn.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(r.turn.ToolCalls))
		for i, c := range r.turn.ToolCalls {
			calls = append(calls, map[string]any{
				"id": r.toolCallID("call", i), "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": c.argumentsJSON()},
			})
		}
		msg["tool_calls"] = calls
	}
	return map[string]any{
		"id": r.completionID(), "object": "chat.completion", "created": r.created, "model": r.model,
		"choices": []map[string]any{{
			"index": 0, "message": msg, "logprobs": nil, "finish_reason": r.finishReason(),
		}},
		"usage": openAIUsage(r.turn.Usage),
	}
}

// openAIChunks renders the turn as chat.completion.chunk events followed by [DONE]. With
// includeUsage every chunk carries "usage": null and a last chunk with empty choices carries
// the usage, as the API reference describes.
func (r reply) openAIChunks(includeUsage bool) []sseEvent {
	var events []sseEvent
	chunk := func(delta map[string]any, finish any) {
		c := map[string]any{
			"id": r.completionID(), "object": "chat.completion.chunk", "created": r.created, "model": r.model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "logprobs": nil, "finish_reason": finish}},
		}
		if includeUsage {
			c["usage"] = nil
		}
		events = append(events, sseEvent{data: mustJSON(c)})
	}

	chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	for _, piece := range chunkText(r.turn.Text) {
		if piece != "" {
			chunk(map[string]any{"content": piece}, nil)
		}
	}
	for i, c := range r.turn.ToolCalls {
		chunk(map[string]any{"tool_calls": []map[string]any{{
			"index": i, "id": r.toolCallID("call", i), "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": ""},
		}}}, nil)
		for _, frag := range splitHalf(c.argumentsJSON()) {
			chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": i, "function": map[string]any{"arguments": frag},
			}}}, nil)
		}
	}
	chunk(map[string]any{}, r.finishReason())
	if includeUsage {
		events = append(events, sseEvent{data: mustJSON(map[string]any{
			"id": r.completionID(), "object": "chat.completion.chunk", "created": r.created, "model": r.model,
			"choices": []any{}, "usage": openAIUsage(r.turn.Usage),
		})})
	}
	return append(events, sseEvent{data: "[DONE]"})
}
