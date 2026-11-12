// SPDX-License-Identifier: Apache-2.0

package fakemodel

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/project"
)

// AnyTool matches a tool result from any tool in [When.ToolResult].
const AnyTool = "*"

// Script is the scripted behaviour of one model name.
type Script struct {
	// Model is the model name requests select the script by.
	Model string `yaml:"model"`
	// Faults are injected into every request for this model.
	Faults Faults `yaml:"faults,omitempty"`
	// Turns are tried in order; the first whose When holds answers the request.
	Turns []Turn `yaml:"turns"`
}

// Faults make a script misbehave so retries and timeouts can be tested.
type Faults struct {
	// Unavailable answers the first N requests to this model with HTTP 503.
	Unavailable int `yaml:"unavailable,omitempty"`
	// Delay waits this long before answering (after any 503s), or until the client gives up.
	Delay time.Duration `yaml:"delay,omitempty"`
	// Truncate writes about half of the response (half the SSE events when streaming, half the
	// JSON body otherwise) and then aborts the connection.
	Truncate bool `yaml:"truncate,omitempty"`
	// RateLimit answers the first N requests to this model with HTTP 429 and a Retry-After
	// header of RetryAfter seconds (after any 503s).
	RateLimit int `yaml:"rateLimit,omitempty"`
	// RetryAfter is the Retry-After value, in seconds, sent with a 429. Zero means 1.
	RetryAfter int `yaml:"retryAfter,omitempty"`
	// Refuse answers the first N requests to this model (after any 503s and 429s) with HTTP
	// RefuseStatus, a client error such as a bad key (401) or no access (403).
	Refuse int `yaml:"refuse,omitempty"`
	// RefuseStatus is the status sent for Refuse. Zero means 400.
	RefuseStatus int `yaml:"refuseStatus,omitempty"`
	// DoNotRetry adds the header x-should-retry: false to every scripted 503, 429 and refusal, the
	// gateway's mark for a refusal no layer retries.
	DoNotRetry bool `yaml:"doNotRetry,omitempty"`
	// ContextLength answers every request with the context-length-exceeded error: HTTP 400,
	// code context_length_exceeded (OpenAI-compatible) or "prompt is too long" (Anthropic).
	ContextLength bool `yaml:"contextLength,omitempty"`
	// OverloadedMidStream makes a streaming response send about half of its events and then an
	// "overloaded" error event, and end the stream cleanly. A non-streaming request gets the
	// 503 overloaded error instead.
	OverloadedMidStream bool `yaml:"overloadedMidStream,omitempty"`
	// StallMidStream makes a streaming response send about half of its events and then hold the
	// stream open, sending nothing, until the client or the gateway in between closes it. It is how
	// a test cancels a stream mid-way. A non-streaming request is answered as usual.
	StallMidStream bool `yaml:"stallMidStream,omitempty"`
	// SplitWrites writes every SSE event of a stream in small flushed pieces (7 bytes), so that a
	// reader sees each event cut at many points. It changes the bytes' timing, never their content.
	SplitWrites bool `yaml:"splitWrites,omitempty"`
	// Lockstep holds every request to this model after it is received until the test releases
	// a response with [Server.Release]; [Server.Waiting] counts the held requests. It works only
	// in-process: cmd/koinon-fakemodel has no release endpoint, so a script loaded by the binary
	// with lockstep set holds every request until the client gives up.
	Lockstep bool `yaml:"lockstep,omitempty"`
}

// Turn is one scripted assistant reply. It has text, tool calls, or both (text first).
type Turn struct {
	When      When       `yaml:"when,omitempty"`
	Text      string     `yaml:"text,omitempty"`
	ToolCalls []ToolCall `yaml:"toolCalls,omitempty"`
	// Usage is reported to the client. Zero values are estimated from the request and reply size.
	Usage Usage `yaml:"usage,omitempty"`
}

