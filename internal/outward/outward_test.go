// SPDX-License-Identifier: Apache-2.0

package outward_test

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/outward"
	"github.com/stateracloud/koinon/internal/restorehold"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var _ = Describe("the outward switch", func() {
	var (
		svc   *outward.Service
		org   pgtype.UUID
		user  pgtype.UUID
		actor audit.Actor
		q     *store.Queries
	)
	BeforeEach(func(ctx SpecContext) {
		svc = outward.New(pool)
		org, user, _ = pgtest.NewOrg(ctx, pool)
		actor = audit.User(user)
		q = store.New(pool)
	})

	insert := func(ctx SpecContext, sql string, args ...any) pgtype.UUID {
		GinkgoHelper()
		var id pgtype.UUID
		Expect(pool.QueryRow(ctx, sql, args...).Scan(&id)).To(Succeed())
		return id
	}
	mcp := func(ctx SpecContext, name, secret string) pgtype.UUID {
		GinkgoHelper()
		if secret == "" {
			return insert(ctx, `INSERT INTO mcp_servers (org_id, name, url) VALUES ($1, $2, 'https://mcp.example.com/mcp') RETURNING id`, org, name)
		}
		return insert(ctx, `INSERT INTO mcp_servers (org_id, name, url, source, managed_by_secret) VALUES ($1, $2, 'https://mcp.example.com/mcp', 'secret', $3) RETURNING id`, org, name, secret)
	}
	provider := func(ctx SpecContext, name string) pgtype.UUID {
		GinkgoHelper()
		return insert(ctx, `INSERT INTO providers (org_id, name, type, url) VALUES ($1, $2, 'openai', 'https://api.example.com/v1') RETURNING id`, org, name)
	}
	connection := func(ctx SpecContext, name string) pgtype.UUID {
		GinkgoHelper()
		return insert(ctx, `INSERT INTO connections (org_id, connector, display_name, secret_name) VALUES ($1, 'github', $2, 'conn-' || substr(md5(random()::text), 1, 12)) RETURNING id`, org, name)
	}
	trace := func(ctx SpecContext, name string) pgtype.UUID {
		GinkgoHelper()
		return insert(ctx, `INSERT INTO trace_destinations (org_id, name, backend, endpoint_url) VALUES ($1, $2, 'generic', 'https://traces.example.com/v1/traces') RETURNING id`, org, name)
	}
	events := func(ctx SpecContext, action string) []store.AuditEvent {
		GinkgoHelper()
		rows, err := q.ListAuditEvents(ctx, store.ListAuditEventsParams{OrgID: org, PageSize: 100})
		Expect(err).NotTo(HaveOccurred())
		var out []store.AuditEvent
		for _, r := range rows {
			if r.Action == action {
				out = append(out, r)
			}
		}
		return out
	}

	It("takes a disabled MCP server out of what is served and audits the pair", func(ctx SpecContext) {
		id := mcp(ctx, "docs", "")
		served, err := q.ListServedMCPServers(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(served).To(HaveLen(1))

		st, err := svc.SetDisabled(ctx, org, actor, outward.MCPServer, id, true, "source credentials")
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Disabled).To(BeTrue())
		Expect(st.Note).To(Equal("source credentials"))
		served, err = q.ListServedMCPServers(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(served).To(BeEmpty())
		_, err = q.GetServedMCPServerByName(ctx, store.GetServedMCPServerByNameParams{OrgID: org, Name: "docs"})
		Expect(err).To(HaveOccurred())
		Expect(events(ctx, outward.ActionDisable)).To(HaveLen(1))

		// The same state again changes and audits nothing.
		_, err = svc.SetDisabled(ctx, org, actor, outward.MCPServer, id, true, "again")
		Expect(err).NotTo(HaveOccurred())
		Expect(events(ctx, outward.ActionDisable)).To(HaveLen(1))

		_, err = svc.SetDisabled(ctx, org, actor, outward.MCPServer, id, false, "")
		Expect(err).NotTo(HaveOccurred())
		served, err = q.ListServedMCPServers(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(served).To(HaveLen(1))
		Expect(events(ctx, outward.ActionEnable)).To(HaveLen(1))
	})

	It("takes a disabled provider out of the served catalog", func(ctx SpecContext) {
		id := provider(ctx, "openai")
		_, err := svc.SetDisabled(ctx, org, actor, outward.ModelProvider, id, true, "")
		Expect(err).NotTo(HaveOccurred())
		providers, err := q.ListServedProviders(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(providers).To(BeEmpty())
	})

	It("makes Load refuse a disabled connection and counts a disabled trace destination out", func(ctx SpecContext) {
		cid := connection(ctx, "github")
		_, err := svc.SetDisabled(ctx, org, actor, outward.CodeHostConnection, cid, true, "")
		Expect(err).NotTo(HaveOccurred())
		row, err := q.GetConnection(ctx, store.GetConnectionParams{OrgID: org, ID: cid})
		Expect(err).NotTo(HaveOccurred())
		Expect(outward.Check(row.AdminDisabledAt)).To(MatchError(outward.ErrDisabled))

		tid := trace(ctx, "langfuse")
		n, err := q.CountEnabledTraceDestinations(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeEquivalentTo(1))
		_, err = svc.SetDisabled(ctx, org, actor, outward.TraceDestination, tid, true, "")
		Expect(err).NotTo(HaveOccurred())
		n, err = q.CountEnabledTraceDestinations(ctx, org)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeEquivalentTo(0))
	})

	It("does not find a target of another org", func(ctx SpecContext) {
		id := mcp(ctx, "docs", "")
		other, _, _ := pgtest.NewOrg(ctx, pool)
		_, err := svc.SetDisabled(ctx, other, actor, outward.MCPServer, id, true, "")
		Expect(err).To(MatchError(outward.ErrNotFound))
	})

	Describe("in restore-hold mode", func() {
		It("allows a disable and refuses an enable", func(ctx SpecContext) {
			held := outward.New(pool).WithRestoreHold(restorehold.NewGuard(true))
			id := mcp(ctx, "docs", "")
			_, err := held.SetDisabled(ctx, org, actor, outward.MCPServer, id, true, "")
			Expect(err).NotTo(HaveOccurred())
			_, err = held.SetDisabled(ctx, org, actor, outward.MCPServer, id, false, "")
			Expect(err).To(MatchError(restorehold.ErrHeld))
			st, err := svc.Get(ctx, outward.MCPServer, org, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(st.Disabled).To(BeTrue())
		})
	})

	It("wakes the reconciler when an admin disables or enables an MCP server", func(ctx SpecContext) {
		id := mcp(ctx, "docs", "")
		conn, err := pool.Acquire(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer conn.Release()
		_, err = conn.Exec(ctx, `LISTEN koinon_reconcile`)
		Expect(err).NotTo(HaveOccurred())
		// Other specs share the database: drain until a notification names this org.
		woken := func() bool {
			wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			for {
				n, err := conn.Conn().WaitForNotification(wctx)
				if err != nil {
					return false
				}
				if n.Payload == org.String() {
					return true
				}
			}
		}
		_, err = svc.SetDisabled(ctx, org, actor, outward.MCPServer, id, true, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(woken()).To(BeTrue())
		_, err = svc.SetDisabled(ctx, org, actor, outward.MCPServer, id, false, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(woken()).To(BeTrue())
	})

	Describe("a declaring Secret", func() {
		It("disables what it manages, wins over the API, and releases only its own disable", func(ctx SpecContext) {
			id := mcp(ctx, "wiki", "wiki-secret")
			n, err := svc.SyncDeclared(ctx, org, outward.MCPServer, "wiki-secret", true)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(1))
			// Again: nothing changes.
			n, err = svc.SyncDeclared(ctx, org, outward.MCPServer, "wiki-secret", true)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(0))

			_, err = svc.SetDisabled(ctx, org, actor, outward.MCPServer, id, false, "")
			Expect(err).To(MatchError(outward.ErrDeclared))

			n, err = svc.SyncDeclared(ctx, org, outward.MCPServer, "wiki-secret", false)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(1))
			st, err := svc.Get(ctx, outward.MCPServer, org, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(st.Disabled).To(BeFalse())
		})

		It("claims an item an admin disabled, so the API cannot enable it while the Secret says disabled", func(ctx SpecContext) {
			id := mcp(ctx, "wiki", "wiki-secret")
			_, err := svc.SetDisabled(ctx, org, actor, outward.MCPServer, id, true, "by hand")
			Expect(err).NotTo(HaveOccurred())
			n, err := svc.SyncDeclared(ctx, org, outward.MCPServer, "wiki-secret", true)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(1))
			st, err := svc.Get(ctx, outward.MCPServer, org, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(st.Declared()).To(BeTrue())
			_, err = svc.SetDisabled(ctx, org, actor, outward.MCPServer, id, false, "")
			Expect(err).To(MatchError(outward.ErrDeclared))
			n, err = svc.SyncDeclared(ctx, org, outward.MCPServer, "wiki-secret", true)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(0))
		})

		It("leaves an admin's disable when the Secret does not say disabled", func(ctx SpecContext) {
			id := mcp(ctx, "wiki", "wiki-secret")
			_, err := svc.SetDisabled(ctx, org, actor, outward.MCPServer, id, true, "")
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.SyncDeclared(ctx, org, outward.MCPServer, "wiki-secret", false)
			Expect(err).NotTo(HaveOccurred())
			st, err := svc.Get(ctx, outward.MCPServer, org, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(st.Disabled).To(BeTrue())
		})

		It("does not enable while the install is held", func(ctx SpecContext) {
			id := mcp(ctx, "wiki", "wiki-secret")
			_, err := svc.SyncDeclared(ctx, org, outward.MCPServer, "wiki-secret", true)
			Expect(err).NotTo(HaveOccurred())
			held := outward.New(pool).WithRestoreHold(restorehold.NewGuard(true))
			n, err := held.SyncDeclared(ctx, org, outward.MCPServer, "wiki-secret", false)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(0))
			st, err := svc.Get(ctx, outward.MCPServer, org, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(st.Disabled).To(BeTrue())
		})
	})

	It("switches the platform trace destination with an install event", func(ctx SpecContext) {
		_, err := pool.Exec(ctx, `INSERT INTO platform_trace_destination (backend, endpoint_url) VALUES ('generic', 'https://traces.example.com/v1/traces') ON CONFLICT (singleton) DO NOTHING`)
		Expect(err).NotTo(HaveOccurred())
		st, err := svc.SetDisabled(ctx, pgtype.UUID{}, actor, outward.PlatformTraceDestination, pgtype.UUID{}, true, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Disabled).To(BeTrue())
		var n int
		Expect(pool.QueryRow(ctx, `SELECT count(*) FROM install_audit_events WHERE action = $1`, outward.ActionDisable).Scan(&n)).To(Succeed())
		Expect(n).To(BeNumerically(">=", 1))
	})
})
