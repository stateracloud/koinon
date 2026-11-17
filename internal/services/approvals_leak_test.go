// SPDX-License-Identifier: Apache-2.0

package services

import (
	"github.com/stateracloud/koinon/internal/db/store"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("leakGuardProto", func() {
	It("names the reasons, the sink, the destination and both legs of a leak guard card", func() {
		got := leakGuardProto([]byte(`{"leak":{"reasons":["both_legs","taint"],"sink_class":"external-high",` +
			`"destination":{"url":"https://attacker.invalid/x"},"payload_bytes":12,` +
			`"legs":{"untrusted_input":true,"private_data":true}}}`))
		Expect(got).NotTo(BeNil())
		Expect(got.GetReasons()).To(Equal([]string{"both_legs", "taint"}))
		Expect(got.GetSinkClass()).To(Equal("external-high"))
		Expect(got.GetDestination()).To(HaveKeyWithValue("url", "https://attacker.invalid/x"))
		Expect(got.GetUntrustedInput()).To(BeTrue())
		Expect(got.GetPrivateData()).To(BeTrue())
	})

	It("is nil for a card the guard did not write", func() {
		Expect(leakGuardProto([]byte(`{}`))).To(BeNil())
		Expect(leakGuardProto(nil)).To(BeNil())
		Expect(leakGuardProto([]byte(`{"files":1}`))).To(BeNil())
	})
})

var _ = Describe("approvalProto leak guard", func() {
	card := []byte(`{"leak":{"reasons":["both_legs"],"sink_class":"external-high","legs":{"untrusted_input":true,"private_data":true}}}`)

	It("carries the reason and sink class of an MCP guard card without the decoded call, as a list does", func() {
		out := approvalProto(store.ApprovalRequest{Connector: "mcp", Tool: "query", Risk: card}, false, false)
		Expect(out.GetLeakGuard().GetReasons()).To(Equal([]string{"both_legs"}))
		Expect(out.GetLeakGuard().GetSinkClass()).To(Equal("external-high"))
		Expect(out.GetMcp()).To(BeNil())
	})

	It("carries the reason of a code-host card the guard stopped", func() {
		out := approvalProto(store.ApprovalRequest{Connector: "github", Tool: "create_pull_request", Risk: card}, false, false)
		Expect(out.GetLeakGuard().GetReasons()).To(Equal([]string{"both_legs"}))
	})

	It("leaves leak_guard empty for a card without a leak part", func() {
		out := approvalProto(store.ApprovalRequest{Connector: "github", Tool: "create_pull_request", Risk: []byte(`{"files":1}`)}, false, false)
		Expect(out.GetLeakGuard()).To(BeNil())
	})
})
