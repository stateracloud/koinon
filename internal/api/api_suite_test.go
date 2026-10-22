// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/procoduck/koinon/internal/api"
	"github.com/procoduck/koinon/internal/auth"
	"github.com/procoduck/koinon/internal/auth/oidctest"
	"github.com/procoduck/koinon/internal/db/migrations"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/gen/koinon/auth/v1alpha1/authv1alpha1connect"
	"github.com/procoduck/koinon/internal/runwatch"
	"github.com/procoduck/koinon/internal/services"
	"github.com/procoduck/koinon/templates"
)

// PostgresImage is the minimum supported Postgres version (ADR 0047).
const PostgresImage = "postgres:18-alpine"

const templateDB = "koinon_template"

var (
	adminDSN string // connects to the server's maintenance database
	dbSeq    atomic.Int64
)

func TestAPI(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "API Suite")
}

// BeforeSuite migrates a template database once; every spec gets a fresh
// copy, so bootstrap state never leaks between specs.
var _ = BeforeSuite(func(ctx SpecContext) {
	ctr, err := postgres.Run(ctx, PostgresImage,
		postgres.WithDatabase(templateDB),
		postgres.BasicWaitStrategies(),
	)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) error { return ctr.Terminate(ctx) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	Expect(err).NotTo(HaveOccurred())
	db, err := sql.Open("pgx", dsn)
	Expect(err).NotTo(HaveOccurred())
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	Expect(err).NotTo(HaveOccurred())
	_, err = provider.Up(ctx)
	Expect(err).NotTo(HaveOccurred())
	Expect(db.Close()).To(Succeed())

	adminDSN = withDatabase(dsn, "postgres")
}, NodeTimeout(3*time.Minute))

func withDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	Expect(err).NotTo(HaveOccurred())
	u.Path = "/" + name
	return u.String()
}

// newPool returns a pool on a fresh copy of the migrated template.
func newPool(ctx context.Context) *pgxpool.Pool {
	name := fmt.Sprintf("koinon_%d", dbSeq.Add(1))
	admin, err := sql.Open("pgx", adminDSN)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = admin.Close() }()
	_, err = admin.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDB))
	Expect(err).NotTo(HaveOccurred())
	pool, err := pgxpool.New(ctx, withDatabase(adminDSN, name))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(pool.Close)
	return pool
}

// clock is a settable time source shared by the API and the test issuer.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const (
	bootstrapPassword = "one-time-bootstrap-pw"
	newPassword       = "a-much-better-password"
	idleTimeout       = 10 * time.Minute
	maxLifetime       = 30 * time.Minute
	probeProcedure    = "/koinon.test.v1.ProbeService/Ping"
)

type envOptions struct {
	bootstrapLoginDisabled bool
	oidc                   bool
	groupMappings          []auth.GroupMapping
	defaultOrgMode         store.IdentityMode
	trustedOrigins         []string
	// watchPoll is WatchRun's backstop poll interval; tests that check
	// NOTIFY delivery set it long so that polling cannot hide a failure.
	watchPoll time.Duration
	// plainHTTP serves over http:// instead of TLS, with a matching PublicURL.
	plainHTTP bool
	// username is the bootstrap admin's login name; empty means the default.
	username string
}

// env is one API server on its own database.
type env struct {
	pool     *pgxpool.Pool
	clock    *clock
	issuer   *oidctest.Issuer
	srv      *httptest.Server
	svc      *auth.Service
	hub      *runwatch.Hub
	pwFile   string
	opts     envOptions
	bootstrp auth.BootstrapResult
}

func newEnv(ctx context.Context, opts envOptions) *env { return newEnvOn(ctx, nil, opts) }

func timeNow() time.Time { return time.Now() }

// newEnvOn starts an API server. With base, it shares base's database and
// clock, like a restart with new settings.
func newEnvOn(ctx context.Context, base *env, opts envOptions) *env {
	e := &env{opts: opts}
	if base != nil {
		e.pool, e.clock, e.pwFile = base.pool, base.clock, base.pwFile
	} else {
		e.pool, e.clock = newPool(ctx), &clock{t: time.Now()}
		e.pwFile = filepath.Join(GinkgoT().TempDir(), "password")
		Expect(os.WriteFile(e.pwFile, []byte(bootstrapPassword+"\n"), 0o600)).To(Succeed())
	}

	var handler http.Handler
	serve := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	})
	if opts.plainHTTP {
		e.srv = httptest.NewServer(serve)
	} else {
		e.srv = httptest.NewTLSServer(serve)
	}
	DeferCleanup(e.srv.Close)

	if opts.oidc {
		e.issuer = oidctest.New("koinon", "client-secret")
		e.issuer.Now = e.clock.Now
		DeferCleanup(e.issuer.Close)
	}
	e.bootstrap(ctx)

	cfg := auth.Config{
		SessionIdleTimeout: idleTimeout,
		SessionMaxLifetime: maxLifetime,
		BootstrapLogin:     !opts.bootstrapLoginDisabled,
		PublicURL:          e.srv.URL,
		Logger:             slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
		Now:                e.clock.Now,
	}
	if opts.oidc {
		cfg.OIDC = &auth.OIDCConfig{
			IssuerURL:    e.issuer.URL(),
			ClientID:     "koinon",
			ClientSecret: "client-secret",
			RedirectURL:  e.srv.URL + "/auth/oidc/callback",
		}
	}
	svc, err := auth.New(e.pool, cfg)
	Expect(err).NotTo(HaveOccurred())
	e.svc = svc

	_, err = services.SyncTemplates(ctx, e.pool, mustTemplates())
	Expect(err).NotTo(HaveOccurred())
	hubCtx, stopHub := context.WithCancel(context.Background())
	e.hub = runwatch.New(e.pool.Config().ConnConfig, slog.New(slog.NewTextHandler(GinkgoWriter, nil)))
	hubDone := make(chan struct{})
	go func() { defer close(hubDone); e.hub.Run(hubCtx) }()
	DeferCleanup(func() { stopHub(); <-hubDone })
	Eventually(e.hub.Listening()).Should(BeClosed())
	rpc := services.New(e.pool, services.Options{
		Hub: e.hub, WatchPollInterval: opts.watchPoll,
		Logger: slog.New(slog.NewTextHandler(GinkgoWriter, nil)),
	})

	handler, err = api.NewHandler(api.Options{
		Auth:           svc,
		TrustedOrigins: opts.trustedOrigins,
		Services:       []func(*connect.Server){registerProbe, rpc.Register},
	})
	Expect(err).NotTo(HaveOccurred())
	return e
}

