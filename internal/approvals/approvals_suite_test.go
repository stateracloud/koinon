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
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/codehost"
	"github.com/stateracloud/koinon/internal/codehost/providers"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
)

var (
	pool *pgxpool.Pool
	q    *store.Queries
)

func TestApprovals(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Approvals Suite")
}

var _ = BeforeSuite(func(ctx SpecContext) {
	pool = pgtest.Start(ctx)
	q = store.New(pool)
}, NodeTimeout(3*time.Minute))

var seq atomic.Int64

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, seq.Add(1)) }

// fixture is an org with a GitHub connection to the fake, three users, an active agent
// bound to acme/widgets, a task created by the owner and its running run, the tool server
// and the approval service.
type fixture struct {
	org                  pgtype.UUID
	owner, admin, member pgtype.UUID
	agent                store.Agent
	task                 store.Task
	run                  store.Run
	agentHeader          string

	ring  *secretstore.Keyring
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
	return newFixtureOn(ctx, preset, githubtest.New(), false)
}

// newGHESFixture is newFixture with a connection to a GitHub Enterprise Server: the fake serves
// TLS with a certificate no system root signs and the REST API under /api/v3, the connection
// carries its base URL and CA bundle, and the service's own client points at api.github.com,
// which no test may reach: only the connection can send a call to the fake.
func newGHESFixture(ctx context.Context, preset string) *fixture {
	GinkgoHelper()
	return newFixtureOn(ctx, preset, githubtest.NewGHES(), true)
}