// When decides whether a turn answers a request. All set fields must hold.
type When struct {
	// ToolResult, when empty, requires that the request's last message is not a tool result
	// (normally a user message). When set to a tool name, the last message must carry that
	// tool's result; [AnyTool] accepts a result from any tool. A result from the tool behind a
	// harness's prefix for Koinon's MCP server (koinon_<name>, mcp__koinon__<name>) also
	// matches, so one script fits every harness.
	ToolResult string `yaml:"toolResult,omitempty"`
	// UserContains requires the latest user text to contain this substring.
	UserContains string `yaml:"userContains,omitempty"`
	// SystemContains requires the system prompt to contain this substring: the text of every
	// OpenAI system or developer message, or Anthropic's top-level system field.
	SystemContains string `yaml:"systemContains,omitempty"`
	// AssistantTurns, when set, requires exactly this many assistant messages in the request.
	// Use it to tell apart repeated steps of the same shape.
	AssistantTurns *int `yaml:"assistantTurns,omitempty"`
}

// ToolCall is a tool call the model makes.
type ToolCall struct {
	// ID is the tool call ID. Empty means a deterministic generated ID.
	ID string `yaml:"id,omitempty"`
	// Name is the tool to call. Harnesses prefix MCP tools with their server name (OpenCode
	// koinon_<tool>; other harnesses may use mcp__koinon__<tool>): when the request declares no tool of
	// this name but exactly one of this name behind such a prefix, the call uses that tool's name.
	Name      string         `yaml:"name"`
	Arguments map[string]any `yaml:"arguments,omitempty"`
}

// Usage is the token usage reported for a turn.
type Usage struct {
	// InputTokens is the full prompt size, cached tokens included (OpenAI prompt_tokens).
	// Anthropic responses report InputTokens-CachedInputTokens-CacheWriteTokens as input_tokens.
	InputTokens int `yaml:"inputTokens,omitempty"`
	// CachedInputTokens is the part of InputTokens read from the prompt cache.
	CachedInputTokens int `yaml:"cachedInputTokens,omitempty"`
	// CacheWriteTokens is the part of InputTokens written to the prompt cache. Anthropic responses
	// report it as cache_creation_input_tokens; chat completions have no field for it.
	CacheWriteTokens int `yaml:"cacheWriteTokens,omitempty"`
	OutputTokens     int `yaml:"outputTokens,omitempty"`
	// ReasoningTokens is the part of OutputTokens spent thinking. It is reported only when set:
	// chat completions as completion_tokens_details.reasoning_tokens, Messages as
	// output_tokens_details.thinking_tokens.
	ReasoningTokens int `yaml:"reasoningTokens,omitempty"`
}

// Validate reports problems that would make the script unusable.
func (s Script) Validate() error {
	var errs []error
	if s.Model == "" {
		errs = append(errs, errors.New("model is required"))
	}
	if len(s.Turns) == 0 {
		errs = append(errs, errors.New("at least one turn is required"))
	}
	if s.Faults.Unavailable < 0 || s.Faults.Delay < 0 || s.Faults.RateLimit < 0 || s.Faults.RetryAfter < 0 {
		errs = append(errs, errors.New("faults must not be negative"))
	}
	for i, t := range s.Turns {
		if t.Text == "" && len(t.ToolCalls) == 0 {
			errs = append(errs, fmt.Errorf("turn %d: needs text or toolCalls", i))
		}
		for j, c := range t.ToolCalls {
			if c.Name == "" {
				errs = append(errs, fmt.Errorf("turn %d: tool call %d: name is required", i, j))
			}
		}
		u := t.Usage
		if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens < 0 || u.CacheWriteTokens < 0 ||
			u.CachedInputTokens+u.CacheWriteTokens > u.InputTokens {
			errs = append(errs, fmt.Errorf("turn %d: invalid usage", i))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("script %q: %w", s.Model, err)
	}
	return nil
}

// ParseScript decodes one YAML script. Unknown fields are errors.
func ParseScript(data []byte) (Script, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Script
	if err := dec.Decode(&s); err != nil {
		return Script{}, fmt.Errorf("decode script: %w", err)
	}
	return s, s.Validate()
}

// LoadDir parses every *.yaml and *.yml file in dir (not recursively), one script per file.
func LoadDir(dir string) ([]Script, error) {
	return LoadFS(os.DirFS(dir), ".")
}

// LoadFS is [LoadDir] on a directory of fsys, such as an embedded one.
func LoadFS(fsys fs.FS, dir string) ([]Script, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read scripts: %w", err)
	}
	var scripts []Script
	seen := map[string]string{}
	for _, e := range entries {
		ext := path.Ext(e.Name())
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		name := path.Join(dir, e.Name())
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read script: %w", err)
		}
		s, err := ParseScript(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if prev, ok := seen[s.Model]; ok {
			return nil, fmt.Errorf("%s: model %q already defined in %s", name, s.Model, prev)
		}
		seen[s.Model] = name
		scripts = append(scripts, s)
	}
	return scripts, nil
}

