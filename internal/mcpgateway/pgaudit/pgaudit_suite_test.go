// SPDX-License-Identifier: Apache-2.0

package pgaudit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/mcpgateway"
	"github.com/stateracloud/koinon/internal/mcpgateway/pgaudit"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

func TestPGAudit(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "MCP gateway Postgres audit suite")
}

var _ = Describe("the Postgres audit writer", func() {
	var admin, restricted *pgxpool.Pool

	BeforeEach(func(ctx SpecContext) {
		admin = pgtest.Start(ctx)
		db := stdlib.OpenDBFromPool(admin)
		DeferCleanup(func() { _ = db.Close() })
		// Before the role exists, the grants are a no-op that says so.
		ok, err := migrations.GrantGateway(ctx, db)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(pgtest.EnsureRole(ctx, db, migrations.GatewayRole)).To(Succeed())
		ok, err = migrations.GrantGateway(ctx, db)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		ok, err = migrations.GrantGateway(ctx, db)
		Expect(err).NotTo(HaveOccurred(), "the grants are idempotent")
		Expect(ok).To(BeTrue())

		cfg, err := pgxpool.ParseConfig(admin.Config().ConnString())
		Expect(err).NotTo(HaveOccurred())
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, `SET ROLE `+migrations.GatewayRole)
			return err
		}
		restricted, err = pgxpool.NewWithConfig(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(restricted.Close)
	})

	It("inserts one row per call as the restricted role, with the fields and nothing else", func(ctx SpecContext) {
		w := pgaudit.New(restricted)
		Expect(w.Record(ctx, mcpgateway.AuditRow{
			Agent: gateway.AgentRef{Org: "acme", Name: "reader"}, Target: "alpha", Tool: "read_item",
			Outcome: mcpgateway.OutcomeRefusedGuard, Verdict: "withheld by the leak guard", CredKind: "static",
			Duration: 42 * time.Millisecond,
		})).To(Succeed())

		var org, agent, target, tool, outcome, verdict, kind string
		var ms int32
		Expect(admin.QueryRow(ctx, `SELECT org_slug, agent, target, tool, outcome, verdict, credential_kind, duration_ms FROM mcp_call_log`).
			Scan(&org, &agent, &target, &tool, &outcome, &verdict, &kind, &ms)).To(Succeed())
		Expect([]any{org, agent, target, tool, outcome, verdict, kind, ms}).To(Equal(
			[]any{"acme", "reader", "alpha", "read_item", "refused_guard", "withheld by the leak guard", "static", int32(42)}))

		var cols []string
		rows, err := admin.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'mcp_call_log'`)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		for rows.Next() {
			var c string
			Expect(rows.Scan(&c)).To(Succeed())
			cols = append(cols, c)
		}
		Expect(cols).To(ConsistOf("id", "created_at", "org_slug", "agent", "target", "tool", "outcome", "verdict", "credential_kind", "duration_ms"),
			"the log has no column for an argument, a result or a token")
	})

	It("cuts a long verdict on a character boundary and refuses a made-up outcome", func(ctx SpecContext) {
		w := pgaudit.New(restricted)
		row := mcpgateway.AuditRow{Agent: gateway.AgentRef{Org: "acme", Name: "reader"}, Target: "alpha", Tool: "t", Outcome: mcpgateway.OutcomeOK}
		row.Verdict = strings.Repeat("é", 2000)
		Expect(w.Record(ctx, row)).To(Succeed())
		var n int
		Expect(admin.QueryRow(ctx, `SELECT length(verdict) FROM mcp_call_log`).Scan(&n)).To(Succeed())
		Expect(n).To(BeNumerically("<=", 1024))
		row.Outcome = "approved"
		Expect(w.Record(ctx, row)).NotTo(Succeed())
	})

	It("cannot read, change or delete the log as the restricted role", func(ctx SpecContext) {
		_, err := restricted.Exec(ctx, `SELECT 1 FROM mcp_call_log`)
		Expect(err).To(HaveOccurred())
		_, err = restricted.Exec(ctx, `DELETE FROM mcp_call_log`)
		Expect(err).To(HaveOccurred())
	})

	It("records a row even when the call's context is already cancelled", func(ctx SpecContext) {
		w := pgaudit.New(restricted)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		Expect(w.Record(cancelled, mcpgateway.AuditRow{Agent: gateway.AgentRef{Org: "acme", Name: "a"}, Target: "t", Tool: "x", Outcome: mcpgateway.OutcomeUpstreamError})).To(Succeed())
	})
})
