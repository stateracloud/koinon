// SPDX-License-Identifier: Apache-2.0

// Package pgtest gives Ginkgo suites migrated Postgres databases (old issue 224).
//
// A test process starts at most one Postgres container per kind (the default fast one and the
// Durable one), on first use, and every Run, Start, StartTemplate and Template.NewDB gets its own
// database on it. The schema is migrated once per server into a template database named after a
// hash of the migrations (MigrationsHash), and every migrated database is a `CREATE DATABASE ...
// TEMPLATE` copy of it, so a suite or a spec never re-runs the migrations.
//
// The default server can also be shared by every process of a test run: when ServerVar holds the
// superuser DSN of a server (hack/go-test.sh starts one with hack/pgserver), the fast databases are
// created there instead of in a container of the process's own. Without it (`go test ./internal/x`)
// the process starts its own container, so a package runs stand-alone as before. Durable always
// uses the process's own container.
//
// Isolation under parallel Ginkgo (-p, hack/go-test.sh): every database has a name unique on the
// server (a per-process random part), the template is built under an advisory lock and only read
// afterwards, so processes share no database and no row. They do share the server's cluster-wide
// objects: a suite on the default server changes no server setting and creates a role only under a
// name no other suite uses, through EnsureRole (race-free between processes) and without dropping it
// (the migrations suite's probe roles). Above all it never creates a role the migrations or the code
// look for (koinon_app) on the shared server, since that would change what another process's template
// or grants see. The durable suite, which does, runs on Durable, a container no other process reaches.
//
// Roles are cluster-wide, so the one server-side input of the template besides the migration files is
// whether koinon_app exists when it is built (00012 and 00019 revoke from it only then). The template
// key records that: see AppRole and TemplateKey.
// A suite that is run in parallel must start Postgres in BeforeSuite (not in an init or a package
// variable) and may not rely on state outside the process other than its databases.
package pgtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"sync"
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
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// Image is the minimum supported Postgres version (ADR 0047). It is the one place the test image
// is pinned; suites never spell it out.
const Image = "postgres:18-alpine"

// Database is the name of the database every container starts with. Suites get databases of
// their own beside it.
const Database = "koinon"

// ServerVar names the environment variable with the superuser DSN of a running Postgres that the
// default (non-Durable) databases of every process of a run are created on. hack/go-test.sh sets
// it; when it is empty each process starts its own container.
const ServerVar = "KOINON_PGTEST_DSN"

// maxConnections is high enough for every process of a run on one shared server: each suite
// holds a pool or two, DBOS runtimes hold their own.
const maxConnections = "500"

type config struct{ durable bool }

// Option changes how Run, Start or StartTemplate start Postgres.
type Option func(*config)

// Durable keeps Postgres on its production durability settings (fsync, synchronous_commit and
// full_page_writes on, a data directory that is not tmpfs). By default the server trades
// durability for speed: the data is thrown away with the container, so a crash cannot matter.
// Crash-safety suites (the ledger, DBOS restarts) use Durable so nothing relies on fsync being off
// by accident. Durable databases live on a container of the process's own, never on ServerVar.
func Durable() Option { return func(c *config) { c.durable = true } }

func newConfig(opts []Option) config {
	var c config
	for _, o := range opts {
		o(&c)
	}
	return c
}

// fastFlags are the server settings of the default, fast container.
var fastFlags = []string{
	"-c", "fsync=off",
	"-c", "synchronous_commit=off",
	"-c", "full_page_writes=off",
	"-c", "max_connections=" + maxConnections,
}

// durableFlags spell out what Durable guarantees, so the container does not depend on the image
// or the testcontainers module default (the module itself starts with fsync=off).
var durableFlags = []string{
	"-c", "fsync=on",
	"-c", "synchronous_commit=on",
	"-c", "full_page_writes=on",
	"-c", "max_connections=" + maxConnections,
}

