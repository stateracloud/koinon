// SPDX-License-Identifier: Apache-2.0

package approvals_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/procoduck/koinon/internal/approvals"
	"github.com/procoduck/koinon/internal/connections"
	"github.com/procoduck/koinon/internal/connectors/github"
	"github.com/procoduck/koinon/internal/connectors/github/githubtest"
	"github.com/procoduck/koinon/internal/db/migrations"
	"github.com/procoduck/koinon/internal/db/store"
	"github.com/procoduck/koinon/internal/gateway"
	"github.com/procoduck/koinon/internal/githubtools"
)

// postgresImage is the minimum supported Postgres version (ADR 0047).
const postgresImage = "postgres:18-alpine"

var (
	pool *pgxpool.Pool
	q    *store.Queries
)

func TestApprovals(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Approvals Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	ctr, err := postgres.Run(ctx, postgresImage, postgres.WithDatabase("koinon"), postgres.BasicWaitStrategies())
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func(ctx context.Context) error { return ctr.Terminate(ctx) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	Expect(err).NotTo(HaveOccurred())
	pool, err = pgxpool.New(ctx, dsn)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	DeferCleanup(db.Close)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	Expect(err).NotTo(HaveOccurred())
	_, err = provider.Up(ctx)
	Expect(err).NotTo(HaveOccurred())
	q = store.New(pool)
}, NodeTimeout(3*time.Minute))

var seq atomic.Int64

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

// memSecrets is an in-memory connections.SecretStore.
type memSecrets struct {
	mu sync.Mutex
	m  map[string]connections.Secret
}

func (s *memSecrets) Create(_ context.Context, sec connections.Secret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]connections.Secret{}
	}
	s.m[sec.Name] = sec
	return nil
}

func (s *memSecrets) Get(_ context.Context, name string) (connections.Secret, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, ok := s.m[name]
	return sec, ok, nil
}

func (s *memSecrets) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
	return nil
}

// fixture is an org with a GitHub connection to the fake, three users, an active agent
// bound to acme/widgets, a ticket created by the owner and its running run, the tool server
// and the approval service.
type fixture struct {
	org                  pgtype.UUID
	owner, admin, member pgtype.UUID
	agent                store.Agent
	ticket               store.Ticket
	run                  store.Run
	agentHeader          string

	gh    *githubtest.Fake
	svc   *approvals.Service
	tools *httptest.Server
	logs  *lockedBuffer
}

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func agentConfig(preset, connID string) string {
	return `{"instructions":"help","model":"fake/model-1",` +
		`"grants":[{"connector":"github","scopes":["repo.read","pull_request.create","issue.comment"],"preset":"` + preset + `"}],` +
		`"bindings":[{"connector":"github","connectionId":"` + connID + `","targets":["acme/widgets"]}]}`
}

func newFixture(ctx context.Context, preset string) *fixture {
	GinkgoHelper()
	f := &fixture{gh: githubtest.New(), logs: &lockedBuffer{}}
	DeferCleanup(f.gh.Server.Close)
	slug := uniq("org")
	user := func(tx pgx.Tx, role store.OrgRole, name string) pgtype.UUID {
		var personal, id pgtype.UUID
		Expect(tx.QueryRow(ctx, `INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'P', 'personal') RETURNING id`, uniq("p")).Scan(&personal)).To(Succeed())
		Expect(tx.QueryRow(ctx,
			`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id, display_name, email)
			 VALUES ('https://idp.test', $1, $2, $3, lower($3) || '@example.test') RETURNING id`,
			uniq("sub"), personal, name).Scan(&id)).To(Succeed())
		_, err := tx.Exec(ctx, `INSERT INTO org_memberships (org_id, user_id, source, role)
			VALUES ($1, $2, 'manual', $3), ($4, $2, 'manual', 'owner')`, f.org, id, role, personal)
		Expect(err).NotTo(HaveOccurred())
		return id
	}
	Expect(pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Team', 'team') RETURNING id`, slug).Scan(&f.org); err != nil {
			return err
		}
		f.owner = user(tx, store.OrgRoleOwner, "Olive")
		f.admin = user(tx, store.OrgRoleAdmin, "Adam")
		f.member = user(tx, store.OrgRoleMember, "Mia")
		return nil
	})).To(Succeed())

	secrets := &memSecrets{}
	connID := uuid.New()
	name := connections.SecretName(f.org.Bytes, connID)
	Expect(secrets.Create(ctx, connections.Secret{Name: name, Data: map[string][]byte{connections.GitHubPrivateKeyField: f.gh.PEM}})).To(Succeed())
	cfg, _ := json.Marshal(connections.GitHubConfig{AppID: f.gh.AppID, InstallationID: f.gh.InstallationID})
	_, err := q.CreateConnection(ctx, store.CreateConnectionParams{
		OrgID: f.org, ID: pgtype.UUID{Bytes: connID, Valid: true}, Connector: "github", DisplayName: "acme",
		Config: cfg, SecretName: name, CreatedBy: f.owner,
	})
	Expect(err).NotTo(HaveOccurred())

	agent, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: f.org, DisplayName: "helper", Flavor: store.AgentFlavorOpencode, CreatedBy: f.owner})
	Expect(err).NotTo(HaveOccurred())
	v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
		OrgID: f.org, AgentID: agent.ID, Config: []byte(agentConfig(preset, connID.String())), CreatedBy: f.owner,
	})
	Expect(err).NotTo(HaveOccurred())
	f.agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: f.org, ID: agent.ID, VersionID: v.ID, ActivatedBy: f.owner})
	Expect(err).NotTo(HaveOccurred())
	f.ticket, err = q.CreateTicket(ctx, store.CreateTicketParams{OrgID: f.org, Title: "fix the typo", AgentID: f.agent.ID, CreatedBy: f.owner})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.CreateRun(ctx, store.CreateRunParams{OrgID: f.org, AgentID: f.agent.ID, TicketID: f.ticket.ID, MaxWallClockSeconds: 1800, MaxTokens: 200_000})
	Expect(err).NotTo(HaveOccurred())
	run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: f.org, OwnerID: "agentd-test", LeaseSeconds: 600})
	Expect(err).NotTo(HaveOccurred())
	f.run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: f.org, ID: run.ID, Epoch: run.Epoch})
	Expect(err).NotTo(HaveOccurred())
	f.agentHeader = slug + "/" + f.agent.ID.String()

	log := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.svc = approvals.New(pool, approvals.Options{
		Secrets: secrets, GitHub: github.NewClient(github.Options{BaseURL: f.gh.URL()}), Logger: log,
		ExecutionLease: 2 * time.Second,
	})
	tools, err := githubtools.New(pool, githubtools.Options{
		Logger: log,
		Execute: func(ctx context.Context, org, id pgtype.UUID) {
			_, _ = f.svc.Execute(ctx, org, id)
		},
	})
	Expect(err).NotTo(HaveOccurred())
	f.tools = httptest.NewServer(tools.Handler())
	DeferCleanup(f.tools.Close)
	return f
}

