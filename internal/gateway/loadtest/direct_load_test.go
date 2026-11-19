// SPDX-License-Identifier: Apache-2.0

//go:build load

package loadtest_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/gateway/loadtest"
)

// The reference of the MCP path: the same calls to the fake MCP server with no gateway between, so a
// gateway's overhead is its numbers minus these (issue 280).
var _ = Describe("the fake MCP server, called directly", Label("load"), func() {
	It("serves the reference load", func(ctx SpecContext) {
		fake, err := loadtest.StartFakeMCP(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(fake.Stop)
		ep := loadtest.Endpoint{
			Driver: "direct (no gateway)", URL: fmt.Sprintf("http://127.0.0.1:%d/mcp", fake.Port),
			Headers: map[string]string{"X-Api-Key": loadtest.FakeMCPToken},
			Tool:    "read_item", StreamTool: "stream_progress",
		}
		var results []loadtest.Result
		for _, conc := range loadtest.Levels() {
			res, err := ep.ToolCalls(ctx, conc, 2*time.Second, loadtest.Duration())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Errors).To(BeZero(), "%v", res.ErrorKinds)
			results = append(results, res)
		}
		res, err := ep.Streams(ctx, 64, 20, 500*time.Millisecond, loadtest.Duration()+10*time.Second)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Notes["progressReceived"]).To(Equal(res.Notes["progressExpected"]), "the fake server sends every notification")
		results = append(results, res)
		Expect(loadtest.Save(results)).To(Succeed())
	}, SpecTimeout(30*time.Minute))
})
