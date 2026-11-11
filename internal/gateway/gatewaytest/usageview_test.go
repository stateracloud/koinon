// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/modelformat"
)

// The expectations of the failed-response cases (#550) checked without a gateway: a hand-built
// record of what a gateway that keeps the real usage record writes for each case goes through
// viewOf and statedBy, as the suite does. A wrong number in a case fails here, not only on a driver.
var _ = Describe("the usage view of a record", func() {
	failed := func(f modelformat.Format, in, out int64, status int64) gateway.UsageRecord {
		return gateway.UsageRecord{
			Format: f, Stream: true, Status: status, Error: "Upstream",
			InputTokens: in, OutputTokens: out, InputKnown: in > 0, OutputKnown: out > 0,
		}
	}

	DescribeTable("matches the case's want",
		func(want wantUsage, rec gateway.UsageRecord) {
			Expect(viewOf(rec)).To(Equal(want.statedBy(rec)))
		},
		Entry("chat completions, error event mid-way",
			wantUsage{Format: modelformat.OpenAI, Stream: true, Status: 200, Failed: true},
			failed(modelformat.OpenAI, 0, 0, 200)),
		Entry("Messages, error event mid-way (message_start usage only)",
			wantUsage{Format: modelformat.Anthropic, Stream: true, Status: 200, Failed: true, Input: 100, Output: 1},
			failed(modelformat.Anthropic, 100, 1, 200)),
		Entry("chat completions, client cancel (status is the gateway's)",
			wantUsage{Format: modelformat.OpenAI, Stream: true, Failed: true},
			failed(modelformat.OpenAI, 0, 0, 499)),
		Entry("Messages, client cancel (status is the gateway's)",
			wantUsage{Format: modelformat.Anthropic, Stream: true, Failed: true, Input: 100, Output: 1},
			failed(modelformat.Anthropic, 100, 1, 0)),
		Entry("Messages thinking folds back into Output",
			wantUsage{Format: modelformat.Anthropic, Status: 200, Input: 50, CacheRead: 30, CacheWrite: 20, Output: 40},
			gateway.UsageRecord{
				Format: modelformat.Anthropic, Status: 200, InputTokens: 50, CacheReadTokens: 30, CacheWriteTokens: 20,
				OutputTokens: 15, ReasoningTokens: 25, ReasoningKnown: true,
			}),
	)

	It("tells a failed record from one the gateway left unmarked", func() {
		rec := failed(modelformat.OpenAI, 0, 0, 200)
		rec.Error = ""
		want := wantUsage{Format: modelformat.OpenAI, Stream: true, Status: 200, Failed: true}
		Expect(viewOf(rec)).NotTo(Equal(want.statedBy(rec)))
	})
})
