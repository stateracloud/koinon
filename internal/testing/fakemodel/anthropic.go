// SPDX-License-Identifier: Apache-2.0

package fakemodel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type anthropicRequest struct {
	Model    string             `json:"model"`
	Messages []anthropicMessage `json:"messages"`
	Stream   bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
}

// blocks returns the message content as blocks; a plain string becomes one text block.
func (m anthropicMessage) blocks() []anthropicBlock {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []anthropicBlock{{Type: "text", Text: s}}
	}
	var bs []anthropicBlock
	_ = json.Unmarshal(m.Content, &bs)
	return bs
}

func (req anthropicRequest) conversation() conversation {
	var c conversation
	names := map[string]string{}
	for i, m := range req.Messages {
		blocks := m.blocks()
		switch m.Role {
		case "assistant":
			c.assistantTurns++
			for _, b := range blocks {
				if b.Type == "tool_use" {
					names[b.ID] = b.Name
				}
			}
		case "user":
			var text strings.Builder
			var results []string
			for _, b := range blocks {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "tool_result":
					results = append(results, names[b.ToolUseID])
				}
			}
			if text.Len() > 0 {
				c.lastUserText = text.String()
			}
			if i == len(req.Messages)-1 {
				c.toolResults = results
			}
		}
	}
	return c
}

func writeAnthropicError(w http.ResponseWriter, err error) {
	e, ok := err.(*apiError) //nolint:errorlint // apiError is never wrapped
	if !ok {
		e = &apiError{kind: errBadRequest, message: err.Error()}
	}
	typ := "invalid_request_error"
	switch e.kind {
	case errNotFound:
		typ = "not_found_error"
	case errUnavailable:
		typ = "overloaded_error"
	}
	writeJSON(w, e.status(), map[string]any{
		"type": "error", "error": map[string]any{"type": typ, "message": e.message},
	})
}

func (r reply) stopReason() string {
	if len(r.turn.ToolCalls) > 0 {
		return "tool_use"
	}
	return "end_turn"
}

func (r reply) messageID() string { return fmt.Sprintf("msg_fake_%d", r.seq) }

// anthropicUsage reports uncached input as input_tokens and cache reads separately, as the
// Messages API does.
func anthropicUsage(u Usage, outputTokens int) map[string]any {
	return map[string]any{
		"input_tokens":                u.InputTokens - u.CachedInputTokens,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     u.CachedInputTokens,
		"output_tokens":               outputTokens,
	}
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	var req anthropicRequest
	body, err := s.readBody(w, r, &req)
	if err != nil {
		writeAnthropicError(w, err)
		return
	}
	rep, err := s.plan(r.URL.Path, req.Model, body, req.conversation())
	if err != nil {
		writeAnthropicError(w, err)
		return
	}
	if !wait(r, rep.faults.Delay) {
		return
	}
	if req.Stream {
		writeSSE(w, rep.anthropicEvents(), rep.faults.Truncate)
		return
	}
	writeBody(w, rep.anthropicMessage(), rep.faults.Truncate)
}

func (r reply) anthropicContent() []map[string]any {
	var content []map[string]any
	if r.turn.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": r.turn.Text})
	}
	for i, c := range r.turn.ToolCalls {
		args := c.Arguments
		if args == nil {
			args = map[string]any{}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": r.toolCallID("toolu", i), "name": c.Name, "input": args,
		})
	}
	return content
}

func (r reply) anthropicMessage() map[string]any {
	return map[string]any{
		"id": r.messageID(), "type": "message", "role": "assistant", "model": r.model,
		"content": r.anthropicContent(), "stop_reason": r.stopReason(), "stop_sequence": nil,
		"usage": anthropicUsage(r.turn.Usage, r.turn.Usage.OutputTokens),
	}
}

// anthropicEvents renders the turn as the documented event flow: message_start, a ping, one
// content block per text or tool call (start, deltas, stop), message_delta with cumulative usage,
// then message_stop.
func (r reply) anthropicEvents() []sseEvent {
	var events []sseEvent
	add := func(name string, data map[string]any) {
		data["type"] = name
		events = append(events, sseEvent{name: name, data: mustJSON(data)})
	}

	add("message_start", map[string]any{"message": map[string]any{
		"id": r.messageID(), "type": "message", "role": "assistant", "model": r.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": anthropicUsage(r.turn.Usage, 1),
	}})
	add("ping", map[string]any{})

	idx := 0
	if r.turn.Text != "" {
		add("content_block_start", map[string]any{"index": idx, "content_block": map[string]any{"type": "text", "text": ""}})
		for _, piece := range chunkText(r.turn.Text) {
			if piece != "" {
				add("content_block_delta", map[string]any{"index": idx, "delta": map[string]any{"type": "text_delta", "text": piece}})
			}
		}
		add("content_block_stop", map[string]any{"index": idx})
		idx++
	}
	for i, c := range r.turn.ToolCalls {
		add("content_block_start", map[string]any{"index": idx, "content_block": map[string]any{
			"type": "tool_use", "id": r.toolCallID("toolu", i), "name": c.Name, "input": map[string]any{},
		}})
		for _, frag := range splitHalf(c.argumentsJSON()) {
			add("content_block_delta", map[string]any{"index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": frag}})
		}
		add("content_block_stop", map[string]any{"index": idx})
		idx++
	}

	add("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": r.stopReason(), "stop_sequence": nil},
		"usage": anthropicUsage(r.turn.Usage, r.turn.Usage.OutputTokens),
	})
	add("message_stop", map[string]any{})
	return events
}
