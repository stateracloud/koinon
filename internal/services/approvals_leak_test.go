// SPDX-License-Identifier: Apache-2.0

package services

import (
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
