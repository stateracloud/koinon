// SPDX-License-Identifier: Apache-2.0

package mcpservers_test

import (
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("agent allowlists", func() {
	var (
		svc   *mcpservers.Service
		org   pgtype.UUID
		user  pgtype.UUID
		agent store.Agent
		actor audit.Actor
	)
	BeforeEach(func(ctx SpecContext) {
		svc = mcpservers.New(pool, ring, nil).WithURLPolicy(catalog.URLPolicy{Namespace: "koinon"})
		org, user, _ = pgtest.NewOrg(ctx, pool)
		actor = audit.User(user)
		var err error
		agent, err = store.New(pool).CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "a", Harness: harness.OpenCode, CreatedBy: user})
		Expect(err).NotTo(HaveOccurred())
	})
	add := func(ctx SpecContext, org, user pgtype.UUID, name string) mcpservers.Server {
		GinkgoHelper()
		s, err := svc.Create(ctx, org, audit.User(user), user, mcpservers.Input{Name: name, URL: "https://x.example.com/" + name})
		Expect(err).NotTo(HaveOccurred())
		return s
	}

	It("replaces the allowlist, narrows to tools, and audits the change", func(ctx SpecContext) {
		a, b := add(ctx, org, user, "wiki"), add(ctx, org, user, "tracker")
		got, err := svc.SetAgentServers(ctx, org, actor, agent.ID, []mcpservers.Allowance{{ServerID: a.ID}, {ServerID: b.ID, Tools: []string{"search", "read", "read"}}})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(ConsistOf(mcpservers.Allowance{ServerID: a.ID}, mcpservers.Allowance{ServerID: b.ID, Tools: []string{"read", "search"}}))

		got, err = svc.SetAgentServers(ctx, org, actor, agent.ID, []mcpservers.Allowance{{ServerID: b.ID}})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal([]mcpservers.Allowance{{ServerID: b.ID}}))

		rows, err := store.New(pool).ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: org, PageSize: 100})
		Expect(err).NotTo(HaveOccurred())
		n := 0
		for _, r := range rows {
			if r.Action == "agent.mcp_servers" {
				n++
			}
		}
		Expect(n).To(Equal(2))
	})

	It("removes an allowance when its server is deleted", func(ctx SpecContext) {
		a := add(ctx, org, user, "wiki")
		_, err := svc.SetAgentServers(ctx, org, actor, agent.ID, []mcpservers.Allowance{{ServerID: a.ID}})
		Expect(err).NotTo(HaveOccurred())
		Expect(svc.Delete(ctx, org, actor, a.ID)).To(Succeed())
		got, err := svc.AgentServers(ctx, org, agent.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})

	It("refuses another org's server, a duplicate, an empty narrowing and another org's agent", func(ctx SpecContext) {
		other, otherUser, _ := pgtest.NewOrg(ctx, pool)
		theirs := add(ctx, other, otherUser, "theirs")
		mine := add(ctx, org, user, "mine")
		_, err := svc.SetAgentServers(ctx, org, actor, agent.ID, []mcpservers.Allowance{{ServerID: theirs.ID}})
		Expect(err).To(BeAssignableToTypeOf(&mcpservers.InvalidError{}))
		_, err = svc.SetAgentServers(ctx, org, actor, agent.ID, []mcpservers.Allowance{{ServerID: mine.ID}, {ServerID: mine.ID}})
		Expect(err).To(MatchError(ContainSubstring("twice")))
		_, err = svc.SetAgentServers(ctx, org, actor, agent.ID, []mcpservers.Allowance{{ServerID: mine.ID, Tools: []string{}}})
		Expect(err).To(MatchError(ContainSubstring("no tools")))
		_, err = svc.SetAgentServers(ctx, other, audit.User(otherUser), agent.ID, nil)
		Expect(err).To(MatchError(mcpservers.ErrNotFound))
	})
})