func customizers(c config) []testcontainers.ContainerCustomizer {
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

// StartServer starts a Postgres container outside Ginkgo and returns the superuser DSN of its
// maintenance database and a function that removes it. hack/pgserver uses it to start the server
// a whole test run shares (ServerVar); suites use Run, Start or StartTemplate.
func StartServer(ctx context.Context, opts ...Option) (dsn string, stop func(context.Context) error, err error) {
	return startServer(ctx, newConfig(opts))
}

func startServer(ctx context.Context, c config) (dsn string, stop func(context.Context) error, err error) {
	ctr, err := postgres.Run(ctx, Image, customizers(c)...)
	if err != nil {
		return "", nil, err
	}
	stop = func(context.Context) error { return testcontainers.TerminateContainer(ctr) }
	if dsn, err = ctr.ConnectionString(ctx, "sslmode=disable"); err == nil {
		dsn, err = withDatabase(dsn, "postgres")
	}
	if err != nil {
		_ = stop(ctx)
		return "", nil, err
	}
	return dsn, stop, nil
}

// server is one Postgres the process creates databases on.
type server struct {
	dsn  string                          // superuser DSN of the maintenance database
	stop func(ctx context.Context) error // nil for the run's shared server (ServerVar)
	refs int
	tmpl string // the template database, once ensureTemplate built or found it
}

var (
	mu      sync.Mutex
	servers = map[bool]*server{} // by config.durable
)

// acquire returns the process's server of kind c, starting it on first use, and releases it when
// the current node ends: the container is removed when its last user's node has ended. In practice
// the BeforeSuite's user holds it for the whole suite.
func acquire(ctx context.Context, c config) *server {
	ginkgo.GinkgoHelper()
	testenv.Require(testenv.Docker)
	mu.Lock()
	defer mu.Unlock()
	s := servers[c.durable]
	if s == nil {
		s = &server{}
		if shared := os.Getenv(ServerVar); shared != "" && !c.durable {
			s.dsn = shared
		} else {
			dsn, stop, err := startServer(ctx, c)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			s.dsn, s.stop = dsn, stop
		}
		servers[c.durable] = s
	}
	s.refs++
	ginkgo.DeferCleanup(func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if s.refs--; s.refs > 0 {
			return nil
		}
		delete(servers, c.durable)
		if s.stop == nil {
			return nil
		}
		return s.stop(ctx)
	})
	return s
}

// procID makes database names unique among the processes that share a server.
var procID = sync.OnceValue(func() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
})

var dbSeq atomic.Int64

// newDatabase creates a database on s, empty or as a copy of template, and drops it when the
// current node ends (the pools on it are closed first: cleanups run in reverse order).
func (s *server) newDatabase(ctx context.Context, template string) string {
	ginkgo.GinkgoHelper()
	name := fmt.Sprintf("koinon_%s_%d", procID(), dbSeq.Add(1))
	stmt := "CREATE DATABASE " + name
	if template != "" {
		stmt += " TEMPLATE " + template
	}
	gomega.Expect(s.exec(ctx, stmt)).To(gomega.Succeed())
	ginkgo.DeferCleanup(func(ctx context.Context) {
		// Best effort: a leftover only costs space on a server that is thrown away.
		if err := s.exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			ginkgo.GinkgoWriter.Printf("pgtest: drop %s: %v\n", name, err)
		}
	})
	return WithDatabase(s.dsn, name)
}

func (s *server) exec(ctx context.Context, stmt string, args ...any) error {
	conn, err := pgx.Connect(ctx, s.dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	_, err = conn.Exec(ctx, stmt, args...)
	return err
}

// templateFormat is part of the template key: bump it when how the template is built changes
// without a migration changing, so a long-lived shared server does not hand out the old one.
const templateFormat = "pgtest-template-v2"

// AppRole is the NOLOGIN role the chart's CNPG bootstrap creates and the code grants to (ADR 0003).
// Migrations 00012 and 00019 revoke from it only when it exists, so a template built on a server
// that has it differs from one built without. Shared servers never get it from pgtest.
const AppRole = "koinon_app"

const gooseModule = "github.com/pressly/goose/v3"

// gooseVersion is the version of the goose module linked into this binary: the migrations are run
// by it, so a different goose can build a different schema from the same files.
func gooseVersion() (string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", fmt.Errorf("pgtest: no build info, cannot tell the goose version")
	}
	for _, dep := range info.Deps {
		if dep.Path == gooseModule {
			if dep.Replace != nil {
				return dep.Replace.Path + "@" + dep.Replace.Version + dep.Replace.Sum, nil
			}
			return dep.Version, nil
		}
	}
	return "", fmt.Errorf("pgtest: %s is not in the build info", gooseModule)
}

