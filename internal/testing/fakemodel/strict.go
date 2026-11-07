// SPDX-License-Identifier: Apache-2.0

package fakemodel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Strict Messages mode (rc.8, ADR 0069 amendment 2026-10-29).
//
// The default fake accepts any Messages body, which is why rc.7 shipped a request direct Anthropic
// refuses: OpenCode added thinking.block_binding, a beta-only field, the gateway had removed the
// beta, and nothing in the specs checked bodies the way Anthropic does. Strict mode checks what
// Anthropic checks and answers with Anthropic's own 400:
//
//   - a top-level field the Messages API does not have is refused ("x: Extra inputs are not
//     permitted"),
//   - so is a field of the thinking object it does not have,
//   - a field that exists only behind a beta is refused unless the anthropic-beta header names that
//     beta,
//   - a thinking type the model does not take is refused: of the Claude 5.x families only Haiku
//     takes "disabled", and Opus and Fable take only "adaptive" (old issue 316, see claudeThinkingTypes), and
//   - anthropic-version must be present.
//
// Strict mode also checks the reasoning object of a chat-completions request the way OpenRouter
// does: its fields, the effort values, and that a model whose reasoning is mandatory refuses to
// turn it off (checkStrictReasoning).
//
// Only the top level and thinking are checked field by field: messages, system and tools carry
// arbitrary content, and the specs that need them strict assert on them directly. The tables below
// are the fields the Messages reference lists
// (https://platform.claude.com/docs/en/api/messages); a new field a Koinon harness sends is one
// entry here, which is the point: the day it sends one the gateway cannot vouch for, a spec fails.

// messagesFields are the top-level fields of a Messages request.
var messagesFields = []string{
	"model", "max_tokens", "messages", "system", "stream", "tools", "tool_choice", "temperature",
	"top_p", "top_k", "stop_sequences", "metadata", "service_tier", "thinking", "output_config",
	"container", "mcp_servers", "cache_control",
	// Beta-only; see betaFields.
	"context_management", "speed",
}

// thinkingFields are the fields of the thinking object per type.
var thinkingFields = map[string][]string{
	"enabled":  {"type", "budget_tokens", "display"},
	"adaptive": {"type", "display", "block_binding"},
	"disabled": {"type"},
}

var thinkingTypes = []string{"enabled", "disabled", "adaptive"}

// thinkingDisplays are the values of thinking.display.
var thinkingDisplays = []string{"summarized", "omitted"}

// claudeThinkingTypes are the thinking types each Claude 5.x family takes, by the start of the
// normalized model name (see claudeName); a model of no listed family takes every type. Sonnet 5.5
// refuses "disabled"; Opus 5.5 and Fable 5.1 think always and refuse both "enabled" and
// "disabled" (deep dive G4 of old issue 316, 2026-10-31). The fake keeps its own list rather than reading
// internal/thinking, so a wrong row there fails a spec here.
var claudeThinkingTypes = []struct {
	prefix string
	types  []string
}{
	{"claude-haiku-5-", []string{"enabled", "disabled", "adaptive"}},
	{"claude-sonnet-5-", []string{"enabled", "adaptive"}},
	{"claude-opus-5-", []string{"adaptive"}},
	{"claude-fable-5-", []string{"adaptive"}},
}

// claudeName lowercases a model name, drops OpenRouter's anthropic/ prefix and writes the version
// with dashes.
func claudeName(model string) string {
	n := strings.TrimPrefix(strings.ToLower(model), "anthropic/")
	return strings.ReplaceAll(n, ".", "-")
}

// takesThinkingType reports whether the model accepts the thinking type.
func takesThinkingType(model, typ string) bool {
	name := claudeName(model)
	for _, f := range claudeThinkingTypes {
		if strings.HasPrefix(name, f.prefix) {
			return slices.Contains(f.types, typ)
		}
	}
	return true
}

// betaField is a request field that exists only behind a beta. Prefix is matched against the
// values of the anthropic-beta header, so a dated beta needs no new entry per date unless the field
// is tied to one date.
type betaField struct {
	// Path is the field as Anthropic's error names it.
	Path string
	// Beta is the beta value required, or its prefix when Prefix is set.
	Beta   string
	Prefix bool
}

var betaFields = []betaField{
	{Path: "thinking.block_binding", Beta: "thinking-binding-controls-2026-08-01"},
	{Path: "context_management", Beta: "compact-", Prefix: true},
	{Path: "speed", Beta: "fast-mode-", Prefix: true},
}

// SetStrictMessages turns strict mode on or off: the Messages checks for requests to /v1/messages
// and the reasoning checks for chat completions.
func (s *Server) SetStrictMessages(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strictMessages = on
}

func (s *Server) strict() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.strictMessages
}

