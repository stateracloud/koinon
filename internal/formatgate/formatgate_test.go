// SPDX-License-Identifier: Apache-2.0

package formatgate_test

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/formatgate"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the format gate", func() {
	var (
		pool *pgxpool.Pool
		gate formatgate.Gate
	)
	BeforeEach(func(ctx SpecContext) {
		pool = pgtest.Start(ctx)
		gate = formatgate.Gate{DB: pool, Expect: map[formatgate.Component]int{
			formatgate.API: 2, formatgate.Reconciler: 1, formatgate.Gateway: 2,
		}}
	})
	report := func(ctx SpecContext, c formatgate.Component, instance string, reads ...string) {
		GinkgoHelper()
		r := formatgate.Reporter{DB: pool, Component: c, Instance: instance, Reads: reads}
		Expect(r.Report(ctx)).To(Succeed())
	}
	v2 := formatgate.MCPCredentialV2.Name
	allAtN := func(ctx SpecContext) {
		GinkgoHelper()
		report(ctx, formatgate.API, "api-0", v2)
		report(ctx, formatgate.API, "api-1", v2)
		report(ctx, formatgate.Reconciler, "rec-0", v2)
		report(ctx, formatgate.Gateway, "gw-0", v2)
		report(ctx, formatgate.Gateway, "gw-1", v2)
	}

	It("opens when every expected pod of every reader lists the format", func(ctx SpecContext) {
		allAtN(ctx)
		Expect(gate.Ready(ctx, formatgate.MCPCredentialV2)).To(Succeed())
	})

	It("stays shut when one pod is at N-1 (it lists no format)", func(ctx SpecContext) {
		allAtN(ctx)
		report(ctx, formatgate.Gateway, "gw-1") // a pod that reports but does not read v2
		err := gate.Ready(ctx, formatgate.MCPCredentialV2)
		Expect(err).To(MatchError(formatgate.ErrNotReady))
		Expect(err).To(MatchError(ContainSubstring("gateway pod(s) do not read")))
	})

	It("stays shut when a pod does not report (an older pod writes no row, and the count falls short)", func(ctx SpecContext) {
		report(ctx, formatgate.API, "api-0", v2)
		report(ctx, formatgate.API, "api-1", v2)
		report(ctx, formatgate.Reconciler, "rec-0", v2)
		report(ctx, formatgate.Gateway, "gw-0", v2) // gw-1 never reports
		Expect(gate.Ready(ctx, formatgate.MCPCredentialV2)).To(MatchError(formatgate.ErrNotReady))
	})

	It("stays shut when a whole component does not report", func(ctx SpecContext) {
		report(ctx, formatgate.API, "api-0", v2)
		report(ctx, formatgate.API, "api-1", v2)
		report(ctx, formatgate.Gateway, "gw-0", v2)
		report(ctx, formatgate.Gateway, "gw-1", v2)
		Expect(gate.Ready(ctx, formatgate.MCPCredentialV2)).To(MatchError(ContainSubstring("0 of 1 reconciler")))
	})

	It("opens when a component is newer than N: it lists the format and more", func(ctx SpecContext) {
		allAtN(ctx)
		report(ctx, formatgate.Reconciler, "rec-0", v2, "a-later-format")
		Expect(gate.Ready(ctx, formatgate.MCPCredentialV2)).To(Succeed())
	})

	It("ignores a stale report: a pod that stopped does not keep the gate open", func(ctx SpecContext) {
		allAtN(ctx)
		_, err := pool.Exec(ctx, `UPDATE component_reports SET reported_at = now() - interval '10 minutes' WHERE instance = 'gw-1'`)
		Expect(err).NotTo(HaveOccurred())
		Expect(gate.Ready(ctx, formatgate.MCPCredentialV2)).To(MatchError(formatgate.ErrNotReady))
		gate.TTL = time.Hour
		Expect(gate.Ready(ctx, formatgate.MCPCredentialV2)).To(Succeed())
	})

	It("stays shut when no replica count is set for a reader (fail closed)", func(ctx SpecContext) {
		allAtN(ctx)
		gate.Expect = map[formatgate.Component]int{formatgate.API: 2, formatgate.Reconciler: 1}
		Expect(gate.Ready(ctx, formatgate.MCPCredentialV2)).To(MatchError(ContainSubstring("no replica count is set for gateway")))
	})

	It("refreshes a pod's row in place", func(ctx SpecContext) {
		report(ctx, formatgate.API, "api-0")
		report(ctx, formatgate.API, "api-0", v2)
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM component_reports WHERE instance = 'api-0' AND $1 = ANY (reads)`, v2).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	})
})

var _ = Describe("the registry", func() {
	It("reports to every component the formats it must read", func() {
		for _, c := range formatgate.MCPCredentialV2.Readers {
			Expect(formatgate.Reads(c)).To(ContainElement(formatgate.MCPCredentialV2.Name))
		}
		Expect(formatgate.Reads("agentd")).To(BeEmpty())
	})
	It("reads an expectation", func() {
		got, err := formatgate.ParseExpect("api=2, reconciler=1,gateway=3")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(map[formatgate.Component]int{formatgate.API: 2, formatgate.Reconciler: 1, formatgate.Gateway: 3}))
		for _, bad := range []string{"api", "api=0", "api=x", "=2"} {
			_, err := formatgate.ParseExpect(bad)
			Expect(err).To(HaveOccurred(), bad)
		}
	})
})
