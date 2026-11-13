// SPDX-License-Identifier: Apache-2.0

package runevidence

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/mcpservers"
)

// The converter from an export to the leak guard's replay corpus (#51, #352). The corpus format is
// documented at the top of internal/mcpguard/corpus_test.go; this file writes the `call` and `feed`
// lines, and the `server` lines for the servers the run called.
//
// Where each line comes from:
//
//   - call: an acp.tool_call event of the run's events whose title is an MCP tool of Koinon's
//     (MCPToolPrefix + server + "_" + tool, or the tool alone), paired in order with the leak
//     guard's decision records of that server and tool. The decision gives the server, the tool and
//     the home generation; the event gives the arguments (rawInput) and the result (the finished
//     update's rawOutput or content text).
//   - feed: a finished tool call of another kind. The legs follow agentd's feed: a repository read,
//     search, shell command or edit sets private data, a fetch sets untrusted input, any other kind
//     sets both. The texts are the finished update's content text.
//
// The org's own declarations are not in the export. A --declarations file (org, server and tool
// lines, taken from the admin API) is copied in front of the converted lines and its server lines
// win; for a server it does not declare, the converter writes a `server` line of kind "other" with
// no legs and reports the name, because a replay with legs it guessed would measure nothing.

// CorpusOptions configure Corpus.
type CorpusOptions struct {
	// MCPToolPrefix is what the harness puts before an MCP tool's name (harness.Descriptor.MCPToolPrefix).
	MCPToolPrefix string
	// Declarations are lines copied in front of the output (may be nil).
	Declarations []json.RawMessage
}

// CorpusReport says what the conversion could and could not do.
type CorpusReport struct {
	Calls, Feeds int
	// UndeclaredServers were called but have no server line in Declarations.
	UndeclaredServers []string
	// UnpairedCalls are tool calls of an MCP tool with no decision record (or the other way round):
	// they are left out, because a call without its server and generation cannot be replayed.
	UnpairedCalls, UnpairedDecisions int
	// OwnCalls are MCP-prefixed calls no decision names (Koinon's own tools): written as feeds that set
	// both legs, as the guard held them.
	OwnCalls int
}