// conversation is what turn selection looks at, extracted from either API's request.
type conversation struct {
	// toolResults names the tools whose results form the trailing message(s); nil when the last
	// message is not a tool result. A result whose call cannot be resolved has an empty name.
	toolResults    []string
	lastUserText   string
	systemText     string
	assistantTurns int
	// tools are the tool names the request declares.
	tools []string
}

// mcpPrefixes are what harnesses put before the tools of Koinon's MCP server, which every
// harness names project.Name: OpenCode <server>_<tool>; the mcp__<server>__<tool> form is kept for other harnesses.
// agentgateway puts the target in front of every tool (prefixMode always), and Koinon's own tools are
// the "github" target.
var mcpPrefixes = []string{project.Name + "_", "mcp__" + project.Name + "__", project.Name + "_github_", "mcp__" + project.Name + "__github_"}

// toolMatches reports whether a tool name is the scripted name, possibly behind a harness's
// MCP prefix (koinon_create_pull_request, mcp__koinon__create_pull_request). Another tool that
// merely ends in the name (foo_create_pull_request) does not match.
func toolMatches(name, scripted string) bool {
	if name == scripted {
		return true
	}
	for _, p := range mcpPrefixes {
		if rest, ok := strings.CutPrefix(name, p); ok && rest == scripted {
			return true
		}
	}
	return false
}

// resolveTools returns the calls with each name replaced by the request's prefixed tool of that
// name, when the request declares no tool of the exact name and exactly one prefixed one.
func resolveTools(calls []ToolCall, tools []string) []ToolCall {
	if len(calls) == 0 || len(tools) == 0 {
		return calls
	}
	out := slices.Clone(calls)
	for i, c := range out {
		if slices.Contains(tools, c.Name) {
			continue
		}
		var found []string
		for _, t := range tools {
			if toolMatches(t, c.Name) {
				found = append(found, t)
			}
		}
		if len(found) == 1 {
			out[i].Name = found[0]
		}
	}
	return out
}

func (w When) holds(c conversation) bool {
	switch w.ToolResult {
	case "":
		if c.toolResults != nil {
			return false
		}
	case AnyTool:
		if c.toolResults == nil {
			return false
		}
	default:
		if !slices.ContainsFunc(c.toolResults, func(n string) bool { return toolMatches(n, w.ToolResult) }) {
			return false
		}
	}
	if w.UserContains != "" && !strings.Contains(c.lastUserText, w.UserContains) {
		return false
	}
	if w.SystemContains != "" && !strings.Contains(c.systemText, w.SystemContains) {
		return false
	}
	if w.AssistantTurns != nil && *w.AssistantTurns != c.assistantTurns {
		return false
	}
	return true
}

// selectTurn returns the index of the first turn whose condition holds, or -1.
func (s Script) selectTurn(c conversation) int {
	for i, t := range s.Turns {
		if t.When.holds(c) {
			return i
		}
	}
	return -1
}
