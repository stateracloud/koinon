// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	guuid "github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/auth"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/db/store"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1/agentv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1/approvalv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1/connectionv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/leakguard/v1alpha1/leakguardv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/mcpserver/v1alpha1/mcpserverv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/orgsettings/v1alpha1/orgsettingsv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/outward/v1alpha1/outwardv1alpha1connect"
	providerv1alpha1connect "github.com/stateracloud/koinon/internal/gen/koinon/provider/v1alpha1/providerv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1/questionv1alpha1connect"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1/runv1alpha1connect"
	"github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1/taskv1alpha1connect"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1/templatev1alpha1connect"
	ticketv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1"
	"github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1/ticketv1alpha1connect"
	tokenv1alpha1connect "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1/tokenv1alpha1connect"
	"github.com/stateracloud/koinon/internal/scheduler"
)

const fixturePassword = "fixture-user-password"

var fixtureHash = sync.OnceValues(func() (string, error) { return auth.HashPassword(fixturePassword) })

// client is a logged-in user with typed clients for every service.
type client struct {
	b           *browser
	userID      string
	agents      agentv1alpha1connect.AgentServiceClient
	tickets     ticketv1alpha1connect.TicketServiceClient
	tasks       taskv1alpha1connect.TaskServiceClient
	runs        runv1alpha1connect.RunServiceClient
	templates   templatev1alpha1connect.TemplateServiceClient
	conns       connectionv1alpha1connect.ConnectionServiceClient
	approvals   approvalv1alpha1connect.ApprovalServiceClient
	tokens      tokenv1alpha1connect.TokenServiceClient
	providers   providerv1alpha1connect.ProviderServiceClient
	mcps        mcpserverv1alpha1connect.McpServerServiceClient
	questions   questionv1alpha1connect.QuestionServiceClient
	leakguard   leakguardv1alpha1connect.LeakGuardServiceClient
	orgsettings orgsettingsv1alpha1connect.OrgSettingsServiceClient
	outward     outwardv1alpha1connect.OutwardServiceClient
}

func newClient(b *browser) *client {
	cc := connect.NewClient(connecthttp.NewTransport(b.http, b.e.srv.URL))
	return &client{
		b:           b,
		agents:      agentv1alpha1connect.NewAgentServiceClient(cc),
		tickets:     ticketv1alpha1connect.NewTicketServiceClient(cc),
		tasks:       taskv1alpha1connect.NewTaskServiceClient(cc),
		runs:        runv1alpha1connect.NewRunServiceClient(cc),
		templates:   templatev1alpha1connect.NewTemplateServiceClient(cc),
		conns:       connectionv1alpha1connect.NewConnectionServiceClient(cc),
		approvals:   approvalv1alpha1connect.NewApprovalServiceClient(cc),
		tokens:      tokenv1alpha1connect.NewTokenServiceClient(cc),
		providers:   providerv1alpha1connect.NewProviderServiceClient(cc),
		mcps:        mcpserverv1alpha1connect.NewMcpServerServiceClient(cc),
		questions:   questionv1alpha1connect.NewQuestionServiceClient(cc),
		leakguard:   leakguardv1alpha1connect.NewLeakGuardServiceClient(cc),
		orgsettings: orgsettingsv1alpha1connect.NewOrgSettingsServiceClient(cc),
		outward:     outwardv1alpha1connect.NewOutwardServiceClient(cc),
	}
}

// on returns a client for the same session against another server sharing
// the database, as after a reconnect to another API replica.
func (c *client) on(e *env) *client {
	b := &browser{e: e, http: c.b.http}
	n := newClient(b)
	n.userID = c.userID
	return n
}

// newTeamOrg creates a team org and returns it with a logged-in owner.
func (e *env) newTeamOrg(ctx context.Context) (string, *client) {
	var org string
	var owner *client
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Team', 'team') RETURNING id`,
			fmt.Sprintf("team-%d", dbSeq.Add(1))).Scan(&org); err != nil {
			return err
		}
		var err error
		owner, err = e.createUser(ctx, tx, org, store.OrgRoleOwner)
		return err
	})
	Expect(err).NotTo(HaveOccurred())
	owner.login(ctx)
	return org, owner
}

// userIn returns a logged-in new user holding role in org.
func (e *env) userIn(ctx context.Context, org string, role store.OrgRole) *client {
	var c *client
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		var err error
		c, err = e.createUser(ctx, tx, org, role)
		return err
	})
	Expect(err).NotTo(HaveOccurred())
	c.login(ctx)
	return c
}