type eventLine struct {
	Type    string          `json:"type"`
	RunID   string          `json:"run_id"`
	Seq     int64           `json:"seq"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

type decisionLine struct {
	Type       string `json:"type"`
	RunID      string `json:"run_id"`
	Generation string `json:"generation"`
	Server     string `json:"server"`
	Tool       string `json:"tool"`
	SinkClass  string `json:"sink_class"`
}

type update struct {
	Update   string          `json:"sessionUpdate"`
	ID       string          `json:"toolCallId"`
	Kind     string          `json:"kind"`
	Title    string          `json:"title"`
	Status   string          `json:"status"`
	RawInput json.RawMessage `json:"rawInput"`
	Output   json.RawMessage `json:"rawOutput"`
	Content  []struct {
		Content *struct {
			Text string `json:"text"`
		} `json:"content"`
		NewText string `json:"newText"`
	} `json:"content"`
}

// toolCall follows one tool call across its updates, as agentd's feed does: a later update may leave
// out what the first carried.
type toolCall struct {
	run, kind, title string
	seq              int
	input            json.RawMessage
	output           json.RawMessage
	texts            []string
	done             bool
}

type queueKey struct{ run, server, tool string }

// nameKey is a harness tool name's candidates: "<server>_<tool>" and, for an ask-tier call,
// "<server>-ask_<tool>". A name with underscores can be split in more than one way, so the key keeps
// every candidate and the first with a decision left wins.
type nameKey struct{ run, name string }

// Corpus reads an export's run_events and decisions and writes corpus lines to w.
func Corpus(events, decisions io.Reader, w io.Writer, o CorpusOptions) (CorpusReport, error) {
	var rep CorpusReport
	decs, err := readLines[decisionLine](decisions, "decision")
	if err != nil {
		return rep, err
	}
	evs, err := readLines[eventLine](events, "run_event")
	if err != nil {
		return rep, err
	}

	// Decisions per run, per server/tool, in order.
	queues := map[queueKey][]decisionLine{}
	names := map[nameKey][]queueKey{}
	for _, d := range decs {
		k := queueKey{d.RunID, d.Server, d.Tool}
		if _, seen := queues[k]; !seen {
			for _, n := range []string{d.Server + "_" + d.Tool, d.Server + mcpservers.AskSuffix + "_" + d.Tool} {
				names[nameKey{d.RunID, n}] = append(names[nameKey{d.RunID, n}], k)
			}
		}
		queues[k] = append(queues[k], d)
	}

	// The tool calls in the order they were first seen.
	var order []*toolCall
	calls := map[string]*toolCall{}
	for _, ev := range evs {
		if ev.Kind != "acp.tool_call" && ev.Kind != "acp.tool_call_update" {
			continue
		}
		var u update
		if json.Unmarshal(ev.Payload, &u) != nil || u.ID == "" {
			continue
		}
		id := ev.RunID + "/" + u.ID
		c := calls[id]
		if c == nil {
			c = &toolCall{run: ev.RunID, seq: int(ev.Seq)}
			calls[id] = c
			order = append(order, c)
		}
		if u.Kind != "" {
			c.kind = u.Kind
		}
		if u.Update == "tool_call" && u.Title != "" {
			c.title = u.Title
		}
		if len(u.RawInput) > 0 && string(u.RawInput) != "null" {
			c.input = u.RawInput
		}
		if len(u.Output) > 0 && string(u.Output) != "null" {
			c.output = u.Output
		}
		for _, ct := range u.Content {
			if ct.Content != nil && ct.Content.Text != "" {
				c.texts = append(c.texts, ct.Content.Text)
			}
			if ct.NewText != "" {
				c.texts = append(c.texts, ct.NewText)
			}
		}
		if u.Status == "completed" || u.Status == "failed" {
			c.done = true
		}
	}

	enc := json.NewEncoder(w)
	for _, raw := range o.Declarations {
		if _, err := w.Write(append(slices.Clone([]byte(raw)), '\n')); err != nil {
			return rep, err
		}
	}
	declared := declaredServers(o.Declarations)
	called := map[string]bool{}
	var body []map[string]any
	for _, c := range order {
		name, mcp := strings.CutPrefix(c.title, o.MCPToolPrefix)
		cands := names[nameKey{c.run, name}]
		if mcp && o.MCPToolPrefix != "" || (o.MCPToolPrefix == "" && len(cands) > 0) {
			var k queueKey
			for _, cand := range cands {
				if len(queues[cand]) > 0 {
					k = cand
					break
				}
			}
			q := queues[k]
			if len(q) == 0 {
				if len(cands) > 0 {
					rep.UnpairedCalls++
					continue
				}
				// No decision names this tool in the run: one of Koinon's own tools (no record), or a
				// call the guard refused before it knew the org. Either way its result set both legs in
				// the real run (OwnResultLegs: a tool nobody declared sets both), so the replay gets a feed
				// that does the same, and a replayed call is not passed for lack of legs.
				if !c.done {
					rep.UnpairedCalls++
					continue
				}
				{
					legs := mcpguard.OwnResultLegs(name)
					line := map[string]any{
						"type": "feed", "run": c.run, "seq": c.seq, "tool": name,
						"legs":       map[string]bool{"untrusted_input": legs.UntrustedInput, "private_data": legs.PrivateData},
						"generation": generationOf(decs, c.run),
					}
					if len(c.texts) > 0 {
						line["texts"] = c.texts
					}
					body = append(body, line)
					rep.OwnCalls++
				}
				continue
			}
			d := q[0]
			queues[k] = q[1:]
			args := c.input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			line := map[string]any{
				"type": "call", "generation": d.Generation, "run": c.run, "seq": c.seq, "server": d.Server, "tool": d.Tool,
				"args": args,
			}
			if res := resultOf(c); res != nil {
				line["result"] = res
			}
			body = append(body, line)
			called[d.Server] = true
			rep.Calls++
			continue
		}
		if !c.done || c.kind == "" && len(c.texts) == 0 {
			continue
		}
		legs := feedLegs(c.kind)
		line := map[string]any{"type": "feed", "run": c.run, "seq": c.seq, "tool": c.kind, "legs": legs}
		// The generation of a feed is the one of the run's decisions; a run has one in nearly
		// every case, and the replay needs it to find the session.
		line["generation"] = generationOf(decs, c.run)
		if len(c.texts) > 0 {
			line["texts"] = c.texts
		}
		body = append(body, line)
		rep.Feeds++
	}
	for k, q := range queues {
		_ = k
		rep.UnpairedDecisions += len(q)
	}
	servers := make([]string, 0, len(called))
	for s := range called {
		servers = append(servers, s)
	}
	slices.Sort(servers)
	for _, s := range servers {
		if declared[s] {
			continue
		}
		rep.UndeclaredServers = append(rep.UndeclaredServers, s)
		if err := enc.Encode(map[string]any{"type": "server", "server": s, "kind": "other"}); err != nil {
			return rep, err
		}
	}
	for _, l := range body {
		if err := enc.Encode(l); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func generationOf(decs []decisionLine, run string) string {
	for _, d := range decs {
		if d.RunID == run && d.Generation != "" {
			return d.Generation
		}
	}
	return ""
}

// feedLegs is agentd's feed rule (internal/agentd/taint.go) for a finished tool call of kind.
func feedLegs(kind string) map[string]bool {
	switch kind {
	case "read", "search", "execute", "edit", "delete", "move":
		return map[string]bool{"untrusted_input": false, "private_data": true}
	case "fetch":
		return map[string]bool{"untrusted_input": true, "private_data": false}
	}
	return map[string]bool{"untrusted_input": true, "private_data": true}
}

// resultOf is the CallToolResult the replay reads: the text a finished call produced.
func resultOf(c *toolCall) map[string]any {
	text := strings.Join(c.texts, "\n")
	if text == "" && len(c.output) > 0 {
		var s string
		if json.Unmarshal(c.output, &s) == nil {
			text = s
		} else {
			text = string(c.output)
		}
	}
	if text == "" {
		return nil
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func declaredServers(lines []json.RawMessage) map[string]bool {
	out := map[string]bool{}
	for _, raw := range lines {
		var l struct {
			Type   string `json:"type"`
			Server string `json:"server"`
		}
		if json.Unmarshal(raw, &l) == nil && l.Type == "server" {
			out[l.Server] = true
		}
	}
	return out
}

func readLines[T any](r io.Reader, want string) ([]T, error) {
	var out []T
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for n := 1; sc.Scan(); n++ {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(sc.Bytes(), &probe); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if probe.Type != want {
			continue
		}
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}