func (e *env) bootstrap(ctx context.Context) auth.BootstrapResult {
	cfg := auth.BootstrapConfig{
		Username:               e.opts.username,
		PasswordFile:           e.pwFile,
		LoginEnabled:           !e.opts.bootstrapLoginDisabled,
		DefaultOrgIdentityMode: e.opts.defaultOrgMode,
		GroupMappings:          e.opts.groupMappings,
	}
	if e.issuer != nil {
		cfg.OIDCIssuer = e.issuer.URL()
	}
	res, err := auth.Bootstrap(ctx, e.pool, cfg, e.clock.Now())
	Expect(err).NotTo(HaveOccurred())
	e.bootstrp = res
	return res
}

// registerProbe adds a stand-in for any non-auth RPC, to test the gates
// that apply to every procedure.
func registerProbe(server *connect.Server) {
	server.Register(connect.Method{
		Spec: connect.Spec{Procedure: probeProcedure, StreamType: connect.StreamTypeUnary},
		Handler: func(ctx context.Context, _ connect.Spec, stream connect.ServerStream) error {
			if err := stream.Receive(&emptypb.Empty{}); err != nil {
				return err
			}
			if _, ok := auth.PrincipalFrom(ctx); !ok {
				return connect.NewError(connect.CodeInternal, "no principal")
			}
			return stream.Send(&emptypb.Empty{})
		},
	})
}

// browser is a cookie-keeping HTTP client, like one browser profile.
type browser struct {
	e    *env
	http *http.Client
}

func (e *env) browser() *browser {
	jar, err := cookiejar.New(nil)
	Expect(err).NotTo(HaveOccurred())
	return &browser{e: e, http: &http.Client{Transport: e.srv.Client().Transport, Jar: jar}}
}

func (b *browser) auth() authv1alpha1connect.AuthServiceClient {
	return authv1alpha1connect.NewAuthServiceClient(
		connect.NewClient(connecthttp.NewTransport(b.http, b.e.srv.URL)))
}

func (b *browser) probe(ctx context.Context) error {
	c := connect.NewClient(connecthttp.NewTransport(b.http, b.e.srv.URL))
	return c.CallUnary(ctx, connect.Spec{Procedure: probeProcedure, StreamType: connect.StreamTypeUnary},
		&emptypb.Empty{}, &emptypb.Empty{})
}

func (b *browser) login(ctx context.Context, username, password string) *http.Response {
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.e.srv.URL+"/auth/login", strings.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/json")
	res, err := b.http.Do(req)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(res.Body.Close)
	return res
}

func (b *browser) post(ctx context.Context, path string, header http.Header) *http.Response {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.e.srv.URL+path, nil)
	Expect(err).NotTo(HaveOccurred())
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := b.http.Do(req)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(res.Body.Close)
	return res
}

// sessionToken returns the session cookie the browser holds, or "".
func (b *browser) sessionToken() string {
	u, _ := url.Parse(b.e.srv.URL)
	for _, c := range b.http.Jar.Cookies(u) {
		if c.Name == b.e.svc.SessionCookie() {
			return c.Value
		}
	}
	return ""
}

// setSessionToken plants a session cookie, as a replaying attacker would.
func (b *browser) setSessionToken(token string) {
	u, _ := url.Parse(b.e.srv.URL)
	b.http.Jar.SetCookies(u, []*http.Cookie{{Name: b.e.svc.SessionCookie(), Value: token, Path: "/", Secure: !b.e.opts.plainHTTP}})
}

func codeOf(err error) connect.Code {
	return connect.CodeOf(err)
}

func mustTemplates() []*templates.Template {
	ts, err := templates.All()
	Expect(err).NotTo(HaveOccurred())
	return ts
}
