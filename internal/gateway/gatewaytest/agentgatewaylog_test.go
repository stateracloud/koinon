// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/modelformat"
)

// The adapter's own check: no gateway runs. A JSON line and a logfmt line of the same request give
// the same disjoint record.
var _ = Describe("UsageFromAgentgatewayLog", func() {
	const key = "k-aaaaaaaaaaaa"
	jsonLine := `{"gen_ai.request.model":"m","koinon.key":"k-aaaaaaaaaaaa","koinon.agent":"acme/helper","koinon.request":"r1",` +
		`"koinon.in":100,"koinon.out":40,"koinon.cache_read":30,"koinon.cache_write":20,"koinon.reasoning":25,` +
		`"koinon.format":"anthropic","koinon.stream":true,"koinon.status":200,"koinon.req_bytes":10}`
	logfmt := `ts info request gen_ai.request.model=m koinon.key=k-aaaaaaaaaaaa koinon.agent=acme/helper koinon.request=r1 ` +
		`koinon.in=100 koinon.out=40 koinon.cache_read=30 koinon.cache_write=20 koinon.reasoning=25 ` +
		`koinon.format=anthropic koinon.stream=true koinon.status=200 koinon.req_bytes=10`

	DescribeTable("maps a log line to the disjoint usage record", func(line string) {
		recs, err := UsageFromAgentgatewayLog("noise\n"+line+"\n", key)
		Expect(err).NotTo(HaveOccurred())
		Expect(recs).To(HaveLen(1))
		r := recs[0]
		Expect(r.KeyID).To(Equal(key))
		Expect(r.Format).To(Equal(modelformat.Anthropic))
		Expect(r.Stream).To(BeTrue())
		Expect(r.Status).To(BeEquivalentTo(200))
		Expect([]int64{r.InputTokens, r.CacheReadTokens, r.CacheWriteTokens, r.OutputTokens, r.ReasoningTokens}).
			To(Equal([]int64{50, 30, 20, 15, 25}))
		Expect(r.InputKnown && r.OutputKnown && r.ReasoningKnown).To(BeTrue())
	},
		Entry("JSON", jsonLine),
		Entry("logfmt", logfmt),
	)

	It("reads nothing for another key", func() {
		recs, err := UsageFromAgentgatewayLog(jsonLine, "k-bbbbbbbbbbbb")
		Expect(err).NotTo(HaveOccurred())
		Expect(recs).To(BeEmpty())
	})
})
