// SPDX-License-Identifier: Apache-2.0

package fakemodel_test

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

const exampleScripts = "../../../test/fakemodel/scripts"

var _ = Describe("Scripts", func() {
	It("parses YAML with faults and tool calls", func() {
		s, err := fakemodel.ParseScript([]byte(`
model: m
faults:
  unavailable: 2
  delay: 1500ms
  truncate: true
turns:
  - toolCalls:
      - name: t
        arguments: {a: 1, b: [x, y]}
  - when: {toolResult: "*"}
    text: done
    usage: {inputTokens: 10, cachedInputTokens: 4, outputTokens: 2}
`))
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Faults).To(Equal(fakemodel.Faults{Unavailable: 2, Delay: 1500 * time.Millisecond, Truncate: true}))
		Expect(s.Turns).To(HaveLen(2))
		Expect(s.Turns[0].ToolCalls[0].Arguments).To(HaveKeyWithValue("a", 1))
		Expect(s.Turns[1].When.ToolResult).To(Equal(fakemodel.AnyTool))
		Expect(s.Turns[1].Usage.CachedInputTokens).To(Equal(4))
	})

	DescribeTable("rejects invalid scripts",
		func(doc, msg string) {
			_, err := fakemodel.ParseScript([]byte(doc))
			Expect(err).To(MatchError(ContainSubstring(msg)))
		},
		Entry("unknown field", "model: m\nturns: [{text: a, txet: b}]", "txet"),
		Entry("no model", "turns: [{text: a}]", "model is required"),
		Entry("no turns", "model: m", "at least one turn"),
		Entry("empty turn", "model: m\nturns: [{}]", "needs text or toolCalls"),
		Entry("unnamed tool", "model: m\nturns: [{toolCalls: [{arguments: {}}]}]", "name is required"),
		Entry("cache above input", "model: m\nturns: [{text: a, usage: {inputTokens: 1, cachedInputTokens: 2}}]", "invalid usage"),
		Entry("cache reads and writes above input", "model: m\nturns: [{text: a, usage: {inputTokens: 3, cachedInputTokens: 2, cacheWriteTokens: 2}}]", "invalid usage"),
	)

	It("rejects the same model in two files", func() {
		dir := GinkgoT().TempDir()
		for _, f := range []string{"a.yaml", "b.yml"} {
			Expect(os.WriteFile(filepath.Join(dir, f), []byte("model: m\nturns: [{text: a}]\n"), 0o600)).To(Succeed())
		}
		Expect(os.WriteFile(filepath.Join(dir, "README.md"), []byte("ignored"), 0o600)).To(Succeed())
		_, err := fakemodel.LoadDir(dir)
		Expect(err).To(MatchError(ContainSubstring("already defined")))
	})

	Describe("turn selection", func() {
		It("distinguishes repeated steps with userContains and assistantTurns", func(ctx SpecContext) {
			_, ts := start(fakemodel.Script{Model: "steps", Turns: []fakemodel.Turn{
				{When: fakemodel.When{UserContains: "bye"}, Text: "goodbye"},
				{When: fakemodel.When{AssistantTurns: intPtr(0)}, Text: "first"},
				{Text: "later"},
			}})
			ask := func(messages ...map[string]any) string {
				var c oaCompletion
				decode(post(ctx, ts.URL+"/v1/chat/completions", map[string]any{"model": "steps", "messages": messages}), &c)
				return *c.Choices[0].Message.Content
			}
			Expect(ask(userMsg("hello"))).To(Equal("first"))
			Expect(ask(userMsg("hello"), map[string]any{"role": "assistant", "content": "first"}, userMsg("again"))).To(Equal("later"))
			Expect(ask(userMsg("bye now"))).To(Equal("goodbye"))
		})

		It("matches the system prompt with systemContains", func(ctx SpecContext) {
			_, ts := start(fakemodel.Script{Model: "sys", Turns: []fakemodel.Turn{
				{When: fakemodel.When{SystemContains: "MARKER"}, Text: "instructed"},
				{Text: "plain"},
			}})
			openAI := func(messages ...map[string]any) string {
				var c oaCompletion
				decode(post(ctx, ts.URL+"/v1/chat/completions", map[string]any{"model": "sys", "messages": messages}), &c)
				return *c.Choices[0].Message.Content
			}
			anthropic := func(system any) string {
				body := map[string]any{"model": "sys", "max_tokens": 10, "messages": []any{anUser("hi")}}
				if system != nil {
					body["system"] = system
				}
				var m anMessage
				decode(post(ctx, ts.URL+"/v1/messages", body, "anthropic-version", "2023-06-01"), &m)
				return m.Content[0].Text
			}
			sys := func(role string, content any) map[string]any { return map[string]any{"role": role, "content": content} }

			Expect(openAI(sys("system", "you follow the MARKER rules"), userMsg("hi"))).To(Equal("instructed"))
			Expect(openAI(sys("developer", []map[string]any{{"type": "text", "text": "MARKER"}}), userMsg("hi"))).To(Equal("instructed"))
			Expect(openAI(sys("system", "no rules"), userMsg("hi"))).To(Equal("plain"))
			Expect(openAI(userMsg("MARKER in the user text is not the system prompt"))).To(Equal("plain"))

			Expect(anthropic("the MARKER rules")).To(Equal("instructed"))
			Expect(anthropic([]map[string]any{{"type": "text", "text": "a"}, {"type": "text", "text": "MARKER"}})).To(Equal("instructed"))
			Expect(anthropic(nil)).To(Equal("plain"))
		})

		It("parses systemContains from YAML", func() {
			s, err := fakemodel.ParseScript([]byte("model: m\nturns: [{when: {systemContains: x}, text: a}]"))
			Expect(err).NotTo(HaveOccurred())
			Expect(s.Turns[0].When.SystemContains).To(Equal("x"))
		})
	})

	Describe("the example repo-helper script", func() {
		It("loads", func() {
			scripts, err := fakemodel.LoadDir(exampleScripts)
			Expect(err).NotTo(HaveOccurred())
			Expect(scripts).To(ContainElement(HaveField("Model", "repo-helper")))
		})

		It("calls create_pull_request, then answers after the tool result", func(ctx SpecContext) {
			scripts, err := fakemodel.LoadDir(exampleScripts)
			Expect(err).NotTo(HaveOccurred())
			_, ts := start(scripts...)
			url := ts.URL + "/v1/chat/completions"
			// The instructions koinon-agentd writes, as OpenCode puts them in the system prompt.
			system := map[string]any{"role": "system", "content": "Instructions from: /sandbox/.koinon/AGENTS.md\n# Koinon\n\n# Repo helper\n"}
			messages := []map[string]any{system, userMsg("Add hello.md to koinon-demo/hello")}

			// OpenCode declares the MCP tool behind its server prefix.
			tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "koinon_create_pull_request"}}}
			var first oaCompletion
			decode(post(ctx, url, map[string]any{"model": "repo-helper", "messages": messages, "tools": tools}), &first)
			Expect(first.Choices[0].FinishReason).To(Equal("tool_calls"))
			call := first.Choices[0].Message.ToolCalls[0]
			Expect(call.Function.Name).To(Equal("koinon_create_pull_request"))
			Expect(call.Function.Arguments).To(MatchJSON(`{
				"branch": "koinon/add-hello-md",
				"title": "Add hello.md",
				"body": "Adds hello.md as requested in the task.",
				"files": [{"path": "hello.md", "content": "hello from koinon\n"}]
			}`))

			messages = append(messages,
				map[string]any{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
					"id": call.ID, "type": "function",
					"function": map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments},
				}}},
				map[string]any{"role": "tool", "tool_call_id": call.ID, "content": `{"approval_request":"15d955b99082815a"}`},
			)
			var second oaCompletion
			decode(post(ctx, url, map[string]any{"model": "repo-helper", "messages": messages, "tools": tools}), &second)
			Expect(second.Choices[0].FinishReason).To(Equal("stop"))
			Expect(*second.Choices[0].Message.Content).To(ContainSubstring("approval"))
			Expect(second.Usage.PromptTokensDetails.CachedTokens).To(Equal(1200))

			By("the resumed run reports the pull request agentd's prompt names")
			messages = append(messages,
				map[string]any{"role": "assistant", "content": *second.Choices[0].Message.Content},
				userMsg("[Koinon] Your run was paused ... The outcomes:\n- create_pull_request (approval id=x, github:koinon-demo/hello): "+
					"approved and executed by Koinon: https://github.test/koinon-demo/hello/pull/1"),
			)
			var third oaCompletion
			decode(post(ctx, url, map[string]any{"model": "repo-helper", "messages": messages, "tools": tools}), &third)
			Expect(third.Choices[0].FinishReason).To(Equal("stop"))
			Expect(*third.Choices[0].Message.Content).To(ContainSubstring("https://github.test/koinon-demo/hello/pull/1"))

			By("a request without the repo-helper instructions gets no answer")
			resp := post(ctx, url, map[string]any{"model": "repo-helper", "messages": messages[1:], "tools": tools})
			DeferCleanup(resp.Body.Close)
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		})

		It("asks one question with ask_user, then proposes the pull request once the answer arrives (repo-helper-ask)", func(ctx SpecContext) {
			scripts, err := fakemodel.LoadDir(exampleScripts)
			Expect(err).NotTo(HaveOccurred())
			_, ts := start(scripts...)
			url := ts.URL + "/v1/chat/completions"
			system := map[string]any{"role": "system", "content": "# Koinon\n\n# Repo helper\n"}
			tools := []map[string]any{
				{"type": "function", "function": map[string]any{"name": "koinon_ask_user"}},
				{"type": "function", "function": map[string]any{"name": "koinon_create_pull_request"}},
			}
			step := func(messages ...map[string]any) oaCompletion {
				var c oaCompletion
				decode(post(ctx, url, map[string]any{"model": "repo-helper-ask", "messages": append([]map[string]any{system}, messages...), "tools": tools}), &c)
				return c
			}
			task := userMsg("Add hello.md, ask first")
			first := step(task)
			call := first.Choices[0].Message.ToolCalls[0]
			Expect(call.Function.Name).To(Equal("koinon_ask_user"))
			Expect(call.Function.Arguments).To(MatchJSON(`{"question":"Add hello.md to the repository?","choices":["add it","do not add it"],"default":"do not add it"}`))

			asked := []map[string]any{
				task,
				{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
					"id": call.ID, "type": "function",
					"function": map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments},
				}}},
				{"role": "tool", "tool_call_id": call.ID, "content": `{"status":"open"}`},
			}
			Expect(step(asked...).Choices[0].FinishReason).To(Equal("stop"))

			answered := append(asked, map[string]any{"role": "assistant", "content": "I asked."},
				userMsg("[Koinon] Your run was paused ... the person answered: \"add it\""))
			pr := step(answered...)
			Expect(pr.Choices[0].Message.ToolCalls[0].Function.Name).To(Equal("koinon_create_pull_request"))
		})

		It("keeps the scripted name when no declared tool or several match it", func(ctx SpecContext) {
			_, ts := start(fakemodel.Script{Model: "m", Turns: []fakemodel.Turn{
				{ToolCalls: []fakemodel.ToolCall{{Name: "ping"}}},
			}})
			call := func(names ...string) string {
				var tools []map[string]any
				for _, n := range names {
					tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": n}})
				}
				var c oaCompletion
				decode(post(ctx, ts.URL+"/v1/chat/completions", map[string]any{"model": "m", "messages": []map[string]any{userMsg("hi")}, "tools": tools}), &c)
				return c.Choices[0].Message.ToolCalls[0].Function.Name
			}
			Expect(call()).To(Equal("ping"))
			Expect(call("ping", "koinon_ping")).To(Equal("ping"))
			Expect(call("mcp__koinon__ping")).To(Equal("mcp__koinon__ping"))
			Expect(call("koinon_ping", "mcp__koinon__ping")).To(Equal("ping"))
			Expect(call("a_ping", "b_ping")).To(Equal("ping"))
			Expect(call("foo_ping")).To(Equal("ping"), "another server's tool of the same name")
			Expect(call("mcp__other__ping")).To(Equal("ping"))
			Expect(call("pinger")).To(Equal("ping"))
		})

		It("matches a tool result only from the scripted tool, behind a harness's MCP prefix", func(ctx SpecContext) {
			_, ts := start(fakemodel.Script{Model: "m", Turns: []fakemodel.Turn{
				{When: fakemodel.When{ToolResult: "create_pull_request"}, Text: "pull request result"},
				{When: fakemodel.When{ToolResult: fakemodel.AnyTool}, Text: "another result"},
				{Text: "first"},
			}})
			after := func(tool string) string {
				messages := []map[string]any{
					userMsg("hi"),
					{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
						"id": "call_1", "type": "function", "function": map[string]any{"name": tool, "arguments": "{}"},
					}}},
					{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
				}
				var c oaCompletion
				decode(post(ctx, ts.URL+"/v1/chat/completions", map[string]any{"model": "m", "messages": messages}), &c)
				return *c.Choices[0].Message.Content
			}
			Expect(after("create_pull_request")).To(Equal("pull request result"))
			Expect(after("koinon_create_pull_request")).To(Equal("pull request result"))
			Expect(after("mcp__koinon__create_pull_request")).To(Equal("pull request result"))
			Expect(after("foo_create_pull_request")).To(Equal("another result"))
			Expect(after("mcp__other__create_pull_request")).To(Equal("another result"))
		})
	})
})
