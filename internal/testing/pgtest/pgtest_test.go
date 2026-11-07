// SPDX-License-Identifier: Apache-2.0

package pgtest_test

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/db/migrations"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

func TestPgtest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "pgtest")
}

func show(ctx context.Context, pool *pgxpool.Pool, setting string) string {
	var v string
	Expect(pool.QueryRow(ctx, "SELECT current_setting($1)", setting).Scan(&v)).To(Succeed())
	return v
}

func hasOrgs(ctx context.Context, dsn string) bool {
	conn, err := pgx.Connect(ctx, dsn)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = conn.Close(ctx) }()
	var ok bool
	Expect(conn.QueryRow(ctx, "SELECT to_regclass('public.orgs') IS NOT NULL").Scan(&ok)).To(Succeed())
	return ok
}

var _ = Describe("MigrationsHash", func() {
	base := func() fstest.MapFS {
		return fstest.MapFS{
			"00001_a.sql": {Data: []byte("CREATE TABLE a (id int);")},
			"00002_b.sql": {Data: []byte("CREATE TABLE b (id int);")},
		}
	}
	hash := func(fsys fstest.MapFS) string {
		GinkgoHelper()
		h, err := pgtest.MigrationsHash(fsys)
		Expect(err).NotTo(HaveOccurred())
		return h
	}

	It("is stable for the same files", func() {
		Expect(hash(base())).To(Equal(hash(base())))
	})

	It("changes when a migration's content, name or the set of files changes", func() {
		h := hash(base())
		changed := base()
		changed["00002_b.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE b (id bigint);")}
		Expect(hash(changed)).NotTo(Equal(h), "content")

		renamed := base()
		renamed["00003_b.sql"] = renamed["00002_b.sql"]
		delete(renamed, "00002_b.sql")
		Expect(hash(renamed)).NotTo(Equal(h), "name")

		added := base()
		added["00003_c.sql"] = &fstest.MapFile{Data: []byte("SELECT 1;")}
		Expect(hash(added)).NotTo(Equal(h), "added")
	})

	It("changes when the goose version changes", func() {
		a, err := pgtest.HashWithGoose(base(), "v3.1.0")
		Expect(err).NotTo(HaveOccurred())
		b, err := pgtest.HashWithGoose(base(), "v3.2.0")
		Expect(err).NotTo(HaveOccurred())
		Expect(a).NotTo(Equal(b))
	})

	It("is keyed on the goose version linked into the binary", func() {
		v, err := pgtest.GooseVersion()
		Expect(err).NotTo(HaveOccurred())
		Expect(v).To(MatchRegexp(`^v\d+\.\d+\.\d+`))
		want, err := pgtest.HashWithGoose(base(), v)
		Expect(err).NotTo(HaveOccurred())
		Expect(hash(base())).To(Equal(want))
	})

	It("names the template of the current migrations", func() {
		h, err := pgtest.MigrationsHash(migrations.FS)
		Expect(err).NotTo(HaveOccurred())
		Expect(pgtest.TemplateName()).To(Equal("koinon_tmpl_" + h[:16]))
	})
})

