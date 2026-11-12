// SPDX-License-Identifier: Apache-2.0

package flavors_test

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/acp"
	koinflavor "github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/testing/fakemodel"
)

// ADR 0069 decision 6: a model in the Anthropic format runs on the koinon-anthropic entry, which
// posts Messages to <gateway>/v1/messages with the agent's key as a bearer token.

// entryFor is the model value agentd selects for a format and a model name; the flavor table in
// internal/harness is its source, so the suite uses the same one.
func entryFor(format modelformat.Format, model string) string {
	GinkgoHelper()
	d, ok := koinflavor.Lookup(opencode.storeFlavor)
	Expect(ok).To(BeTrue())
	entry := d.ModelEntries[format]
	Expect(entry).NotTo(BeEmpty(), "no provider entry for the %s format", format)
	return entry + "/" + model
}

// harnessIn starts the entry script in sb with the model format, brings it to where agentd sends
// the first prompt and selects the format's entry. A zero session starts a new one; otherwise the
// session is loaded, as agentd does for a task's later runs.
func harnessIn(ctx context.Context, sb *sandbox, gw *gateway, format modelformat.Format, session acp.SessionID, extra ...string) (*harness, acp.SessionID) {
	GinkgoHelper()
	return harnessFor(ctx, sb, gw, contractModel, format, session, extra...)
}

// harnessFor is harnessIn for another model name, which is how a real Claude ID reaches OpenCode.
func harnessFor(ctx context.Context, sb *sandbox, gw *gateway, model string, format modelformat.Format, session acp.SessionID, extra ...string) (*harness, acp.SessionID) {
	GinkgoHelper()
	cmd := append(opencode.command(gw.url("/v1"), model, gw.url("/mcp")), "--model-format", string(format))
	h := sb.harness(context.WithoutCancel(ctx), workdir, append(cmd, extra...))
	DeferCleanup(h.close)
	_, err := h.conn.Initialize(ctx)
	Expect(err).NotTo(HaveOccurred())
	if session == "" {
		session, err = h.conn.NewSession(ctx, workdir)
	} else {
		err = h.conn.LoadSession(ctx, session, workdir)
	}
	Expect(err).NotTo(HaveOccurred())
	Eventually(h.client.availableCommands).WithTimeout(30 * time.Second).Should(ContainElement(opencode.readyCommand))
	time.Sleep(readySettle)
	Expect(h.conn.SetModel(ctx, session, entryFor(format, model))).To(Succeed())
	return h, session
}

// cacheBreakpoints counts the cache_control markers anywhere in a Messages request: the tools, the
// system parts and the message blocks.
func cacheBreakpoints(body []byte) int {
	var v any
	Expect(json.Unmarshal(body, &v)).To(Succeed())
	var count func(any) int
	count = func(v any) int {
		n := 0
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if k == "cache_control" {
					n++
				}
				n += count(x)
			}
		case []any:
			for _, x := range v {
				n += count(x)
			}
		}
		return n
	}
	return count(v)
}