// record keeps a request that is refused before a script is selected, so a test can see what was
// sent.
func (s *Server) record(path string, header http.Header, model string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Request{Path: path, Model: model, Body: body, Header: header.Clone()})
}

// betas returns every value of the anthropic-beta header, across repeated lines and commas.
func betas(h http.Header) []string {
	var out []string
	for _, line := range h.Values("Anthropic-Beta") {
		for _, v := range strings.Split(line, ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

func hasBeta(have []string, f betaField) bool {
	return slices.ContainsFunc(have, func(b string) bool {
		if f.Prefix {
			return strings.HasPrefix(b, f.Beta)
		}
		return b == f.Beta
	})
}

// checkStrictMessages validates a Messages body and headers the way Anthropic does. It returns the
// message of the 400, in the order Anthropic reports: the first problem found.
func checkStrictMessages(body []byte, h http.Header) error {
	if h.Get("Anthropic-Version") == "" {
		return fmt.Errorf("anthropic-version: header is required")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	have := betas(h)
	// The first unknown field in sorted order is reported, so the message is the same on every run.
	for _, k := range sortedKeys(top) {
		if slices.Contains(messagesFields, k) {
			continue
		}
		return extraInput(k)
	}
	for _, f := range betaFields {
		if !strings.Contains(f.Path, ".") && top[f.Path] != nil && !hasBeta(have, f) {
			return extraInput(f.Path)
		}
	}
	raw, ok := top["thinking"]
	if !ok {
		return nil
	}
	var th map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&th); err != nil {
		return fmt.Errorf("thinking: Input should be a valid dictionary")
	}
	var typ string
	_ = json.Unmarshal(th["type"], &typ)
	if !slices.Contains(thinkingTypes, typ) {
		return fmt.Errorf("thinking.type: Input should be 'enabled', 'disabled' or 'adaptive'")
	}
	for _, k := range sortedKeys(th) {
		if !slices.Contains(thinkingFields[typ], k) {
			return extraInput("thinking." + typ + "." + k)
		}
	}
	if raw, ok := th["display"]; ok {
		var d string
		if json.Unmarshal(raw, &d) != nil || !slices.Contains(thinkingDisplays, d) {
			return fmt.Errorf("thinking.%s.display: Input should be 'summarized' or 'omitted'", typ)
		}
	}
	var model string
	_ = json.Unmarshal(top["model"], &model)
	if !takesThinkingType(model, typ) {
		return fmt.Errorf("thinking.type: '%s' is not supported for this model", typ)
	}
	for _, f := range betaFields {
		if k, ok := strings.CutPrefix(f.Path, "thinking."); ok && th[k] != nil && !hasBeta(have, f) {
			return extraInput("thinking." + typ + "." + k)
		}
	}
	return nil
}

// reasoningFields are the fields of OpenRouter's reasoning object, and reasoningEfforts its effort
// values (https://openrouter.ai/docs/use-cases/reasoning-tokens).
var (
	reasoningFields  = []string{"effort", "max_tokens", "exclude", "enabled"}
	reasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh"}
)

// mandatoryReasoning are the chat-completions models whose reasoning cannot be turned off
// (reasoning.mandatory in OpenRouter's models API, 2026-10-31), by the start of the lowercased name.
var mandatoryReasoning = []string{"openai/gpt-6-astra", "anthropic/claude-sonnet-5", "anthropic/claude-opus-5", "anthropic/claude-fable-5"}

// checkStrictReasoning validates the reasoning object of a chat-completions body. It returns the
// message of the 400.
func checkStrictReasoning(body []byte) error {
	var top struct {
		Model     string                     `json:"model"`
		Reasoning map[string]json.RawMessage `json:"reasoning"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if top.Reasoning == nil {
		return nil
	}
	for _, k := range sortedKeys(top.Reasoning) {
		if !slices.Contains(reasoningFields, k) {
			return fmt.Errorf("reasoning.%s: unrecognized field", k)
		}
	}
	var effort string
	if raw, ok := top.Reasoning["effort"]; ok {
		if json.Unmarshal(raw, &effort) != nil || !slices.Contains(reasoningEfforts, effort) {
			return fmt.Errorf("reasoning.effort: must be one of %s", strings.Join(reasoningEfforts, ", "))
		}
	}
	var enabled *bool
	if raw, ok := top.Reasoning["enabled"]; ok {
		_ = json.Unmarshal(raw, &enabled)
	}
	off := effort == "none" || (enabled != nil && !*enabled)
	name := strings.ToLower(top.Model)
	if off && slices.ContainsFunc(mandatoryReasoning, func(p string) bool { return strings.HasPrefix(name, p) }) {
		return errors.New("reasoning is mandatory for this endpoint and cannot be disabled")
	}
	return nil
}

func extraInput(path string) error { return fmt.Errorf("%s: Extra inputs are not permitted", path) }

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
