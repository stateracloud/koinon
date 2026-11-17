// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/stateracloud/koinon/internal/adminapi"
	"github.com/stateracloud/koinon/internal/api"
	"github.com/stateracloud/koinon/internal/apiassembly"
	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/auth/oidctest"
	"github.com/stateracloud/koinon/internal/codehost/scopesync"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1/authv1alpha1connect"
	providerv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
	"github.com/stateracloud/koinon/internal/installbinding"
	"github.com/stateracloud/koinon/internal/modelcheck"
	"github.com/stateracloud/koinon/internal/modelformat"
	"github.com/stateracloud/koinon/internal/runwatch"
	"github.com/stateracloud/koinon/internal/sandboxres"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/services"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/tracedest"
	"github.com/stateracloud/koinon/templates"
)

var (
	tmpl  *pgtest.Template
	dbSeq atomic.Int64
)

func TestAPI(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "API Suite")
}

// BeforeSuite migrates a template database once; every spec gets a fresh
// copy, so bootstrap state never leaks between specs.
var _ = BeforeSuite(func(ctx SpecContext) {
	tmpl = pgtest.StartTemplate(ctx)
}, NodeTimeout(3*time.Minute))

// newPool returns a pool on a fresh copy of the migrated template.
func newPool(ctx context.Context) *pgxpool.Pool { return tmpl.NewDB(ctx) }

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
	// binding makes the auth service stamp sessions with an install mark and refuse the ones another
	// install issued (ADR 0110). It gets the env's pool.
	binding                func(*pgxpool.Pool) *installbinding.Binding
	bootstrapLoginDisabled bool
	oidc                   bool
	groupMappings          []auth.GroupMapping
	platformAdminGroups    []string
	defaultOrgMode         store.IdentityMode
	trustedOrigins         []string
	// metrics counts the RPCs, if set.
	metrics *api.Metrics
	// watchPoll is WatchRun's backstop poll interval; tests that check
	// NOTIFY delivery set it long so that polling cannot hide a failure.
	watchPoll time.Duration
	// watchHeartbeat is WatchRun's idle heartbeat interval; 0 keeps the default.
	watchHeartbeat time.Duration
	// watchCancelGrace is how long WatchRun waits for late events of a canceled run; 0 keeps the default.
	watchCancelGrace time.Duration
	// ui is the web UI to serve, if any.
	ui fs.FS
	// keys and github back ConnectionService and the repository binding
	// of CreateAgentFromTemplate. Unset, the env uses a random keyring and the
	// suite's fake GitHub; ghFake names the fake that github talks to, for
	// fixtures that create connections.
	keys   *secretstore.Keyring
	github *github.Client
	ghFake *githubtest.Fake
	noKeys bool
	// scopeSync reads a code-host connection's repositories into the org's code-host scope when it is
	// created, re-keyed or tested, as koinon-api does. Off by default: the reading audits, and runs
	// in the background past the spec.
	scopeSync bool
	// allowedHosts are exempt from the connection base URL policy (a test server on 127.0.0.1).
	allowedHosts []string
	// logs, when set, receives the services' and auth's logs as well.
	logs io.Writer
	// plainHTTP serves over http:// instead of TLS, with a matching PublicURL.
	plainHTTP bool
	// username is the bootstrap admin's login name; empty means the default.
	username string
	// directMessages delivers direct messages through a DBOS queue (TR3), if set.
	directMessages *services.DirectMessageQueue
	// runNow fires schedule triggers by hand through a DBOS queue, if set.
	runNow *services.RunNowQueue
	// modelChat is what the provider echo check sends its request through; nil means the check
	// finds no gateway.
	modelChat modelcheck.Chat
	// settings changes the Settings that Assemble reads, after the suite has filled them.
	settings func(*apiassembly.Settings)
}

// env is one API server on its own database.
type env struct {
	pool     *pgxpool.Pool
	traces   *tracedest.Service
	clock    *clock
	issuer   *oidctest.Issuer
	srv      *httptest.Server
	svc      *auth.Service
	hub      *runwatch.Hub
	rpc      *services.Server
	syncer   *scopesync.Syncer
	pwFile   string
	opts     envOptions
	bootstrp auth.BootstrapResult

	mu    sync.Mutex
	conns map[string]string // org ID -> GitHub connection ID made by githubConnection
}

