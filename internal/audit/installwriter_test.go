// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"bytes"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

// koinon-api checks its own login (maintainer decision 2026-11-07): only it knows the role it
// connects as. A server of its own: roles are global to a Postgres cluster.
var _ = Describe("koinon-api's check of its own install audit writer membership", func() {
	It("sets the gauge and logs one line per change of state, with the GRANT for its own login", func(ctx SpecContext) {
		dsn, stop, err := pgtest.StartServer(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(stop)
		admin, err := pgx.Connect(ctx, dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx SpecContext) { _ = admin.Close(ctx) })
		exec := func(stmt string) {
			GinkgoHelper()
			_, err := admin.Exec(ctx, stmt)
			Expect(err).NotTo(HaveOccurred(), stmt)
		}
		// A login whose name needs quoting: the GRANT in the log must be one an operator can paste.
		const password = "koinon_test_not-a-real-token"
		exec(`CREATE ROLE "Koinon-API" LOGIN PASSWORD '` + password + `'`)
		exec(`CREATE ROLE koinon_noinherit_spec LOGIN NOINHERIT PASSWORD '` + password + `'`)
		as := func(user string) *pgxpool.Pool {
			GinkgoHelper()
			u, err := url.Parse(dsn)
			Expect(err).NotTo(HaveOccurred())
			u.User = url.UserPassword(user, password)
			p, err := pgxpool.New(ctx, u.String())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(p.Close)
			return p
		}

		reg := prometheus.NewRegistry()
		audit.RegisterInstallWriterMetric(reg)
		gauge := func() float64 {
			GinkgoHelper()
			fams, err := reg.Gather()
			Expect(err).NotTo(HaveOccurred())
			Expect(fams).To(HaveLen(1))
			Expect(fams[0].GetName()).To(Equal("koinon_install_audit_writer_ok"))
			Expect(fams[0].GetMetric()).To(HaveLen(1))
			Expect(fams[0].GetMetric()[0].GetLabel()).To(BeEmpty(), "no labels (ADR 0023)")
			return fams[0].GetMetric()[0].GetGauge().GetValue()
		}
		var logs bytes.Buffer
		lines := func() []string {
			s := strings.TrimSpace(logs.String())
			logs.Reset()
			if s == "" {
				return nil
			}
			return strings.Split(s, "\n")
		}
		check := &audit.InstallWriterCheck{DB: as("Koinon-API"), Log: slog.New(slog.NewTextHandler(&logs, nil))}
		const grant = `GRANT koinon_install_audit_writer TO \"Koinon-API\"`

		By("no writer role: not ok, one error with the CREATE ROLE and the GRANT")
		s := check.Check(ctx)
		Expect(s).To(Equal(audit.WriterStatus{User: "Koinon-API"}))
		Expect(gauge()).To(BeZero())
		got := lines()
		Expect(got).To(HaveLen(1))
		Expect(got[0]).To(And(ContainSubstring("level=ERROR"), ContainSubstring("does not exist"),
			ContainSubstring("CREATE ROLE koinon_install_audit_writer NOLOGIN"), ContainSubstring(grant)))
		check.Check(ctx)
		Expect(lines()).To(BeEmpty(), "the same state logs nothing again")

		By("the role exists, the login is not a member: one error with the GRANT")
		exec(`CREATE ROLE koinon_install_audit_writer NOLOGIN`)
		Expect(check.Check(ctx)).To(Equal(audit.WriterStatus{User: "Koinon-API", RoleExists: true}))
		Expect(gauge()).To(BeZero())
		got = lines()
		Expect(got).To(HaveLen(1))
		Expect(got[0]).To(And(ContainSubstring("level=ERROR"), ContainSubstring("is not a member"), ContainSubstring(grant)))
		check.Check(ctx)
		Expect(lines()).To(BeEmpty())

		By("the grant lands: ok, the gauge is 1, one info line")
		exec(`GRANT koinon_install_audit_writer TO "Koinon-API"`)
		Expect(check.Check(ctx).OK()).To(BeTrue())
		Expect(gauge()).To(Equal(1.0))
		got = lines()
		Expect(got).To(HaveLen(1))
		Expect(got[0]).To(And(ContainSubstring("level=INFO"), ContainSubstring("now a member")))
		check.Check(ctx)
		Expect(lines()).To(BeEmpty())

		By("a revoke shows at the next check")
		exec(`REVOKE koinon_install_audit_writer FROM "Koinon-API"`)
		Expect(check.Check(ctx).OK()).To(BeFalse())
		Expect(gauge()).To(BeZero())
		Expect(lines()).To(HaveLen(1))

		By("a member that does not inherit the role is not ok: koinon-api inserts without SET ROLE")
		exec(`GRANT koinon_install_audit_writer TO koinon_noinherit_spec`)
		s, err = audit.CheckInstallWriter(ctx, as("koinon_noinherit_spec"))
		Expect(err).NotTo(HaveOccurred())
		Expect(s).To(Equal(audit.WriterStatus{User: "koinon_noinherit_spec", RoleExists: true}))

		By("a database it cannot ask keeps the gauge and logs one warning")
		exec(`GRANT koinon_install_audit_writer TO "Koinon-API"`)
		Expect(check.Check(ctx).OK()).To(BeTrue())
		lines()
		broken := &audit.InstallWriterCheck{DB: check.DB, Log: check.Log}
		closed := as("Koinon-API")
		closed.Close()
		broken.DB = closed
		Expect(broken.Check(ctx)).To(Equal(audit.WriterStatus{}))
		Expect(broken.Check(ctx)).To(Equal(audit.WriterStatus{}))
		Expect(gauge()).To(Equal(1.0), "an unreachable database is not a missing grant")
		got = lines()
		Expect(got).To(HaveLen(1))
		Expect(got[0]).To(ContainSubstring("level=WARN"))

		By("the reconciler or agentd warns when its login can write the install chain (a shared login), and only then")
		audit.WarnSharedWriterLogin(ctx, as("koinon_noinherit_spec"), "koinon-reconciler", check.Log)
		Expect(lines()).To(BeEmpty())
		audit.WarnSharedWriterLogin(ctx, as("Koinon-API"), "koinon-reconciler", check.Log)
		got = lines()
		Expect(got).To(HaveLen(1))
		Expect(got[0]).To(And(ContainSubstring("level=WARN"), ContainSubstring("koinon-reconciler's database login can write the install audit chain")))
	}, SpecTimeout(3*time.Minute))
})