// requestsOn returns the fake model's requests to path.
func requestsOn(gw *gateway, path string) []fakemodel.Request {
	var out []fakemodel.Request
	for _, r := range gw.model.Requests() {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

var _ = Describe("koinon-opencode with --model-format anthropic", func() {
	It("refuses a format it has no entry for, and a chat-completions cache switch with Messages", func(ctx SpecContext) {
		sb := newOpencodeSandbox(ctx, instructionsMarker+"\n")
		base := opencode.command("http://gw.test/v1", "m-1", "http://gw.test/mcp")
		for _, extra := range [][]string{
			{"--model-format", "gemini"},
			{"--model-format"},
			{"--model-format", "anthropic", "--cache-anthropic"},
		} {
			_, stderr, code := sb.try(ctx, append(slices.Clone(base), extra...)...)
			Expect(code).To(Equal(64), "%v: %s", extra, stderr)
			Expect(stderr).To(ContainSubstring("koinon-opencode:"), "%v", extra)
		}
	}, SpecTimeout(2*time.Minute))

	It("posts Messages to /v1/messages with cache_control breakpoints and an Authorization bearer token", func(ctx SpecContext) {
		gw := newGateway(contractScript(opencode))
		sb := newOpencodeSandbox(ctx, instructionsMarker+"\n")
		h, session := harnessIn(ctx, sb, gw, modelformat.Anthropic, "", "--session-id", "01234567-run")

		// A turn with a tool call, then a plain one: the second request carries the history.
		stop, err := h.conn.Prompt(ctx, session, writePrompt)
		Expect(err).NotTo(HaveOccurred())
		Expect(stop).To(Equal(acp.StopEndTurn))
		stop, err = h.conn.Prompt(ctx, session, helloPrompt)
		Expect(err).NotTo(HaveOccurred())
		Expect(stop).To(Equal(acp.StopEndTurn))
		updates, _ := h.client.take()
		Expect(acp.Text(updates, "agent_message_chunk")).To(And(ContainSubstring(wroteReply), ContainSubstring(helloReply)))

		reqs := gw.model.Requests()
		Expect(len(reqs)).To(BeNumerically(">=", 3))
		Expect(requestsOn(gw, "/v1/chat/completions")).To(BeEmpty(), "chat completions was not used")
		msgs := requestsOn(gw, "/v1/messages")
		Expect(msgs).To(HaveLen(len(reqs)), "every model request is a Messages request")
		for i, r := range msgs {
			n := cacheBreakpoints(r.Body)
			Expect(n).To(BeNumerically(">=", 1), "request %d has no cache_control breakpoint", i)
			Expect(n).To(BeNumerically("<=", 4), "request %d has more than four breakpoints", i)
			// Anthropic's 5-minute cache: no TTL is sent (Koinon prices writes at that rate).
			Expect(string(r.Body)).NotTo(ContainSubstring(`"ttl"`))
			Expect(string(r.Body)).NotTo(ContainSubstring("stream_options"))
			// The OpenRouter sticky-routing overlay goes on this entry as well.
			Expect(string(r.Body)).To(ContainSubstring(`"session_id":"01234567-run"`))
			// The token is a bearer token, and only that. OpenShell v0.1.5 rewrites its placeholder in any
			// header (crates/openshell-core/src/secrets.rs); Authorization is chosen because the gateway's
			// key policy reads it, and the Anthropic provider's default x-api-key is not used.
			Expect(r.Header.Values("Authorization")).To(Equal([]string{"Bearer " + fakeKey}))
			Expect(r.Header.Values("X-Api-Key")).To(BeEmpty())
			Expect(r.Header.Get("anthropic-version")).NotTo(BeEmpty())
		}
		// The MCP route sees the same bearer key.
		Expect(gw.authHeaders()).To(HaveEach("Bearer " + fakeKey))
		Expect(gw.anonymous()).To(BeEmpty())
	}, SpecTimeout(4*time.Minute))

	// ADR 0069: a task's session saved under one entry is loaded under the other when its model's
	// format changed (an admin override, or the OpenRouter flip). The session keeps its history; the
	// next request is in the new format.
	DescribeTable("keeps a session's history when its entry changes",
		func(ctx SpecContext, from, to modelformat.Format, fromPath, toPath, callID string) {
			script := contractScript(opencode)
			if callID != "" {
				for _, t := range script.Turns {
					for i := range t.ToolCalls {
						t.ToolCalls[i].ID = callID
					}
				}
			}
			gw := newGateway(script)
			sb := newOpencodeSandbox(ctx, instructionsMarker+"\n")

			h1, session := harnessIn(ctx, sb, gw, from, "")
			stop, err := h1.conn.Prompt(ctx, session, writePrompt)
			Expect(err).NotTo(HaveOccurred())
			Expect(stop).To(Equal(acp.StopEndTurn))
			Expect(requestsOn(gw, fromPath)).NotTo(BeEmpty())
			h1.close()
			before := len(gw.model.Requests())

			h2, loaded := harnessIn(ctx, sb, gw, to, session)
			Expect(loaded).To(Equal(session))
			updates, _ := h2.client.take()
			Expect(acp.Text(updates, "user_message_chunk")).To(ContainSubstring("KOINON-WRITE"))
			Expect(acp.Text(updates, "agent_message_chunk")).To(ContainSubstring(wroteReply))

			stop, err = h2.conn.Prompt(ctx, session, recallPrompt)
			Expect(err).NotTo(HaveOccurred())
			Expect(stop).To(Equal(acp.StopEndTurn))
			after := gw.model.Requests()[before:]
			Expect(after).NotTo(BeEmpty())
			for _, r := range after {
				Expect(r.Path).To(Equal(toPath), "the resumed session talks the new format")
			}
			// The model sees the earlier turns, tool call and result included.
			last := string(after[len(after)-1].Body)
			Expect(last).To(And(ContainSubstring("KOINON-WRITE"), ContainSubstring(wroteReply), ContainSubstring(recallPrompt), ContainSubstring("notes.txt")))
			if callID != "" && to == modelformat.Anthropic {
				// Anthropic accepts [a-zA-Z0-9_-] in a tool_use id; OpenCode rewrites the rest.
				Expect(last).To(ContainSubstring(`"id":"functions_write_0"`))
				Expect(last).NotTo(ContainSubstring("functions.write:0"))
			}
		},
		Entry("koinon to koinon-anthropic", SpecTimeout(5*time.Minute), modelformat.OpenAI, modelformat.Anthropic, "/v1/chat/completions", "/v1/messages", ""),
		Entry("koinon to koinon-anthropic with a tool-call ID outside Anthropic's pattern", SpecTimeout(5*time.Minute), modelformat.OpenAI, modelformat.Anthropic, "/v1/chat/completions", "/v1/messages", "functions.write:0"),
		Entry("koinon-anthropic to koinon", SpecTimeout(5*time.Minute), modelformat.Anthropic, modelformat.OpenAI, "/v1/messages", "/v1/chat/completions", ""),
	)
})
