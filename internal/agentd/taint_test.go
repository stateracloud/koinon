// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"encoding/json"

	sdk "github.com/coder/acp-go-sdk"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/mcpguard"
)

var _ = Describe("The leak guard's feed (ADR 0091 decision 4)", func() {
	// update is a session update on the wire, as agentd marshals it after the SDK decoded it.
	update := func(wire string) []byte {
		GinkgoHelper()
		var u sdk.SessionUpdate
		Expect(json.Unmarshal([]byte(wire), &u)).To(Succeed())
		b, err := json.Marshal(u)
		Expect(err).NotTo(HaveOccurred())
		return b
	}
	var finished func([]byte) (mcpguard.Legs, []string, bool)
	BeforeEach(func() { finished = agentd.NewTaintCalls() })

	It("reports a repository read when it finishes, with its output, as private data", func() {
		_, _, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t1","title":"read","kind":"read","status":"pending"}`))
		Expect(ok).To(BeFalse())
		_, _, ok = finished(update(`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"in_progress",
			"content":[{"type":"content","content":{"type":"text","text":"customer list: koinon-test-acme-row-1"}}]}`))
		Expect(ok).To(BeFalse(), "not finished yet")
		legs, texts, ok := finished(update(`{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}`))
		Expect(ok).To(BeTrue())
		Expect(legs).To(Equal(mcpguard.Legs{PrivateData: true}))
		Expect(texts).To(ConsistOf("customer list: koinon-test-acme-row-1"))
	})

	It("reports shell output from the raw output too, and a failed command as well", func() {
		legs, texts, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t2","title":"bash","kind":"execute","status":"failed",
			"rawOutput":{"output":"cat: secrets.env line 1","metadata":{"exit":1}}}`))
		Expect(ok).To(BeTrue())
		Expect(legs.PrivateData).To(BeTrue())
		Expect(texts).To(ContainElement("cat: secrets.env line 1"))
	})

	It("reports both sides of an edit's diff", func() {
		_, texts, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t3","title":"edit","kind":"edit","status":"completed",
			"content":[{"type":"diff","path":"/sandbox/work/a.go","oldText":"old private line","newText":"new private line"}]}`))
		Expect(ok).To(BeTrue())
		Expect(texts).To(ConsistOf("old private line", "new private line"))
	})

	It("reports a fetch as untrusted input", func() {
		legs, _, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t4","title":"webfetch","kind":"fetch","status":"completed",
			"content":[{"type":"content","content":{"type":"text","text":"a page someone wrote"}}]}`))
		Expect(ok).To(BeTrue())
		Expect(legs).To(Equal(mcpguard.Legs{UntrustedInput: true}))
	})

	It("leaves MCP tools to the guard and ignores other updates", func() {
		_, _, ok := finished(update(`{"sessionUpdate":"tool_call","toolCallId":"t5","title":"mcp__acme-wiki_read_page","kind":"other","status":"completed",
			"content":[{"type":"content","content":{"type":"text","text":"x"}}]}`))
		Expect(ok).To(BeFalse())
		_, _, ok = finished(update(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hi"}}`))
		Expect(ok).To(BeFalse())
	})
})