// MigrationsHash is the hex SHA-256 of the goose version and every file of fsys (path and content,
// in lexical order), the key of the template database: a changed, added or removed migration, or a
// different goose, gives a new template.
func MigrationsHash(fsys fs.FS) (string, error) {
	v, err := gooseVersion()
	if err != nil {
		return "", err
	}
	return migrationsHashFor(fsys, v)
}

func migrationsHashFor(fsys fs.FS, goose string) (string, error) {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00", templateFormat, goose)
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", path, len(b))
		_, _ = h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var migrationsHash = sync.OnceValues(func() (string, error) { return MigrationsHash(migrations.FS) })

// TemplateName is the template database of the current migrations.
func TemplateName() string {
	ginkgo.GinkgoHelper()
	hash, err := migrationsHash()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return "koinon_tmpl_" + hash[:16]
}

// TemplateKey is the name of the template database that fits the server at adminDSN: TemplateName,
// plus "_app" when the server has the role AppRole, because the migrations then run differently. A
// server on which the role appears or goes away gets a second template instead of a stale one. The
// check is not atomic with the build; nothing in pgtest creates the role on a shared server.
func TemplateKey(ctx context.Context, adminDSN string) (string, error) {
	ginkgo.GinkgoHelper()
	name := TemplateName()
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var has bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, AppRole).Scan(&has); err != nil {
		return "", err
	}
	if has {
		name += "_app"
	}
	return name, nil
}

// templateBuilds counts the templates this process migrated (the tests check it).
var templateBuilds atomic.Int64

// template returns the migrated template database of s, building it when no process has yet.
func (s *server) template(ctx context.Context) string {
	ginkgo.GinkgoHelper()
	mu.Lock()
	defer mu.Unlock()
	if s.tmpl == "" {
		name, err := TemplateKey(ctx, s.dsn)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(ensureTemplate(ctx, s.dsn, name)).To(gomega.Succeed())
		s.tmpl = name
	}
	return s.tmpl
}

