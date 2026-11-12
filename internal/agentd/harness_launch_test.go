// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/agentd"
	"github.com/stateracloud/koinon/internal/harness"
)

var _ = Describe("LaunchCommand", func() {
	e := agentd.Endpoints{ModelBaseURL: "http://gw.test/v1", Model: "m-1", MCPURL: "http://gw.test/mcp", Retry: agentd.DefaultRetryPolicy()}

	DescribeTable("is the harness's entry script, by absolute path, with the endpoints as arguments",
		func(f string, script string) {
			cmd, err := agentd.LaunchCommand(f, e)
			Expect(err).NotTo(HaveOccurred())
			Expect(cmd).To(Equal([]string{
				script, "--model-base-url", "http://gw.test/v1", "--model", "m-1", "--mcp-url", "http://gw.test/mcp",
				"--model-tries", "3", "--model-retry-backoff-ms", "1000", "--model-max-retry-after-ms", "60000",
			}))
		},
		Entry("opencode", harness.OpenCode, "/usr/local/bin/koinon-opencode"),
	)

	// One retrying layer (ADR 0056 amendment 2026-10-31, point 3): a harness that retries model
	// requests is handed Koinon's policy, and is never launched without one.
	It("hands a retrying harness Koinon's retry policy, and refuses to launch it without one", func() {
		d, ok := harness.Lookup(harness.OpenCode)
		Expect(ok).To(BeTrue())
		Expect(d.RetriesModelRequests).To(BeTrue(), "OpenCode 2.2.0 retries model requests itself")

		p := e
		p.Retry = agentd.RetryPolicy{Tries: 1, Backoff: 250 * time.Millisecond, MaxRetryAfter: 5 * time.Second}
		cmd, err := agentd.LaunchCommand(harness.OpenCode, p)
		Expect(err).NotTo(HaveOccurred())
		Expect(cmd).To(ContainElements("--model-tries", "1", "--model-retry-backoff-ms", "250", "--model-max-retry-after-ms", "5000"))

		p.Retry = agentd.RetryPolicy{}
		_, err = agentd.LaunchCommand(harness.OpenCode, p)
		Expect(err).To(MatchError(ContainSubstring("needs Koinon's retry policy")))
	})

	It("defaults to 3 tries, 1s backoff and a 60s Retry-After cap", func() {
		Expect(agentd.DefaultRetryPolicy()).To(Equal(agentd.RetryPolicy{Tries: 3, Backoff: time.Second, MaxRetryAfter: time.Minute}))
	})

	It("covers every harness Koinon ships", func() {
		Expect(harness.Names()).NotTo(BeEmpty())
		for _, name := range harness.Names() {
			_, err := agentd.LaunchCommand(name, e)
			Expect(err).NotTo(HaveOccurred(), name)
		}
	})

	It("rejects an unknown harness", func() {
		_, err := agentd.LaunchCommand("claude", e)
		Expect(err).To(MatchError(ContainSubstring(`harness "claude" has no ACP launch command`)))
	})
})
