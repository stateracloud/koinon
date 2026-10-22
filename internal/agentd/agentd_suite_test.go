// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/procoduck/koinon/internal/agentd"
	"github.com/procoduck/koinon/internal/db/migrations"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/execution"
	"github.com/procoduck/koinon/internal/execution/openshell"
	"github.com/procoduck/koinon/internal/execution/openshell/fakegateway"
)

// postgresImage is the minimum supported Postgres version (ADR 0047).
const postgresImage = "postgres:18-alpine"

var (
	db *pgxpool.Pool
	q  *store.Queries
)

func TestAgentd(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "agentd")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	ctr, err := postgres.Run(ctx, postgresImage, postgres.WithDatabase("koinon"), postgres.BasicWaitStrategies())
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) error { return ctr.Terminate(ctx) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	Expect(err).NotTo(HaveOccurred())
	db, err = pgxpool.New(ctx, dsn)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(db.Close)

	sqlDB := stdlib.OpenDBFromPool(db)
	DeferCleanup(sqlDB.Close)
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	Expect(err).NotTo(HaveOccurred())
	_, err = provider.Up(ctx)
	Expect(err).NotTo(HaveOccurred())
	q = store.New(db)
}, NodeTimeout(3*time.Minute))

var seq atomic.Int64

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

const (
	gatewayURL = "http://gateway.koinon.test:8080"
	model      = "fake/model-1"
)

// env is one org with an active agent, its sandbox in a fake OpenShell gateway whose exec
// runs the fake harness, and helpers to create tickets and runs. Pools scan every org, so
// an env stops its pools and cancels its unfinished runs when the spec ends.
type env struct {
	org, user pgtype.UUID
	agent     store.Agent
	gw        *fakegateway.Gateway
	backend   *openshell.Backend
	harness   *harness
	workspace string
	sandbox   string
}

func newEnv(ctx context.Context, flavor store.AgentFlavor) *env {
	return newEnvWith(ctx, flavor, nil)
}

// newEnvWith is newEnv with the agent version's config from configFor, which runs once the
// org and user exist.
func newEnvWith(ctx context.Context, flavor store.AgentFlavor, configFor func(*env) string) *env {
	e := &env{}
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Personal', 'personal') RETURNING id`,
			uniq("org")).Scan(&e.org); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id) VALUES ('https://idp.test', $1, $2) RETURNING id`,
			uniq("sub"), e.org).Scan(&e.user); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner')`, e.org, e.user)
		return err
	})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) {
		_, err := db.Exec(ctx, `UPDATE runs SET state = 'canceled', outcome_reason = 'spec ended',
			finished_at = now(), ready_at = NULL, lease_expires_at = NULL, heartbeat_at = NULL
			WHERE org_id = $1 AND state NOT IN ('succeeded', 'failed', 'canceled')`, e.org)
		Expect(err).NotTo(HaveOccurred())
	})

	agent, err := q.CreateAgent(ctx, store.CreateAgentParams{
		OrgID: e.org, DisplayName: uniq("agent"), Flavor: flavor, CreatedBy: e.user,
	})
	Expect(err).NotTo(HaveOccurred())
	config := `{"model":{"name":"` + model + `"}}`
	if configFor != nil {
		config = configFor(e)
	}
	version, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
		OrgID: e.org, AgentID: agent.ID, Config: []byte(config), CreatedBy: e.user,
	})
	Expect(err).NotTo(HaveOccurred())
	e.agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{
		OrgID: e.org, ID: agent.ID, VersionID: version.ID, ActivatedBy: e.user,
	})
	Expect(err).NotTo(HaveOccurred())

	e.gw = fakegateway.New()
	conn, err := e.gw.Start()
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(e.gw.Stop)
	DeferCleanup(conn.Close)
	e.backend = openshell.New(conn)
	e.workspace, e.sandbox = execution.WorkspaceName(e.org.Bytes), execution.AgentSandboxName(e.org.Bytes, e.agent.ID.Bytes)
	_, err = e.backend.EnsureWorkspace(ctx, e.workspace, nil)
	Expect(err).NotTo(HaveOccurred())
	_, err = e.backend.CreateSandbox(ctx, e.workspace, execution.SandboxSpec{Name: e.sandbox, Image: "koinon/flavor-opencode:dev"})
	Expect(err).NotTo(HaveOccurred())
	// What koinon-reconciler records for the sandbox, which is all agentd reads about it.
	_, err = q.UpsertOrgWorkspace(ctx, store.UpsertOrgWorkspaceParams{OrgID: e.org, Workspace: e.workspace})
	Expect(err).NotTo(HaveOccurred())
	names := execution.AgentObjectNames(e.org.Bytes, e.agent.ID.Bytes)
	_, err = q.CreateAgentSandbox(ctx, store.CreateAgentSandboxParams{
		OrgID: e.org, AgentID: e.agent.ID, Workspace: e.workspace,
		SandboxName: names.Sandbox, ProviderName: names.Provider, KeySecretName: names.KeySecret,
	})
	Expect(err).NotTo(HaveOccurred())
	e.recordSandbox(ctx, store.SandboxStateReady, "")

	e.harness = newHarness()
	for _, script := range []string{"/usr/local/bin/koinon-opencode", "/usr/local/bin/koinon-hermes", "/usr/local/bin/koinon-pi"} {
		e.gw.Handle(script, e.harness.command)
	}
	e.gw.Handle("/bin/mkdir", func(context.Context, []string, io.Reader, io.Writer, io.Writer) int { return 0 })
	return e
}