func newFixtureOn(ctx context.Context, preset string, gh *githubtest.Fake, ghes bool) *fixture {
	GinkgoHelper()
	f := &fixture{gh: gh, logs: &lockedBuffer{}}
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

	ring := secretstoretest.Keyring()
	f.ring = ring
	connID := uuid.New()
	connPG := pgtype.UUID{Bytes: connID, Valid: true}
	env, err := connections.SealFields(ring, f.org, connPG, map[string][]byte{connections.GitHubPrivateKeyField: f.gh.PEM})
	Expect(err).NotTo(HaveOccurred())
	gc := connections.GitHubConfig{AppID: f.gh.AppID, InstallationID: f.gh.InstallationID}
	newClient := func() *github.Client { return github.NewClient(github.Options{BaseURL: f.gh.URL()}) }
	if ghes {
		newClient = func() *github.Client { return github.NewClient(github.Options{}) }
		gc.Site = codehost.Site{BaseURL: f.gh.WebURL(), CABundle: f.gh.CAPEM()}
	}
	client := newClient()
	cfg, _ := json.Marshal(gc)
	_, err = q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
		OrgID: f.org, ID: connPG, Connector: "github", DisplayName: "acme",
		Config: cfg, SecretEnvelope: env, Source: store.ConfigSourceUi, CreatedBy: f.owner,
	})
	Expect(err).NotTo(HaveOccurred())

	agent, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: f.org, DisplayName: "helper", Harness: harness.OpenCode, CreatedBy: f.owner})
	Expect(err).NotTo(HaveOccurred())
	v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{
		OrgID: f.org, AgentID: agent.ID, Config: []byte(agentConfig(preset, connID.String())), CreatedBy: f.owner,
	})
	Expect(err).NotTo(HaveOccurred())
	f.agent, err = q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: f.org, ID: agent.ID, VersionID: v.ID, ActivatedBy: f.owner})
	Expect(err).NotTo(HaveOccurred())
	f.task, err = q.CreateTask(ctx, store.CreateTaskParams{OrgID: f.org, Title: "fix the typo", AgentID: f.agent.ID, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: f.owner})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.CreateRun(ctx, store.CreateRunParams{OrgID: f.org, AgentID: f.agent.ID, TaskID: f.task.ID, MaxWallClockSeconds: 1800})
	Expect(err).NotTo(HaveOccurred())
	run, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: f.org, OwnerID: "agentd-test", LeaseSeconds: 600})
	Expect(err).NotTo(HaveOccurred())
	f.run, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: f.org, ID: run.ID, Epoch: run.Epoch})
	Expect(err).NotTo(HaveOccurred())
	f.agentHeader = slug + "/" + f.agent.ID.String()

	log := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.svc = approvals.New(pool, approvals.Options{
		Keys: ring, SavedWorkKeys: ring, GitHub: client, Logger: log,
		ExecutionLease: 2 * time.Second,
	})
	tools, err := githubtools.New(pool, githubtools.Options{
		Logger: log, Keys: ring, Hosts: providers.New(newClient()), // its own token cache, as in koinon-api's separate tool server
		Execute: func(ctx context.Context, org, id pgtype.UUID) {
			_, _ = f.svc.Execute(ctx, org, id)
		},
		LeakCheck: cleanLeakCheck,
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

// callRefused makes a tool call the server refuses and returns the reason.
func (f *fixture) callRefused(ctx context.Context, tool string, args any) string {
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
	Expect(res.IsError).To(BeTrue())
	b, _ := json.Marshal(res.Content)
	return string(b)
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
	// Recording a pull request checks the branch with a read token of its own; the specs count
	// what the executor does, so forget that.
	f.gh.ResetTokens()
	return out
}

// branch is the branch Koinon files the agent's suggestion under (ADR 0062).
func (f *fixture) branch(name string) string {
	return githubtools.BranchPrefix + githubtools.AgentNamespace("helper", f.agent.ID.String()) + "/" + name
}

func (f *fixture) get(ctx context.Context, id string) store.ApprovalRequest {
	GinkgoHelper()
	row, err := q.GetApprovalRequest(ctx, store.GetApprovalRequestParams{OrgID: f.org, ID: mustID(id)})
	Expect(err).NotTo(HaveOccurred())
	return row
}

func (f *fixture) taskEvents(ctx context.Context) []store.TaskEvent {
	GinkgoHelper()
	evs, err := q.ListTaskEvents(ctx, store.ListTaskEventsParams{OrgID: f.org, TaskID: f.task.ID, MaxEvents: 100})
	Expect(err).NotTo(HaveOccurred())
	return evs
}

// decisions returns the approval audit events of the org, oldest first, as "action actor".
func (f *fixture) decisions(ctx context.Context) []string {
	GinkgoHelper()
	rows, err := pool.Query(ctx, `SELECT action || ' ' || actor_kind || ' ' || actor_id FROM audit_events
		WHERE org_id = $1 AND action LIKE 'approval.%' ORDER BY chain_seq`, f.org)
	Expect(err).NotTo(HaveOccurred())
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	Expect(err).NotTo(HaveOccurred())
	return out
}

func (f *fixture) getRun(ctx context.Context) store.Run {
	GinkgoHelper()
	r, err := q.GetRun(ctx, store.GetRunParams{OrgID: f.org, ID: f.run.ID})
	Expect(err).NotTo(HaveOccurred())
	return r
}

func (f *fixture) getTask(ctx context.Context) store.Task {
	GinkgoHelper()
	t, err := q.GetTask(ctx, store.GetTaskParams{OrgID: f.org, ID: f.task.ID})
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
	_, err = q.SetTaskStateFrom(ctx, store.SetTaskStateFromParams{
		OrgID: f.org, ID: f.task.ID, ToState: store.TaskStateInputRequired,
		FromStates: []string{string(store.TaskStateSubmitted), string(store.TaskStateWorking)},
	})
	Expect(err).NotTo(HaveOccurred())
}

func mustID(s string) pgtype.UUID {
	var id pgtype.UUID
	Expect(id.Scan(s)).To(Succeed())
	return id
}

func kinds(evs []store.TaskEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

// cleanLeakCheck stands in for the leak guard (#687): these specs cover how an unattended call
// runs, so every call is clean. The guard's own verdicts are specified in internal/mcpguard.
func cleanLeakCheck(context.Context, githubtools.LeakCall) (map[string]any, error) { return nil, nil }
