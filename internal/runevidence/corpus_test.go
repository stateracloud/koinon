// SPDX-License-Identifier: Apache-2.0

package runevidence_test

import (
	"bytes"
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/runevidence"
)

func jl(lines ...string) *strings.Reader { return strings.NewReader(strings.Join(lines, "\n") + "\n") }

func parse(b []byte) []map[string]any {
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		ExpectWithOffset(1, json.Unmarshal([]byte(l), &m)).To(Succeed())
		out = append(out, m)
	}
	return out
}

var _ = Describe("Corpus", func() {
	events := jl(
		`{"type":"run_event","run_id":"r1","seq":1,"kind":"acp.tool_call","payload":{"sessionUpdate":"tool_call","toolCallId":"t1","kind":"other","title":"koinon_mail_send","rawInput":{"to":"nobody@invalid"}}}`,
		`{"type":"run_event","run_id":"r1","seq":2,"kind":"acp.tool_call_update","payload":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","rawOutput":"sent"}}`,
		`{"type":"run_event","run_id":"r1","seq":3,"kind":"acp.tool_call","payload":{"sessionUpdate":"tool_call","toolCallId":"t2","kind":"read","title":"read file"}}`,
		`{"type":"run_event","run_id":"r1","seq":4,"kind":"acp.tool_call_update","payload":{"sessionUpdate":"tool_call_update","toolCallId":"t2","status":"completed","content":[{"content":{"text":"private text"}}]}}`,
		`{"type":"run_event","run_id":"r1","seq":5,"kind":"acp.tool_call","payload":{"sessionUpdate":"tool_call","toolCallId":"t3","kind":"other","title":"koinon_mail_unknown"}}`,
	)
	decisions := jl(
		`{"type":"decision","run_id":"r1","generation":"g1","server":"mail","tool":"send","sink_class":"external-low"}`,
		`{"type":"decision","run_id":"r1","generation":"g1","server":"mail","tool":"never_called","sink_class":"external-low"}`,
	)

	It("pairs MCP calls with their decisions and turns other tools into feeds", func() {
		var out bytes.Buffer
		rep, err := runevidence.Corpus(events, decisions, &out, runevidence.CorpusOptions{MCPToolPrefix: "koinon_"})
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Calls).To(Equal(1))
		Expect(rep.Feeds).To(Equal(1))
		Expect(rep.UnpairedCalls).To(Equal(1))
		Expect(rep.UnpairedDecisions).To(Equal(1))
		Expect(rep.UndeclaredServers).To(Equal([]string{"mail"}))

		lines := parse(out.Bytes())
		var call, feed map[string]any
		for _, l := range lines {
			switch l["type"] {
			case "call":
				call = l
			case "feed":
				feed = l
			}
		}
		Expect(call).To(HaveKeyWithValue("server", "mail"))
		Expect(call).To(HaveKeyWithValue("tool", "send"))
		Expect(call).To(HaveKeyWithValue("generation", "g1"))
		Expect(feed["legs"]).To(Equal(map[string]any{"untrusted_input": false, "private_data": true}))
		Expect(feed["texts"]).To(Equal([]any{"private text"}))
	})

	It("copies declarations in front and does not stub a declared server", func() {
		var out bytes.Buffer
		rep, err := runevidence.Corpus(events, decisions, &out, runevidence.CorpusOptions{
			MCPToolPrefix: "koinon_",
			Declarations:  []json.RawMessage{json.RawMessage(`{"type":"server","server":"mail","kind":"send"}`)},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.UndeclaredServers).To(BeEmpty())
		Expect(parse(out.Bytes())[0]).To(HaveKeyWithValue("kind", "send"))
	})

	It("pairs an ask-tier call on its -ask name and splits a tool name with underscores", func() {
		ev := jl(
			`{"type":"run_event","run_id":"r1","seq":1,"kind":"acp.tool_call","payload":{"sessionUpdate":"tool_call","toolCallId":"t1","kind":"other","title":"koinon_gh-ask_issue_create","rawInput":{"title":"x"}}}`,
			`{"type":"run_event","run_id":"r1","seq":2,"kind":"acp.tool_call_update","payload":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}}`,
		)
		dec := jl(`{"type":"decision","run_id":"r1","generation":"g1","server":"gh","tool":"issue_create","sink_class":"external-high"}`)
		var out bytes.Buffer
		rep, err := runevidence.Corpus(ev, dec, &out, runevidence.CorpusOptions{MCPToolPrefix: "koinon_"})
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Calls).To(Equal(1))
		Expect(rep.UnpairedCalls).To(BeZero())
		Expect(rep.UnpairedDecisions).To(BeZero())
	})

	It("writes a finished call of an own tool as a feed that sets both legs", func() {
		ev := jl(
			`{"type":"run_event","run_id":"r1","seq":1,"kind":"acp.tool_call","payload":{"sessionUpdate":"tool_call","toolCallId":"t1","kind":"other","title":"koinon_ticket_get"}}`,
			`{"type":"run_event","run_id":"r1","seq":2,"kind":"acp.tool_call_update","payload":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}}`,
		)
		var out bytes.Buffer
		rep, err := runevidence.Corpus(ev, jl(), &out, runevidence.CorpusOptions{MCPToolPrefix: "koinon_"})
		Expect(err).NotTo(HaveOccurred())
		Expect(rep.OwnCalls).To(Equal(1))
		Expect(rep.UnpairedCalls).To(BeZero())
		Expect(parse(out.Bytes())[0]["legs"]).To(Equal(map[string]any{"untrusted_input": true, "private_data": true}))
	})
})