var _ = Describe("pgtest", Ordered, func() {
	var (
		fast, durable *pgxpool.Pool
		tmpl          *pgtest.Template
	)
	BeforeAll(func(ctx SpecContext) {
		fast = pgtest.Start(ctx)
		durable = pgtest.Start(ctx, pgtest.Durable())
		tmpl = pgtest.StartTemplate(ctx)
	}, NodeTimeout(3*time.Minute))

	It("starts fast by default: fsync, synchronous_commit and full_page_writes off", func(ctx SpecContext) {
		Expect(show(ctx, fast, "fsync")).To(Equal("off"))
		Expect(show(ctx, fast, "synchronous_commit")).To(Equal("off"))
		Expect(show(ctx, fast, "full_page_writes")).To(Equal("off"))
	})

	It("keeps production durability settings with Durable", func(ctx SpecContext) {
		Expect(show(ctx, durable, "fsync")).To(Equal("on"))
		Expect(show(ctx, durable, "synchronous_commit")).To(Equal("on"))
		Expect(show(ctx, durable, "full_page_writes")).To(Equal("on"))
	})

	It("migrates the schema", func(ctx SpecContext) {
		var n int
		Expect(fast.QueryRow(ctx, "SELECT count(*) FROM orgs").Scan(&n)).To(Succeed())
		Expect(durable.QueryRow(ctx, "SELECT count(*) FROM orgs").Scan(&n)).To(Succeed())
	})

	It("gives every NewDB its own migrated copy of the template", func(ctx SpecContext) {
		a, b := tmpl.NewDB(ctx), tmpl.NewDB(ctx)
		pgtest.NewOrg(ctx, a)
		var na, nb, nf int
		Expect(a.QueryRow(ctx, "SELECT count(*) FROM orgs").Scan(&na)).To(Succeed())
		Expect(b.QueryRow(ctx, "SELECT count(*) FROM orgs").Scan(&nb)).To(Succeed())
		Expect(fast.QueryRow(ctx, "SELECT count(*) FROM orgs").Scan(&nf)).To(Succeed())
		Expect(na).To(Equal(1))
		Expect(nb).To(BeZero())
		Expect(nf).To(BeZero())
		var da, db string
		Expect(a.QueryRow(ctx, "SELECT current_database()").Scan(&da)).To(Succeed())
		Expect(b.QueryRow(ctx, "SELECT current_database()").Scan(&db)).To(Succeed())
		Expect(da).NotTo(Equal(db))
		Expect(da).To(ContainSubstring(pgtest.ProcID()), "names are unique among processes sharing a server")
	})

	It("copies the template without migrating again", func(ctx SpecContext) {
		builds := pgtest.TemplateBuilds()
		tmpl.NewDB(ctx)
		pgtest.Start(ctx)
		Expect(hasOrgs(ctx, tmpl.DSN(ctx))).To(BeTrue())
		Expect(pgtest.TemplateBuilds()).To(Equal(builds))

		By("another process finds the template on the server and does not build it")
		Expect(pgtest.EnsureTemplate(ctx, tmpl.ServerDSN(), pgtest.TemplateName())).To(Succeed())
		Expect(pgtest.TemplateBuilds()).To(Equal(builds))
	})

	It("keeps the suite's database for the suite and gives Run an empty one", func(ctx SpecContext) {
		var name string
		Expect(fast.QueryRow(ctx, "SELECT current_database()").Scan(&name)).To(Succeed())
		admin, err := pgx.Connect(ctx, tmpl.ServerDSN())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = admin.Close(ctx) })
		count := func(n string) int {
			var c int
			Expect(admin.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname = $1", n).Scan(&c)).To(Succeed())
			return c
		}
		Expect(count(name)).To(Equal(1), "the suite's database lives for the suite")
		// The next spec checks that this spec's database is gone once the spec ended.
		dropped = pgtest.Run(ctx)
		Expect(hasOrgs(ctx, dropped)).To(BeFalse(), "Run creates an empty database")
	})

	It("drops a database when the node that created it ends", func(ctx SpecContext) {
		admin, err := pgx.Connect(ctx, tmpl.ServerDSN())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = admin.Close(ctx) })
		name := dropped[strings.LastIndex(dropped, "/")+1:]
		name, _, _ = strings.Cut(name, "?")
		var c int
		Expect(admin.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname = $1", name).Scan(&c)).To(Succeed())
		Expect(c).To(BeZero())
	})

	It("keys the template on the server's koinon_app role, which 00012 and 00019 branch on", func(ctx SpecContext) {
		// Durable is a container of this process's own, so creating koinon_app there touches no other suite.
		dsn := pgtest.Run(ctx, pgtest.Durable())
		admin, err := sql.Open("pgx", dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(admin.Close)
		Expect(pgtest.EnsureRole(ctx, admin, pgtest.AppRole)).To(Succeed())
		DeferCleanup(func(ctx context.Context) {
			_, _ = admin.ExecContext(ctx, "DROP ROLE IF EXISTS "+pgtest.AppRole)
		})
		with, err := pgtest.TemplateKey(ctx, dsn)
		Expect(err).NotTo(HaveOccurred())
		Expect(with).To(Equal(pgtest.TemplateName() + "_app"))

		_, err = admin.ExecContext(ctx, "DROP ROLE "+pgtest.AppRole)
		Expect(err).NotTo(HaveOccurred())
		without, err := pgtest.TemplateKey(ctx, dsn)
		Expect(err).NotTo(HaveOccurred())
		Expect(without).To(Equal(pgtest.TemplateName()))
	})

	It("builds a template once when several processes ask for it at the same time", func(ctx SpecContext) {
		name := "koinon_tmpl_race_" + pgtest.ProcID()
		admin := tmpl.ServerDSN()
		DeferCleanup(func(ctx context.Context) {
			conn, err := pgx.Connect(ctx, admin)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close(ctx) }()
			_, _ = conn.Exec(ctx, "ALTER DATABASE "+name+" WITH IS_TEMPLATE false")
			_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		})
		builds := pgtest.TemplateBuilds()
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Go(func() { errs[i] = pgtest.EnsureTemplate(ctx, admin, name) })
		}
		wg.Wait()
		for _, err := range errs {
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(pgtest.TemplateBuilds()).To(Equal(builds + 1))

		conn, err := pgx.Connect(ctx, admin)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _ = conn.Close(ctx) })
		var isTemplate, allowConn bool
		Expect(conn.QueryRow(ctx, "SELECT datistemplate, datallowconn FROM pg_database WHERE datname = $1", name).Scan(&isTemplate, &allowConn)).To(Succeed())
		Expect(isTemplate).To(BeTrue())
		Expect(allowConn).To(BeFalse(), "nobody can hold a connection that would block a copy")
		_, err = conn.Exec(ctx, "CREATE DATABASE "+name+"_copy TEMPLATE "+name)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) { _, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+"_copy") })
	}, SpecTimeout(3*time.Minute))
})

