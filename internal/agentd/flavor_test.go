// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/procoduck/koinon/internal/agentd"
	"github.com/procoduck/koinon/internal/db/store"
)

var _ = Describe("LaunchCommand", func() {
	e := agentd.Endpoints{ModelBaseURL: "http://gw.test/v1", Model: "m-1", MCPURL: "http://gw.test/mcp"}

	DescribeTable("is the flavor's entry script, by absolute path, with the endpoints as arguments",
		func(f store.AgentFlavor, script string) {
			cmd, err := agentd.LaunchCommand(f, e)
			Expect(err).NotTo(HaveOccurred())
			Expect(cmd).To(Equal([]string{
				script, "--model-base-url", "http://gw.test/v1", "--model", "m-1", "--mcp-url", "http://gw.test/mcp",
			}))
		},
		Entry("opencode", store.AgentFlavorOpencode, "/usr/local/bin/koinon-opencode"),
		Entry("hermes", store.AgentFlavorHermes, "/usr/local/bin/koinon-hermes"),
		Entry("pi", store.AgentFlavorPi, "/usr/local/bin/koinon-pi"),
	)

	It("covers every flavor of the schema's agent_flavor enum", func(ctx SpecContext) {
		rows, err := db.Query(ctx, `SELECT unnest(enum_range(NULL::agent_flavor))::text`)
		Expect(err).NotTo(HaveOccurred())
		flavors, err := pgx.CollectRows(rows, pgx.RowTo[string])
		Expect(err).NotTo(HaveOccurred())
		Expect(flavors).NotTo(BeEmpty())
		for _, f := range flavors {
			_, err := agentd.LaunchCommand(store.AgentFlavor(f), e)
			Expect(err).NotTo(HaveOccurred(), f)
		}
	})

	It("rejects an unknown flavor", func() {
		_, err := agentd.LaunchCommand("claude", e)
		Expect(err).To(MatchError(ContainSubstring(`flavor "claude" has no ACP launch command`)))
	})
})
