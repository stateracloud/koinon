// SPDX-License-Identifier: Apache-2.0

// Command acpdrive is throwaway spike code for step 7 of docs/spikes/openshell.md: an ACP
// client that starts a harness inside an OpenShell sandbox through
// `openshell sandbox exec --no-tty` and drives one session/new or session/load plus one
// session/prompt, writing every session update to a JSON-lines transcript.
//
// Harness-local permission requests are granted: the sandbox is the boundary, and risky
// actions go through the approval MCP tool (ADR 0042), not through ACP permissions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

type client struct {
	mu  sync.Mutex
	out *json.Encoder
	msg strings.Builder
}

func (c *client) record(kind string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.out.Encode(map[string]any{"t": time.Now().UTC().Format(time.RFC3339Nano), "kind": kind, "data": v})
}

func (c *client) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil:
		c.mu.Lock()
		c.msg.WriteString(u.AgentMessageChunk.Content.Text.Text)
		c.mu.Unlock()
	case u.ToolCall != nil:
		fmt.Printf("  tool call %s %q status=%s\n", u.ToolCall.ToolCallId, u.ToolCall.Title, u.ToolCall.Status)
	case u.ToolCallUpdate != nil && u.ToolCallUpdate.Status != nil:
		fmt.Printf("  tool update %s status=%s\n", u.ToolCallUpdate.ToolCallId, *u.ToolCallUpdate.Status)
	}
	c.record("session/update", n)
	return nil
}

func (c *client) RequestPermission(_ context.Context, r acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.record("session/request_permission", r)
	for _, o := range r.Options {
		if strings.HasPrefix(string(o.Kind), "allow") {
			fmt.Printf("  permission %q -> %s\n", deref(r.ToolCall.Title), o.Name)
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(o.OptionId)}, nil
		}
	}
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}

var errUnsupported = errors.New("acpdrive: not supported; the harness works on its own sandbox filesystem")

func (c *client) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errUnsupported
}

func (c *client) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errUnsupported
}

func (c *client) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errUnsupported
}

func (c *client) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errUnsupported
}

func (c *client) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errUnsupported
}

func (c *client) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errUnsupported
}

func (c *client) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errUnsupported
}

func main() {
	sandbox := flag.String("sandbox", "", "OpenShell sandbox name")
	workspace := flag.String("workspace", "spike-a", "OpenShell workspace")
	harness := flag.String("cmd", "", "harness command inside the sandbox, e.g. 'opencode acp'")
	cwd := flag.String("cwd", "/sandbox/work", "session working directory inside the sandbox")
	load := flag.String("load", "", "session id to load instead of starting a new session")
	prompt := flag.String("prompt", "", "prompt text")
	transcript := flag.String("out", "transcript.jsonl", "JSON-lines transcript path")
	timeout := flag.Duration("timeout", 15*time.Minute, "overall timeout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	f, err := os.Create(*transcript)
	must(err, "transcript")
	defer f.Close()
	c := &client{out: json.NewEncoder(f)}

	args := append([]string{"sandbox", "exec", "--workspace", *workspace, "--name", *sandbox, "--no-tty", "--"}, strings.Fields(*harness)...)
	cmd := exec.CommandContext(ctx, os.Getenv("OPENSHELL"), args...)
	stdin, err := cmd.StdinPipe()
	must(err, "stdin")
	stdout, err := cmd.StdoutPipe()
	must(err, "stdout")
	stderr, err := os.Create(*transcript + ".stderr")
	must(err, "stderr file")
	cmd.Stderr = stderr
	must(cmd.Start(), "start harness")

	conn := acp.NewClientSideConnection(c, stdin, stdout)
	t0 := time.Now()
	init, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	must(err, "7.1 initialize")
	c.record("initialize", init)
	caps, _ := json.Marshal(init.AgentCapabilities)
	fmt.Printf("7.1 initialize ok in %s: agent=%v loadSession=%v caps=%s\n", time.Since(t0).Round(time.Millisecond), init.AgentInfo, init.AgentCapabilities.LoadSession, caps)

	var sid acp.SessionId
	if *load == "" {
		s, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: *cwd, McpServers: []acp.McpServer{}})
		must(err, "7.2 session/new")
		c.record("session/new", s)
		sid = s.SessionId
		fmt.Printf("7.2 session/new id=%s\n", sid)
	} else {
		s, err := conn.LoadSession(ctx, acp.LoadSessionRequest{SessionId: acp.SessionId(*load), Cwd: *cwd, McpServers: []acp.McpServer{}})
		must(err, "7.3 session/load")
		c.record("session/load", s)
		sid = acp.SessionId(*load)
		fmt.Printf("7.3 session/load id=%s ok\n", sid)
	}

	if *prompt != "" {
		t1 := time.Now()
		r, err := conn.Prompt(ctx, acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{{Text: &acp.ContentBlockText{Type: "text", Text: *prompt}}}})
		must(err, "7.2 session/prompt")
		c.record("session/prompt", r)
		c.mu.Lock()
		reply := c.msg.String()
		c.mu.Unlock()
		fmt.Printf("7.2 prompt done in %s stopReason=%s\nreply: %.600s\n", time.Since(t1).Round(time.Second), r.StopReason, strings.TrimSpace(reply))
	}
	_ = stdin.Close()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func must(err error, what string) {
	if err != nil {
		log.Fatalf("%s: %v", what, err)
	}
}