// recordSandbox writes the sandbox state as koinon-reconciler would.
func (e *env) recordSandbox(ctx context.Context, state store.SandboxState, message string) {
	_, err := q.RecordAgentSandbox(ctx, store.RecordAgentSandboxParams{
		OrgID: e.org, AgentID: e.agent.ID, State: state, Phase: "", Message: message,
	})
	Expect(err).NotTo(HaveOccurred())
}

// ticket creates a ticket for the agent with one user message.
func (e *env) ticket(ctx context.Context, message string) store.Ticket {
	t, err := q.CreateTicket(ctx, store.CreateTicketParams{
		OrgID: e.org, Title: uniq("ticket"), AgentID: e.agent.ID, CreatedBy: e.user,
	})
	Expect(err).NotTo(HaveOccurred())
	e.message(ctx, t, message)
	return t
}

func (e *env) message(ctx context.Context, t store.Ticket, text string) {
	_, err := q.AppendTicketEvent(ctx, store.AppendTicketEventParams{
		OrgID: e.org, TicketID: t.ID, Kind: "message", ActorKind: store.TicketActorKindUser,
		ActorUserID: e.user, Payload: []byte(fmt.Sprintf(`{"text":%q}`, text)),
	})
	Expect(err).NotTo(HaveOccurred())
}

func (e *env) enqueue(ctx context.Context, t store.Ticket) store.Run {
	r, err := q.CreateRun(ctx, store.CreateRunParams{
		OrgID: e.org, AgentID: e.agent.ID, TicketID: t.ID, MaxWallClockSeconds: 1800, MaxTokens: 100_000,
	})
	Expect(err).NotTo(HaveOccurred())
	return r
}

// pool starts a pool on the env's gateway and stops it when the spec ends. The returned
// function stops it early, like a killed pod when the pool keeps its leases.
func (e *env) pool(name string, mutate ...func(*agentd.Config)) (stop func()) {
	cfg := agentd.Config{
		OwnerID:        name,
		Workers:        2,
		Lease:          2 * time.Second,
		Heartbeat:      200 * time.Millisecond,
		Poll:           50 * time.Millisecond,
		GatewayURL:     gatewayURL,
		Logger:         slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug})).With("pool", name),
		CancelGrace:    2 * time.Second,
		SandboxTimeout: 10 * time.Second,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	p, err := agentd.New(db, e.backend, cfg)
	Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(done)
		Expect(p.Run(ctx)).To(Succeed())
	}()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			Eventually(done).WithTimeout(10 * time.Second).Should(BeClosed())
		}
	}
	DeferCleanup(stop)
	return stop
}

func getRun(ctx context.Context, r store.Run) store.Run {
	got, err := q.GetRun(ctx, store.GetRunParams{OrgID: r.OrgID, ID: r.ID})
	Expect(err).NotTo(HaveOccurred())
	return got
}

func runState(ctx context.Context, r store.Run) func() store.RunState {
	return func() store.RunState { return getRun(ctx, r).State }
}

func events(ctx context.Context, r store.Run) []store.RunEvent {
	evs, err := q.ListRunEventsAfter(ctx, store.ListRunEventsAfterParams{OrgID: r.OrgID, RunID: r.ID, MaxEvents: 1000})
	Expect(err).NotTo(HaveOccurred())
	return evs
}

func kinds(evs []store.RunEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func ticketEvents(ctx context.Context, e *env, t store.Ticket) []store.TicketEvent {
	evs, err := q.ListTicketEvents(ctx, store.ListTicketEventsParams{OrgID: e.org, TicketID: t.ID, MaxEvents: 100})
	Expect(err).NotTo(HaveOccurred())
	return evs
}