var _ = Describe("EnsureRole", func() {
	open := func(ctx context.Context, dsn, app string) *sql.DB {
		GinkgoHelper()
		u, err := url.Parse(dsn)
		Expect(err).NotTo(HaveOccurred())
		q := u.Query()
		q.Set("application_name", app)
		u.RawQuery = q.Encode()
		db, err := sql.Open("pgx", u.String())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(db.Close)
		return db
	}
	dropRole := func(dsn, role string) {
		DeferCleanup(func(ctx context.Context) {
			db, err := sql.Open("pgx", dsn)
			if err != nil {
				return
			}
			defer func() { _ = db.Close() }()
			_, _ = db.ExecContext(ctx, "DROP ROLE IF EXISTS "+role)
		})
	}

	It("is idempotent, and rejects a name that is not a plain identifier", func(ctx SpecContext) {
		dsn := pgtest.Run(ctx)
		db := open(ctx, dsn, "ensure_idem")
		role := "pgtest_role_idem_" + pgtest.ProcID()
		dropRole(dsn, role)
		Expect(pgtest.EnsureRole(ctx, db, role)).To(Succeed())
		Expect(pgtest.EnsureRole(ctx, db, role)).To(Succeed())
		Expect(pgtest.EnsureRole(ctx, db, `x"; DROP ROLE y; --`)).NotTo(Succeed())
	}, SpecTimeout(2*time.Minute))

	It("survives a creator that is still in flight on the same name (the race of old issue 619)", func(ctx SpecContext) {
		dsn := pgtest.Run(ctx)
		role := "pgtest_role_race_" + pgtest.ProcID()
		dropRole(dsn, role)

		// A holds an uncommitted CREATE ROLE: B checks, sees no role, and has to wait on A's commit.
		holder := open(ctx, dsn, "ensure_holder")
		tx, err := holder.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tx.Rollback() })
		_, err = tx.ExecContext(ctx, "CREATE ROLE "+role+" NOLOGIN")
		Expect(err).NotTo(HaveOccurred())

		waiter := open(ctx, dsn, "ensure_waiter")
		done := make(chan error, 1)
		go func() { done <- pgtest.EnsureRole(ctx, waiter, role) }()

		By("waiting until the server shows B blocked on A's transaction")
		observer := open(ctx, dsn, "ensure_observer")
		Eventually(func(g Gomega) int {
			var n int
			g.Expect(observer.QueryRowContext(ctx,
				`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND application_name = 'ensure_waiter' AND wait_event_type = 'Lock'`).Scan(&n)).To(Succeed())
			return n
		}).WithContext(ctx).Should(Equal(1))
		Expect(done).NotTo(Receive(), "B must still be waiting")

		Expect(tx.Commit()).To(Succeed())
		Eventually(done).WithContext(ctx).Should(Receive(BeNil()), "B treats A's role as its own instead of failing on pg_authid_rolname_index")

		var n int
		Expect(observer.QueryRowContext(ctx, `SELECT count(*) FROM pg_roles WHERE rolname = $1`, role).Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))
	}, SpecTimeout(2*time.Minute))

	It("lets many concurrent creators of fresh names all succeed with one role each", func(ctx SpecContext) {
		dsn := pgtest.Run(ctx)
		db := open(ctx, dsn, "ensure_many")
		db.SetMaxOpenConns(16)
		for round := range 5 {
			role := "pgtest_role_many_" + pgtest.ProcID() + "_" + string(rune('a'+round))
			dropRole(dsn, role)
			var wg sync.WaitGroup
			errs := make([]error, 16)
			for i := range errs {
				wg.Go(func() { errs[i] = pgtest.EnsureRole(ctx, db, role) })
			}
			wg.Wait()
			for _, err := range errs {
				Expect(err).NotTo(HaveOccurred())
			}
			var n int
			Expect(db.QueryRowContext(ctx, `SELECT count(*) FROM pg_roles WHERE rolname = $1`, role).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(1))
		}
	}, SpecTimeout(2*time.Minute))
})

var dropped string