type agentHeader struct {
	agent string
	base  http.RoundTripper
}

func (a agentHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(gateway.AgentHeader, a.agent)
	return a.base.RoundTrip(r)
}

// call makes a tool call as the agent, through the tool server, and returns its result.
func (f *fixture) call(ctx context.Context, tool string, args any) githubtools.Result {
	GinkgoHelper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "0.0.1"}, nil)
	s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             f.tools.URL,
		HTTPClient:           &http.Client{Transport: agentHeader{agent: f.agentHeader, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = s.Close() }()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	Expect(err).NotTo(HaveOccurred())
	Expect(res.IsError).To(BeFalse(), "%v", res.Content)
	b, err := json.Marshal(res.StructuredContent)
	Expect(err).NotTo(HaveOccurred())
	var out githubtools.Result
	Expect(json.Unmarshal(b, &out)).To(Succeed())
	return out
}

func (f *fixture) get(ctx context.Context, id string) store.ApprovalRequest {
	GinkgoHelper()
	row, err := q.GetApprovalRequest(ctx, store.GetApprovalRequestParams{OrgID: f.org, ID: mustID(id)})
	Expect(err).NotTo(HaveOccurred())
	return row
}

func (f *fixture) ticketEvents(ctx context.Context) []store.TicketEvent {
	GinkgoHelper()
	evs, err := q.ListTicketEvents(ctx, store.ListTicketEventsParams{OrgID: f.org, TicketID: f.ticket.ID, MaxEvents: 100})
	Expect(err).NotTo(HaveOccurred())
	return evs
}

func (f *fixture) getRun(ctx context.Context) store.Run {
	GinkgoHelper()
	r, err := q.GetRun(ctx, store.GetRunParams{OrgID: f.org, ID: f.run.ID})
	Expect(err).NotTo(HaveOccurred())
	return r
}

func (f *fixture) getTicket(ctx context.Context) store.Ticket {
	GinkgoHelper()
	t, err := q.GetTicket(ctx, store.GetTicketParams{OrgID: f.org, ID: f.ticket.ID})
	Expect(err).NotTo(HaveOccurred())
	return t
}

// park parks the run as agentd does when the turn ends with an open request.
func (f *fixture) park(ctx context.Context) {
	GinkgoHelper()
	_, err := q.ParkRun(ctx, store.ParkRunParams{
		OrgID: f.org, ID: f.run.ID, Epoch: f.run.Epoch, ParkReason: store.RunParkReasonApproval,
		ParkExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(7 * 24 * time.Hour), Valid: true},
	})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.MoveTicketFrom(ctx, store.MoveTicketFromParams{
		OrgID: f.org, ID: f.ticket.ID, ToStatus: store.TicketStatusWaitingOnYou,
		FromStatuses: []string{string(store.TicketStatusTodo), string(store.TicketStatusInProgress)},
	})
	Expect(err).NotTo(HaveOccurred())
}

func mustID(s string) pgtype.UUID {
	var id pgtype.UUID
	Expect(id.Scan(s)).To(Succeed())
	return id
}

func kinds(evs []store.TicketEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}
