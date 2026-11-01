// SPDX-License-Identifier: Apache-2.0

// Package pgtest starts a migrated Postgres for a Ginkgo suite with testcontainers.
//
// Parallel Ginkgo (-p, hack/go-test.sh) runs a suite's BeforeSuite in every process, so Run, Start
// and StartTemplate give every process its own container (random host port, removed when that
// process's suite ends): no database, port or row is shared between processes. A suite that is run
// in parallel must start Postgres in BeforeSuite (not in an init or a package variable) and may not
// rely on state outside the process other than that database.
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/stateracloud/koinon/internal/db/migrations"
)

// Image is the minimum supported Postgres version (ADR 0047). It is the one place the test image
// is pinned; suites never spell it out.
const Image = "postgres:18-alpine"

// Database is the name of the database every container starts with.
const Database = "koinon"

type config struct{ durable bool }

// Option changes how Run, Start or StartTemplate start Postgres.
type Option func(*config)

// Durable keeps Postgres on its production durability settings (fsync, synchronous_commit and
// full_page_writes on, a data directory that is not tmpfs). By default the container trades
// durability for speed: the data is thrown away with the container, so a crash cannot matter.
// Crash-safety suites (the ledger, DBOS restarts) use Durable so nothing relies on fsync being off
// by accident.
func Durable() Option { return func(c *config) { c.durable = true } }

// fastFlags are the server settings of the default, fast container.
var fastFlags = []string{
	"-c", "fsync=off",
	"-c", "synchronous_commit=off",
	"-c", "full_page_writes=off",
}

// durableFlags spell out what Durable guarantees, so the container does not depend on the image
// or the testcontainers module default (the module itself starts with fsync=off).
var durableFlags = []string{
	"-c", "fsync=on",
	"-c", "synchronous_commit=on",
	"-c", "full_page_writes=on",
}

func customizers(opts []Option) []testcontainers.ContainerCustomizer {
	var c config
	for _, o := range opts {
		o(&c)
	}
	cs := []testcontainers.ContainerCustomizer{
		postgres.WithDatabase(Database),
		postgres.BasicWaitStrategies(),
	}
	if c.durable {
		return append(cs, testcontainers.WithCmd(append([]string{"postgres"}, durableFlags...)...))
	}
	return append(cs,
		testcontainers.WithCmd(append([]string{"postgres"}, fastFlags...)...),
		testcontainers.WithEnv(map[string]string{"POSTGRES_INITDB_ARGS": "--no-sync"}),
		testcontainers.WithTmpfs(map[string]string{"/var/lib/postgresql": "rw"}),
	)
}

// Run starts an empty Postgres (nothing migrated) and returns its DSN. The container is removed
// when the current spec node ends; call it from a BeforeSuite or BeforeAll.
func Run(ctx context.Context, opts ...Option) string {
	ctr, err := postgres.Run(ctx, Image, customizers(opts)...)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.DeferCleanup(func(ctx context.Context) error { return testcontainers.TerminateContainer(ctr) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return dsn
}

// Migrate applies every migration to the database at dsn.
func Migrate(ctx context.Context, dsn string) {
	db, err := sql.Open("pgx", dsn)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	defer func() { _ = db.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = provider.Up(ctx)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

// Start runs Postgres and applies every migration; call it from a BeforeSuite. The container
// and the pool are removed when the suite ends.
func Start(ctx context.Context, opts ...Option) *pgxpool.Pool {
	dsn := Run(ctx, opts...)
	Migrate(ctx, dsn)
	pool, err := pgxpool.New(ctx, dsn)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.DeferCleanup(pool.Close)
	return pool
}

// Template is a Postgres server with a migrated template database; NewDB copies it per spec.
type Template struct {
	dsn string // the server's maintenance database
	seq atomic.Int64
}

const templateDB = "koinon_template"

// StartTemplate runs Postgres, migrates a template database once and returns the handle whose
// NewDB gives every spec a fresh copy, so state never leaks between specs.
func StartTemplate(ctx context.Context, opts ...Option) *Template {
	dsn := Run(ctx, opts...)
	t := &Template{dsn: WithDatabase(dsn, "postgres")}
	t.exec(ctx, "CREATE DATABASE "+templateDB)
	Migrate(ctx, WithDatabase(dsn, templateDB))
	return t
}

func (t *Template) exec(ctx context.Context, stmt string) {
	admin, err := sql.Open("pgx", t.dsn)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	defer func() { _ = admin.Close() }()
	_, err = admin.ExecContext(ctx, stmt)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}

// DSN creates a copy of the migrated template and returns its connection string.
func (t *Template) DSN(ctx context.Context) string {
	name := fmt.Sprintf("koinon_%d", t.seq.Add(1))
	t.exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDB))
	return WithDatabase(t.dsn, name)
}

// NewDB returns a pool on a fresh copy of the migrated template, closed when the spec ends.
func (t *Template) NewDB(ctx context.Context) *pgxpool.Pool {
	pool, err := pgxpool.New(ctx, t.DSN(ctx))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.DeferCleanup(pool.Close)
	return pool
}

// WithDatabase returns dsn pointed at another database of the same server.
func WithDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	u.Path = "/" + name
	return u.String()
}

var seq atomic.Int64

// Uniq returns a name unique within the test process.
func Uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

// NewOrg creates an org with an owner and returns their IDs and the org's slug.
func NewOrg(ctx context.Context, pool *pgxpool.Pool) (org, user pgtype.UUID, slug string) {
	slug = Uniq("org")
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Org', 'personal') RETURNING id`, slug).Scan(&org); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', $1, $2) RETURNING id`,
			Uniq("sub"), org).Scan(&user); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner')`, org, user)
		return err
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return org, user, slug
}

// TruncateOrgs empties orgs and every table that references it, for tests that start from nothing.
// The audit log refuses TRUNCATE for every role, the owner included (ADR 0023), and TRUNCATE ...
// CASCADE reaches it, so this runs the statement with session_replication_role = replica, which
// skips ordinary triggers. It needs a role that may set that (the testcontainers superuser).
func TruncateOrgs(ctx context.Context, pool *pgxpool.Pool) {
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `TRUNCATE orgs CASCADE`)
		return err
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
}