func (e *env) createUser(ctx context.Context, tx pgx.Tx, org string, role store.OrgRole) (*client, error) {
	hash, err := fixtureHash()
	if err != nil {
		return nil, err
	}
	n := dbSeq.Add(1)
	var personal, user string
	if err := tx.QueryRow(ctx,
		`INSERT INTO orgs (slug, display_name, kind) VALUES ($1, 'Personal', 'personal') RETURNING id`,
		fmt.Sprintf("p-%d", n)).Scan(&personal); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO users (oidc_issuer, oidc_subject, personal_org_id, display_name, email)
		 VALUES ('https://idp.test', $1, $2, $1, $1 || '@example.test') RETURNING id`,
		fmt.Sprintf("user-%d", n), personal).Scan(&user); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO org_memberships (org_id, user_id, source, role) VALUES ($1, $2, 'manual', 'owner'), ($3, $2, 'manual', $4)`,
		personal, user, org, role); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO local_credentials (user_id, username, password_hash, must_change_password) VALUES ($1, $2, $3, false)`,
		user, fmt.Sprintf("user-%d", n), hash); err != nil {
		return nil, err
	}
	c := newClient(e.browser())
	c.userID = user
	return c, nil
}

func (c *client) login(ctx context.Context) {
	var username string
	Expect(c.b.e.pool.QueryRow(ctx, `SELECT username FROM local_credentials WHERE user_id = $1`, c.userID).
		Scan(&username)).To(Succeed())
	Expect(c.b.login(ctx, username, fixturePassword).StatusCode).To(Equal(http.StatusOK)) //nolint:bodyclose // login closes it at cleanup
}

// repoHelper returns the current repo-helper template.
func (c *client) repoHelper(ctx context.Context) *templatev1alpha1.Template {
	res, err := c.templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{})
	Expect(err).NotTo(HaveOccurred())
	for _, t := range res.GetTemplates() {
		if t.GetSlug() == "repo-helper" {
			return t
		}
	}
	Fail("repo-helper template not listed")
	return nil
}

// draftAgent creates a draft agent from the repo-helper template, bound to
// acme/widgets through the org's fixture GitHub connection.
func (c *client) draftAgent(ctx context.Context, org string) *agentv1alpha1.Agent {
	res, err := c.agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
		OrgId: org, TemplateId: c.repoHelper(ctx).GetId(), DisplayName: "Repo helper",
		Grants: c.b.e.repoGrant(ctx, org, "acme/widgets"),
	})
	Expect(err).NotTo(HaveOccurred())
	return res.GetAgent()
}

// repoGrant binds the repo-helper template's GitHub grant to repos through
// the org's fixture connection.
func (e *env) repoGrant(ctx context.Context, org string, repos ...string) []*agentv1alpha1.GrantBinding {
	return []*agentv1alpha1.GrantBinding{{Connector: "github", ConnectionId: e.githubConnection(ctx, org), Repositories: repos}}
}

// githubConnection returns the org's fixture GitHub connection to the env's
// fake GitHub, creating it (row and key Secret, as CreateGitHubConnection
// does) on first use. It bypasses the API, so any test user can use it.
func (e *env) githubConnection(ctx context.Context, org string) string {
	GinkgoHelper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if id, ok := e.conns[org]; ok {
		return id
	}
	Expect(e.opts.keys).NotTo(BeNil(), "this env has no secret store")
	var orgID pgtype.UUID
	Expect(orgID.Scan(org)).To(Succeed())
	id := pgtype.UUID{Bytes: guuid.New(), Valid: true}
	gh := e.opts.ghFake
	Expect(gh).NotTo(BeNil(), "envOptions.github is set without ghFake")
	envelope, err := connections.SealFields(e.opts.keys, orgID, id, map[string][]byte{connections.GitHubPrivateKeyField: gh.PEM})
	Expect(err).NotTo(HaveOccurred())
	cfg := fmt.Sprintf(`{"appId": %d, "installationId": %d}`, gh.AppID, gh.InstallationID)
	conn, err := store.New(e.pool).CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
		OrgID: orgID, ID: id, Connector: "github", DisplayName: "acme", Config: []byte(cfg),
		SecretEnvelope: envelope, Source: store.ConfigSourceUi,
	})
	Expect(err).NotTo(HaveOccurred())
	e.conns[org] = conn.ID.String()
	return e.conns[org]
}

// activeAgent creates and activates an agent; c must be an admin or owner.
func (c *client) activeAgent(ctx context.Context, org string) *agentv1alpha1.Agent {
	a := c.draftAgent(ctx, org)
	res, err := c.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
		OrgId: org, AgentId: a.GetId(), Version: a.GetLatestVersion(),
	})
	Expect(err).NotTo(HaveOccurred())
	return res.GetAgent()
}

func (c *client) createTicket(ctx context.Context, org, agent, title string) (*ticketv1alpha1.Ticket, *runv1alpha1.Run) {
	res, err := c.tickets.CreateTicket(ctx, &ticketv1alpha1.CreateTicketRequest{
		OrgId: org, AgentId: agent, Title: title, Message: "please " + title,
	})
	Expect(err).NotTo(HaveOccurred())
	return res.GetTicket(), res.GetRun()
}

func (c *client) getTicket(ctx context.Context, org, ticket string) *ticketv1alpha1.GetTicketResponse {
	res, err := c.tickets.GetTicket(ctx, &ticketv1alpha1.GetTicketRequest{OrgId: org, TicketId: ticket})
	Expect(err).NotTo(HaveOccurred())
	return res
}

func (c *client) getRun(ctx context.Context, org, run string) *runv1alpha1.Run {
	res, err := c.runs.GetRun(ctx, &runv1alpha1.GetRunRequest{OrgId: org, RunId: run})
	Expect(err).NotTo(HaveOccurred())
	return res.GetRun()
}

// orgFixture is one org with an owner, an active agent, a ticket and its
// queued run, all made through the API.
type orgFixture struct {
	org    string
	owner  *client
	agent  *agentv1alpha1.Agent
	draft  *agentv1alpha1.Agent // set by newIsolationFixture
	ticket *ticketv1alpha1.Ticket
	run    *runv1alpha1.Run
	// connection and appKey are set by newIsolationFixture.
	connection string
	appKey     []byte
	// approval is a pending request of run; set by newIsolationFixture.
	approval store.ApprovalRequest
	// question is an open question of run to the owner; set by newIsolationFixture.
	question store.Question
	// token is an admin API token of the org; set by newIsolationFixture.
	token string
	// provider and model are a provider and a model of the org, for the isolation suite;
	// modelName is the model's name, unique across orgs.
	provider, model, modelName string
	// mcpServer is a remote MCP server of the org, with the tool "search"; mcpTool is its name.
	mcpServer, mcpTool string
	// traceDest is a trace destination of the org.
	traceDest string
	// trustedHost is on the org's fetch-trusted list; the org's match threshold is 30.
	trustedHost string
}

func (e *env) newOrgFixture(ctx context.Context) orgFixture {
	var f orgFixture
	f.org, f.owner = e.newTeamOrg(ctx)
	f.agent = f.owner.activeAgent(ctx, f.org)
	f.ticket, f.run = f.owner.createTicket(ctx, f.org, f.agent.GetId(), "fix the README")
	Expect(f.run).NotTo(BeNil())
	return f
}

// agentd stands in for koinon-agentd: it claims runs and writes events as
// their owner, straight through the store.
type agentd struct {
	q  *store.Queries
	id string
}

func (e *env) agentd(name string) *agentd { return &agentd{q: store.New(e.pool), id: name} }

func (a *agentd) claim(ctx context.Context, org string) (store.Run, error) {
	return a.q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: uuid(org), OwnerID: a.id, LeaseSeconds: 60})
}

func (a *agentd) mustClaim(ctx context.Context, org string) store.Run {
	r, err := a.claim(ctx, org)
	Expect(err).NotTo(HaveOccurred())
	r, err = a.q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: r.OrgID, ID: r.ID, Epoch: r.Epoch})
	Expect(err).NotTo(HaveOccurred())
	return r
}

func (a *agentd) event(ctx context.Context, r store.Run, n int) store.RunEvent {
	ev, err := a.q.AppendRunEvent(ctx, store.AppendRunEventParams{
		OrgID: r.OrgID, RunID: r.ID, Epoch: r.Epoch, Kind: "session.update",
		Payload: fmt.Appendf(nil, `{"n":%d}`, n),
	})
	Expect(err).NotTo(HaveOccurred())
	return ev
}

// item stores an event of an item, as the Owner does: the payload names the item ("item").
func (a *agentd) item(ctx context.Context, r store.Run, name string) store.RunEvent {
	ev, err := a.q.AppendRunEvent(ctx, store.AppendRunEventParams{
		OrgID: r.OrgID, RunID: r.ID, Epoch: r.Epoch, Kind: "acp.agent_message_chunk",
		Payload: fmt.Appendf(nil, `{"content":{"type":"text","text":"tail"},"item":%q}`, name),
	})
	Expect(err).NotTo(HaveOccurred())
	return ev
}

func (a *agentd) finish(ctx context.Context, r store.Run) {
	_, err := a.q.FinishRun(ctx, store.FinishRunParams{
		OrgID: r.OrgID, ID: r.ID, Epoch: r.Epoch, Outcome: store.RunStateSucceeded,
	})
	Expect(err).NotTo(HaveOccurred())
}

func (a *agentd) park(ctx context.Context, r store.Run) store.Run {
	r, err := a.q.ParkRun(ctx, store.ParkRunParams{
		OrgID: r.OrgID, ID: r.ID, Epoch: r.Epoch, ParkReason: store.RunParkReasonApproval,
	})
	Expect(err).NotTo(HaveOccurred())
	return r
}

func uuid(s string) (id pgtype.UUID) {
	Expect(id.Scan(s)).To(Succeed())
	return id
}

func ticketKinds(res *ticketv1alpha1.GetTicketResponse) []string {
	var kinds []string
	for _, e := range res.GetEvents() {
		kinds = append(kinds, e.GetKind())
	}
	return kinds
}

func sweep(ctx context.Context, e *env) (int, error) { return scheduler.SweepWallClock(ctx, e.pool) }

func recordTokens(ctx context.Context, e *env, r store.Run, tokens int64) (store.Run, error) {
	return scheduler.RecordUsage(ctx, e.pool, r.OrgID, r.ID, tokens)
}

func mustUUID(s string) pgtype.UUID {
	var id pgtype.UUID
	Expect(id.Scan(s)).To(Succeed())
	return id
}