// suiteGitHub is the fake GitHub of envs that bring none: App 4242,
// installation 777 on account acme with repositories widgets and gadgets.
var suiteGitHub = sync.OnceValue(func() *githubtest.Fake { return githubtest.New() })

func newEnv(ctx context.Context, opts envOptions) *env { return newEnvOn(ctx, nil, opts) }

func timeNow() time.Time { return time.Now() }

// newEnvOn starts an API server. With base, it shares base's database and
// clock, like a restart with new settings.
func newEnvOn(ctx context.Context, base *env, opts envOptions) *env {
	if opts.github == nil {
		opts.ghFake = suiteGitHub()
		opts.github = github.NewClient(github.Options{BaseURL: opts.ghFake.URL()})
	}
	if opts.keys == nil && !opts.noKeys {
		if base != nil && base.opts.keys != nil {
			opts.keys = base.opts.keys
		} else {
			opts.keys = secretstoretest.Keyring()
		}
	}
	e := &env{opts: opts, conns: map[string]string{}}
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

	var logOut io.Writer = GinkgoWriter
	if opts.logs != nil {
		logOut = io.MultiWriter(GinkgoWriter, opts.logs)
	}
	cfg := auth.Config{
		SessionIdleTimeout: idleTimeout,
		SessionMaxLifetime: maxLifetime,
		BootstrapLogin:     !opts.bootstrapLoginDisabled,
		PublicURL:          e.srv.URL,
		Logger:             slog.New(slog.NewTextHandler(logOut, nil)),
		Now:                e.clock.Now,
	}
	if opts.oidc {
		cfg.OIDC = &auth.OIDCConfig{
			IssuerURL:    e.issuer.URL(),
			ClientID:     "koinon",
			ClientSecret: "client-secret",
			RedirectURL:  e.srv.URL + "/auth/oidc/callback",

			PlatformAdminGroups: opts.platformAdminGroups,
		}
	}
	if opts.binding != nil {
		cfg.Binding = opts.binding(e.pool)
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
	var set apiassembly.Settings
	set.EgressAllowedHosts = opts.allowedHosts
	set.WatchPoll, set.WatchHeartbeat, set.WatchCancelGrace = opts.watchPoll, opts.watchHeartbeat, opts.watchCancelGrace
	set.TrustedOrigins = opts.trustedOrigins
	set.Namespace = "koinon"
	set.SandboxResources = sandboxres.DefaultPolicy()
	if opts.settings != nil {
		opts.settings(&set)
	}
	out := apiassembly.Outside{
		Pool: e.pool, Keyring: opts.keys, GitHub: opts.github, NoScopeSync: !opts.scopeSync, ModelChat: opts.modelChat,
		UI: opts.ui, NoUI: opts.ui == nil, Logger: slog.New(slog.NewTextHandler(logOut, nil)),
		Services: []func(*connect.Server){registerProbe},
	}
	if opts.binding != nil {
		out.Binding = cfg.Binding
	}
	asm, err := apiassembly.Assemble(ctx, out, set, apiassembly.Prebuilt{
		Auth: svc, Hub: e.hub, Metrics: opts.metrics, DirectMessages: opts.directMessages, RunNow: opts.runNow,
	})
	Expect(err).NotTo(HaveOccurred())
	e.traces, e.rpc = asm.Traces, asm.Server
	e.syncer = asm.Scope
	handler = asm.Handler
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

// recordingChat answers the echo check and counts what reached it.
type recordingChat struct{ calls atomic.Int32 }

func (c *recordingChat) Complete(context.Context, pgtype.UUID, string, modelformat.Format, string) (string, error) {
	c.calls.Add(1)
	return modelcheck.Line, nil
}

// The suite starts the API through apiassembly.Assemble, as koinon-api does, so what Assemble
// reads from Settings and Outside is covered here (ticket 2 of design-07-compose-api).
var _ = Describe("the assembled API", func() {
	It("refuses a sandbox override outside the bounds of Settings.SandboxResources", func(ctx SpecContext) {
		e := newEnv(ctx, envOptions{settings: func(s *apiassembly.Settings) {
			s.SandboxResources.Bounds.MemoryLimit = sandboxres.Range{Min: "512Mi", Max: "1Gi"}
		}})
		org, owner := e.newTeamOrg(ctx)
		agent := owner.activeAgent(ctx, org)
		set := func(limit string) error {
			_, err := owner.agents.SetAgentSandboxResources(ctx, &agentv1alpha1.SetAgentSandboxResourcesRequest{
				OrgId: org, AgentId: agent.GetId(), Resources: &agentv1alpha1.SandboxResources{MemoryLimit: limit},
			})
			return err
		}
		err := set("2Gi")
		Expect(codeOf(err)).To(Equal(connect.CodeInvalidArgument))
		Expect(err).To(MatchError(ContainSubstring("the platform allows 512Mi to 1Gi")))
		Expect(set("1Gi")).To(Succeed())
	})

	It("refuses the echo check and Approve over HTTP while a restart is held", func(ctx SpecContext) {
		chat := &recordingChat{}
		e := newEnv(ctx, envOptions{modelChat: chat})
		f := e.newOrgFixture(ctx)
		req := e.pendingApproval(ctx, f)
		provider := serveModel(ctx, e, f.org, "fake/faithful", true)

		held := newEnvOn(ctx, e, envOptions{modelChat: chat, settings: func(s *apiassembly.Settings) { s.RestoreHold = true }})
		c := f.owner.on(held)
		_, err := c.providers.CheckProviderEcho(ctx, &providerv1alpha1.CheckProviderEchoRequest{OrgId: f.org, ProviderId: provider, Model: "fake/faithful"})
		Expect(codeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		Expect(chat.calls.Load()).To(BeZero(), "a held install sends nothing to the gateway")
		Expect(approveStatus(ctx, c, f.org, req.ID.String(), req.ArgsSha256)).To(Equal(http.StatusBadRequest))
		var status string
		Expect(e.pool.QueryRow(ctx, `SELECT status FROM approval_requests WHERE id = $1`, req.ID).Scan(&status)).To(Succeed())
		Expect(status).To(Equal(string(store.ApprovalStatusPending)), "the held Approve decided nothing")

		By("the same restart without the hold works")
		open := f.owner.on(newEnvOn(ctx, e, envOptions{modelChat: chat}))
		_, err = open.providers.CheckProviderEcho(ctx, &providerv1alpha1.CheckProviderEchoRequest{OrgId: f.org, ProviderId: provider, Model: "fake/faithful"})
		Expect(err).NotTo(HaveOccurred())
		Expect(chat.calls.Load()).To(Equal(int32(1)))
	})

	It("refuses a token issued under another install binding", func(ctx SpecContext) {
		quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
		claim := func(hash string) func(*pgxpool.Pool) *installbinding.Binding {
			return func(p *pgxpool.Pool) *installbinding.Binding {
				b := installbinding.New(p, hash, false, quiet)
				_, err := b.Claim(ctx, installbinding.Targets{})
				Expect(err).NotTo(HaveOccurred())
				return b
			}
		}
		e := newEnv(ctx, envOptions{binding: claim("hash-source")})
		org, owner := e.newTeamOrg(ctx)
		res, err := owner.tokens.CreateToken(ctx, &tokenv1alpha1.CreateTokenRequest{OrgId: org, Name: "ci", Role: authv1alpha1.OrgRole_ORG_ROLE_ADMIN})
		Expect(err).NotTo(HaveOccurred())
		status := func(e *env) int {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+adminapi.BasePath+"/providers", nil)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+res.GetSecret())
			r, err := e.srv.Client().Do(req)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(r.Body.Close)
			return r.StatusCode
		}
		Expect(status(e)).To(Equal(http.StatusOK), "the install that issued it accepts it")

		By("the database comes back on another install")
		copy := newEnvOn(ctx, e, envOptions{binding: claim("hash-copy")})
		Expect(status(copy)).To(Equal(http.StatusUnauthorized))
	})
})
