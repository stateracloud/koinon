// SPDX-License-Identifier: Apache-2.0

package mcpguard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"

	g "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// The replay with the fake model in the loop (#70: "run against the fake model server"). Per
// generation, a minimal agent loop asks the fake model (ADR 0025, in-process: no socket, no network)
// for its next step; the model's script answers with the corpus's next call as a tool_use; the loop
// takes the tool_use's name and input as the call, decides it on the replay's guard and answers with
// the recorded result as the tool_result. So the arguments the guard decides are the ones a model
// emitted, after the model's JSON round trip, not the corpus's own bytes.
//
// What it does not cover: the gateway hop (agentgateway, the MCP session, Guard.Check against
// Postgres) and a real harness. Those are the escape suite's and the kind e2e's (#70's exfil case).

// The fake model emits a tool call's arguments with their keys sorted (a Go map, as any encoder
// that sorts keys would), so the mail's subject and body arrive as body, subject. The guard reads
// the payload in canonical order, the keys sorted (#256), so the order an encoder wrote the keys in
// decides nothing: every call the model loop sends is decided exactly as the direct replay decides
// the corpus's own bytes, the split shapes included.

// modelToolName is the name a corpus tool is declared under to the fake model.
func modelToolName(server, tool string) string { return server + "__" + tool }

// modelScript scripts the model of one generation: turn k answers the request with k assistant
// turns by emitting the generation's k-th call, and the last turn ends the conversation.
func modelScript(model string, calls []corpusLine) (fakemodel.Script, error) {
	sc := fakemodel.Script{Model: model}
	for k, l := range calls {
		var args map[string]any
		if err := json.Unmarshal(l.Args, &args); err != nil {
			return sc, fmt.Errorf("%s #%d: arguments are not an object: %w", l.Run, l.Seq, err)
		}
		w := fakemodel.When{AssistantTurns: new(k)}
		if k > 0 {
			w.ToolResult = fakemodel.AnyTool
		}
		sc.Turns = append(sc.Turns, fakemodel.Turn{When: w, ToolCalls: []fakemodel.ToolCall{{Name: modelToolName(l.Server, l.Tool), Arguments: args}}})
	}
	sc.Turns = append(sc.Turns, fakemodel.Turn{When: fakemodel.When{ToolResult: fakemodel.AnyTool, AssistantTurns: new(len(calls))}, Text: "Done."})
	return sc, nil
}

type modelBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	Text  string          `json:"text,omitempty"`
}

type modelMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// ask sends one Messages request to the fake model and returns the reply's content blocks.
func ask(ctx context.Context, srv http.Handler, model string, tools []map[string]any, msgs []modelMessage) ([]modelBlock, error) {
	body, err := json.Marshal(map[string]any{"model": model, "max_tokens": 1024, "tools": tools, "messages": msgs})
	if err != nil {
		return nil, err
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, fmt.Errorf("fake model: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Content []modelBlock `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return nil, err
	}
	return resp.Content, nil
}

// replayThroughModel replays the given generations of lines, in corpus order within each, with
// every call's arguments taken from the fake model's tool_use, and returns the decision on every
// call of those generations.
func replayThroughModel(ctx context.Context, lines []corpusLine, gens []string) ([]replayed, error) {
	r, err := newReplay(lines, replayOptions{})
	if err != nil {
		return nil, err
	}
	var tools []map[string]any
	for _, l := range lines {
		if l.Type == "tool" {
			tools = append(tools, map[string]any{"name": modelToolName(l.Server, l.Tool), "input_schema": map[string]any{"type": "object"}})
		}
	}
	byGen := map[string][]corpusLine{}
	scripts := make([]fakemodel.Script, 0, len(gens))
	for _, l := range lines {
		if l.Type == "call" || l.Type == "feed" {
			byGen[l.Generation] = append(byGen[l.Generation], l)
		}
	}
	for i, gen := range gens {
		var calls []corpusLine
		for _, l := range byGen[gen] {
			if l.Type == "call" {
				calls = append(calls, l)
			}
		}
		if len(calls) == 0 {
			return nil, fmt.Errorf("generation %q has no calls", gen)
		}
		sc, err := modelScript(fmt.Sprintf("replay-%d", i), calls)
		if err != nil {
			return nil, err
		}
		scripts = append(scripts, sc)
	}
	srv, err := fakemodel.New(scripts...)
	if err != nil {
		return nil, err
	}
	var out []replayed
	for i, gen := range gens {
		model := fmt.Sprintf("replay-%d", i)
		msgs := []modelMessage{{Role: "user", Content: "Work on the task of " + gen + "."}}
		for _, l := range byGen[gen] {
			if l.Type == "feed" {
				if _, err := r.apply(ctx, l); err != nil {
					return nil, err
				}
				continue
			}
			blocks, err := ask(ctx, srv, model, tools, msgs)
			if err != nil {
				return nil, err
			}
			if len(blocks) != 1 || blocks[0].Type != "tool_use" {
				return nil, fmt.Errorf("%s #%d: the model answered %+v, not one tool_use", l.Run, l.Seq, blocks)
			}
			use := blocks[0]
			server, tool, ok := strings.Cut(use.Name, "__")
			if !ok {
				return nil, fmt.Errorf("%s #%d: tool %q is not a corpus tool", l.Run, l.Seq, use.Name)
			}
			call := l
			call.Server, call.Tool, call.Args = server, tool, use.Input
			d, err := r.apply(ctx, call)
			if err != nil {
				return nil, err
			}
			out = append(out, *d)
			text := "refused by the leak guard: " + d.verdict.Reason
			if d.verdict.Action != Refuse {
				text = strings.Join(Strings(l.Result), "\n")
			}
			msgs = append(msgs,
				modelMessage{Role: "assistant", Content: []modelBlock{use}},
				modelMessage{Role: "user", Content: []map[string]any{{"type": "tool_result", "tool_use_id": use.ID, "content": text}}},
			)
		}
		blocks, err := ask(ctx, srv, model, tools, msgs)
		if err != nil {
			return nil, err
		}
		if len(blocks) != 1 || blocks[0].Type != "text" {
			return nil, fmt.Errorf("%s: the model did not end the conversation: %+v", gen, blocks)
		}
	}
	return out, nil
}

// sampleGenerations picks, per server/tool and expectation, the first generation of the corpus with
// such a call, plus the generations whose case has one of the extra shapes. The result is in corpus
// order.
func sampleGenerations(lines []corpusLine, shapes ...string) []string {
	want, seen := map[string]bool{}, map[string]bool{}
	for _, l := range lines {
		if l.Type != "call" {
			continue
		}
		k := l.Server + "/" + l.Tool + " " + l.Expect
		_, shape, _ := strings.Cut(l.Case, "/")
		if !seen[k] || (shape != "" && slices.Contains(shapes, shape)) {
			seen[k], want[l.Generation] = true, true
		}
	}
	var out []string
	done := map[string]bool{}
	for _, l := range lines {
		if want[l.Generation] && !done[l.Generation] {
			done[l.Generation] = true
			out = append(out, l.Generation)
		}
	}
	return out
}

var _ = g.Describe("Replay with the fake model in the loop (#70)", func() {
	// Every call of the sampled generations is decided as the direct replay decides it, and the
	// expectations hold on what the model sent.
	sameAsDirect := func(ctx g.SpecContext, path string, extra []string, shapes ...string) []replayed {
		g.GinkgoHelper()
		lines := mustCorpus(path)
		gens := sampleGenerations(lines, shapes...)
		for _, gen := range extra {
			if !slices.Contains(gens, gen) {
				gens = append(gens, gen)
			}
		}
		viaModel, err := replayThroughModel(ctx, lines, gens)
		Expect(err).NotTo(HaveOccurred())
		inSample := map[string]bool{}
		for _, gen := range gens {
			inSample[gen] = true
		}
		var direct []replayed
		for _, c := range mustReplay(ctx, lines, replayOptions{}) {
			if inSample[c.line.Generation] {
				direct = append(direct, c)
			}
		}
		Expect(viaModel).To(HaveLen(len(direct)))
		for i, c := range viaModel {
			d := direct[i]
			Expect(c.line.Server+"/"+c.line.Tool).To(Equal(d.line.Server+"/"+d.line.Tool), where(c))
			Expect(c.line.Args).To(MatchJSON(d.line.Args), "%s: the model sent other arguments", where(c))
			Expect(c.class).To(Equal(d.class), where(c))
			Expect(c.bytes).To(Equal(d.bytes), where(c))
			Expect(c.verdict.Action).To(Equal(d.verdict.Action), where(c))
			Expect(c.verdict.Reason).To(Equal(d.verdict.Reason), where(c))
			Expect(c.verdict.Reasons).To(Equal(d.verdict.Reasons), where(c))
		}
		return viaModel
	}

	g.It("decides a sample of the benign corpus, every server and tool, as the direct replay does, with no text match", func(ctx g.SpecContext) {
		// gen-09 holds the long research run that goes over the byte cap.
		calls := sameAsDirect(ctx, benignCorpus, []string{"gen-09"})
		tools, overCap := map[string]bool{}, 0
		for _, c := range calls {
			if c.verdict.Reason == ReasonByteCap {
				overCap++
			}
			tools[c.line.Server+"/"+c.line.Tool] = true
			Expect(c.verdict.Match).To(BeNil(), "%s: a benign payload matched private text", where(c))
			Expect(c.verdict.Reason).NotTo(BeElementOf(ReasonTaint, ReasonURLRule, ReasonAmbiguous, ReasonCheck), where(c))
		}
		Expect(tools).To(HaveLen(9), "every server/tool of the benign corpus: %v", tools)
		Expect(overCap).To(BeNumerically(">", 0), "the long research run reaches the byte cap through the model too")
	})

	g.It("blocks a sample of the planted leaks per sink class, split ones included, on what the model sent", func(ctx g.SpecContext) {
		calls := sameAsDirect(ctx, leakCorpus, nil, "split-args", "split-calls", "split-args-b64-halves", "whole-b64")
		caught := map[string]int{}
		// The split whose halves the model reorders (subject and body arrive sorted) is in the sample,
		// and decided as the direct replay decides it (#256).
		Expect(slices.ContainsFunc(calls, func(c replayed) bool {
			return c.line.Generation == "leak/handbook-rule/split-args-b64-halves/private-only"
		})).To(BeTrue(), "the split with reordered keys is in the sample")
		for _, c := range calls {
			switch c.line.Expect {
			case "caught", "caught_before_both_legs":
				Expect(c.verdict.Action).NotTo(Equal(Pass), "%s: %s", c.line.Case, where(c))
				caught[string(c.class)]++
				_, shape, _ := strings.Cut(c.line.Case, "/")
				caught[shape]++
			case "pass":
				Expect(c.verdict.Action).To(Equal(Pass), c.line.Case)
			}
		}
		for _, k := range []string{"external-low", "external-high", "split-args", "split-calls", "split-args-b64-halves", "whole-b64"} {
			Expect(caught[k]).To(BeNumerically(">", 0), "no leak of %q blocked through the model: %v", k, caught)
		}
	})
})