// ensureTemplate builds the template database name on the server at adminDSN unless it is there.
// Processes that share the server serialise on an advisory lock (on the maintenance database, so
// every process takes the same one); the template is marked IS_TEMPLATE only after its migrations
// ran, so a half-built one (a process that died while building) is dropped and rebuilt. It ends
// with ALLOW_CONNECTIONS false and no session on it: CREATE DATABASE ... TEMPLATE refuses a source
// that has connections, and none can open afterwards.
func ensureTemplate(ctx context.Context, adminDSN, name string) error {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	ready := func() (bool, error) {
		var ok bool
		err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1 AND datistemplate)`, name).Scan(&ok)
		return ok, err
	}
	if ok, err := ready(); err != nil || ok {
		return err
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, name); err != nil {
		return err
	}
	// Closing the connection releases the lock as well.
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, name)
	}()
	if ok, err := ready(); err != nil || ok {
		return err
	}
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		return err
	}
	dsn, err := withDatabase(adminDSN, name)
	if err != nil {
		return err
	}
	// The chart creates the install audit writer group role before the first migration; so do we.
	// Roles are cluster-wide, hence the race-free helper.
	rdb, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return err
	}
	err = EnsureRole(ctx, rdb, migrations.InstallAuditWriterRole)
	_ = rdb.Close()
	if err != nil {
		return err
	}
	if err := migrate(ctx, dsn); err != nil {
		return fmt.Errorf("migrate template %s: %w", name, err)
	}
	templateBuilds.Add(1)
	// The migration's connections are closed; their backends may still be exiting.
	if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name); err != nil {
		return err
	}
	_, err = conn.Exec(ctx, "ALTER DATABASE "+ident+" WITH IS_TEMPLATE true ALLOW_CONNECTIONS false")
	return err
}

func migrate(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return err
	}
	if _, err = provider.Up(ctx); err != nil {
		return err
	}
	// koinon-api migrate grants the install audit writer role after goose, once the chart (here: the
	// test) has created it.
	_, err = migrations.GrantInstallAuditWriter(ctx, db)
	return err
}

// Execer is what EnsureRole needs of *sql.DB, *sql.Conn and *sql.Tx.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// EnsureRole creates the NOLOGIN role name unless it exists, for any number of processes and
// goroutines at once. Roles are cluster-wide, so concurrent creators share one name space. Checking
// pg_roles first and then creating is a race: two sessions both see no role and the second CREATE
// ROLE fails on pg_authid_rolname_index (unique_violation, or duplicate_object when it lost by a
// commit). So it creates first and treats both errors as "someone else did". A creator that finds
// the name taken by an uncommitted CREATE ROLE waits on that transaction and then gets the error.
// Use it for roles no other suite uses; never for AppRole on a shared server.
func EnsureRole(ctx context.Context, db Execer, name string) error {
	if !roleName.MatchString(name) {
		return fmt.Errorf("pgtest: role name %q is not a plain lower-case identifier", name)
	}
	_, err := db.ExecContext(ctx, `DO $$ BEGIN CREATE ROLE `+name+` NOLOGIN; `+
		`EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$`)
	return err
}

// Run creates an empty database (nothing migrated) and returns its DSN. The database is dropped
// when the current spec node ends; call it from a BeforeSuite, a BeforeAll or a spec. Without
// Docker it skips that node (fails it under KOINON_REQUIRE=docker, see testenv), so a suite needs
// no Docker check of its own when Postgres is the first container its BeforeSuite starts.
func Run(ctx context.Context, opts ...Option) string {
	ginkgo.GinkgoHelper()
	return acquire(ctx, newConfig(opts)).newDatabase(ctx, "")
}

// Migrate applies every migration to the database at dsn.
func Migrate(ctx context.Context, dsn string) {
	ginkgo.GinkgoHelper()
	gomega.Expect(migrate(ctx, dsn)).To(gomega.Succeed())
}

// MigrateErr applies every migration to the database at dsn and returns the error, for specs that
// migrate as a restricted role and expect a failure.
func MigrateErr(ctx context.Context, dsn string) error { return migrate(ctx, dsn) }

// Start returns a pool on a new migrated database (a copy of the template); call it from a
// BeforeSuite. The pool is closed and the database dropped when the node ends.
func Start(ctx context.Context, opts ...Option) *pgxpool.Pool {
	ginkgo.GinkgoHelper()
	return StartTemplate(ctx, opts...).NewDB(ctx)
}

// Template is a server with a migrated template database; NewDB copies it per spec.
type Template struct{ s *server }

// StartTemplate makes sure the server has the migrated template and returns the handle whose
// NewDB gives every spec a fresh copy, so state never leaks between specs.
func StartTemplate(ctx context.Context, opts ...Option) *Template {
	ginkgo.GinkgoHelper()
	s := acquire(ctx, newConfig(opts))
	s.template(ctx)
	return &Template{s: s}
}

// DSN creates a copy of the migrated template, dropped when the current node ends, and returns
// its connection string.
func (t *Template) DSN(ctx context.Context) string {
	ginkgo.GinkgoHelper()
	return t.s.newDatabase(ctx, t.s.template(ctx))
}

// NewDB returns a pool on a fresh copy of the migrated template, closed when the spec ends.
func (t *Template) NewDB(ctx context.Context) *pgxpool.Pool {
	ginkgo.GinkgoHelper()
	pool, err := pgxpool.New(ctx, t.DSN(ctx))
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	ginkgo.DeferCleanup(pool.Close)
	return pool
}

// WithDatabase returns dsn pointed at another database of the same server.
func WithDatabase(dsn, name string) string {
	ginkgo.GinkgoHelper()
	out, err := withDatabase(dsn, name)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return out
}

func withDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
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
