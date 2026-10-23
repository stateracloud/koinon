// SPDX-License-Identifier: Apache-2.0

package fakemodel

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/procoduck/koinon/internal/project"
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
	// koinon_<tool>, Hermes and pi mcp__koinon__<tool>): when the request declares no tool of
	// this name but exactly one of this name behind such a prefix, the call uses that tool's name.
	Name      string         `yaml:"name"`
	Arguments map[string]any `yaml:"arguments,omitempty"`
}

// Usage is the token usage reported for a turn.
type Usage struct {
	// InputTokens is the full prompt size, cached tokens included (OpenAI prompt_tokens).
	// Anthropic responses report InputTokens-CachedInputTokens as input_tokens.
	InputTokens int `yaml:"inputTokens,omitempty"`
	// CachedInputTokens is the part of InputTokens read from the prompt cache.
	CachedInputTokens int `yaml:"cachedInputTokens,omitempty"`
	OutputTokens      int `yaml:"outputTokens,omitempty"`
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
	if s.Faults.Unavailable < 0 || s.Faults.Delay < 0 {
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
		if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens < 0 ||
			u.CachedInputTokens > u.InputTokens {
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read scripts: %w", err)
	}
	var scripts []Script
	seen := map[string]string{}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path) //nolint:gosec // test scripts from an operator-chosen directory
		if err != nil {
			return nil, fmt.Errorf("read script: %w", err)
		}
		s, err := ParseScript(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if prev, ok := seen[s.Model]; ok {
			return nil, fmt.Errorf("%s: model %q already defined in %s", path, s.Model, prev)
		}
		seen[s.Model] = path
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
// flavor names project.Name: OpenCode <server>_<tool>, Hermes and pi mcp__<server>__<tool>.
var mcpPrefixes = []string{project.Name + "_", "mcp__" + project.Name + "__"}

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
