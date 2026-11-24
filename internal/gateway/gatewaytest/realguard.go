// SPDX-License-Identifier: Apache-2.0

package gatewaytest

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2" //nolint:staticcheck // the Ginkgo DSL is dot-imported, as in every spec file
	. "github.com/onsi/gomega"    //nolint:staticcheck // see above
	"google.golang.org/grpc"

	"github.com/stateracloud/koinon/internal/audit"
	"github.com/stateracloud/koinon/internal/catalog"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/execution"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gen/agentgateway/extmcp"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/mcpask"
	"github.com/stateracloud/koinon/internal/mcpguard"
	"github.com/stateracloud/koinon/internal/mcpservers"
	"github.com/stateracloud/koinon/internal/mcpservers/mcpserverstest"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/sinkclass"
	"github.com/stateracloud/koinon/internal/testing/pgtest"
	"github.com/stateracloud/koinon/internal/testing/testenv"
)

// RealGuard is koinon-api's real MCP leak guard (internal/mcpguard.Server) on a pgtest Postgres,
// served as the gateway's ExtMcp processor on the test host, with one org, one agent that has a
// ready sandbox and a run, and a way to declare the org's MCP servers.
type RealGuard struct {
	Pool  *pgxpool.Pool
	Org   pgtype.UUID
	User  pgtype.UUID
	Slug  string
	Agent store.Agent

	svc   *mcpservers.Service
	ids   map[string]string // server name to ID, for Declare
	port  int
	srv   *grpc.Server
	local *mcpguard.GatewayGuard
}

// AgentRef is the agent as the gateway's keys and the guard's metadata name it.
func (g *RealGuard) AgentRef() gateway.AgentRef {
	return gateway.AgentRef{Org: g.Slug, Name: g.Agent.ID.String()}
}

// Port is the guard's port on the test host.
func (g *RealGuard) Port() int { return g.port }

// URL is the guard's address for Options.GuardURL, as a gateway reaches it on host.
func (g *RealGuard) URL(host string) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(g.port))
}

// Ref is the guard in both forms: the ExtMcp server at host, and the adapter koinon-gateway runs in its
// process (mcpguard.GatewayGuard) on the same guard and database.
func (g *RealGuard) Ref(host string) Guard { return Guard{URL: g.URL(host), Local: g.local} }

// Target is the gateway target name of the org's server.
func (g *RealGuard) Target(server string) string { return mcpservers.TargetName(g.Slug, server) }

// Server registers the org's MCP server with the tools, each on the auto tier, and declares its
// data class: legs are mcpservers.LegPrivateData and mcpservers.LegUntrustedInput, none for a
// server whose results set neither leg. A tool nobody declared is external-high.
//
// The tools are synced as srv publishes them, as the prober does (internal/reconciler): the approved
// hash is then the hash of the text a gateway lists, so a gateway that pins tools (#1132) serves them.
func (g *RealGuard) Server(ctx context.Context, name string, srv *httptest.Server, legs []string, tools ...string) {
	GinkgoHelper()
	s, err := g.svc.Create(ctx, g.Org, audit.System("koinon-test:realguard"), g.User, mcpservers.Input{Name: name, URL: "https://" + name + ".example.com/mcp"})
	Expect(err).NotTo(HaveOccurred())
	g.ids[name] = s.ID
	published := publishedTools(ctx, srv)
	var listed []mcpservers.ListedTool
	for _, t := range tools {
		lt, ok := published[t]
		Expect(ok).To(BeTrue(), "the server does not publish the tool %q", t)
		listed = append(listed, lt)
	}
	_, err = g.svc.SyncTools(ctx, g.Org, uuidOf(s.ID), listed)
	Expect(err).NotTo(HaveOccurred())
	for _, t := range tools {
		_, err = g.svc.SetTier(ctx, g.Org, audit.System("koinon-test:realguard"), s.ID, t, "auto", mcpserverstest.PinOf(ctx, g.svc, g.Org, s.ID, t))
		Expect(err).NotTo(HaveOccurred())
	}
	_, err = g.svc.SetDataClass(ctx, g.Org, audit.System("koinon-test:realguard"), s.ID, mcpservers.Declaration{Kind: mcpservers.KindOther, Legs: append([]string{}, legs...)})
	Expect(err).NotTo(HaveOccurred())
}

// Declare sets the declaration of one tool of the org's server, as an admin does after review
// (ADR 0091 decision 3): its sink class, how the class is derived and its address arguments.
func (g *RealGuard) Declare(ctx context.Context, server, tool string, d sinkclass.Declaration) {
	GinkgoHelper()
	id, ok := g.ids[server]
	Expect(ok).To(BeTrue(), "no server %q: call Server first", server)
	tools, err := g.svc.Tools(ctx, g.Org, id)
	Expect(err).NotTo(HaveOccurred())
	var hash string
	for _, t := range tools {
		if t.Name == tool {
			hash = t.ContentHash
		}
	}
	Expect(hash).NotTo(BeEmpty(), "the server %q has no tool %q", server, tool)
	_, err = g.svc.SetToolDeclaration(ctx, g.Org, audit.System("koinon-test:realguard"), id, tool, hash, d)
	Expect(err).NotTo(HaveOccurred())
}

// publishedTools lists srv's tools directly, by name, in the form the prober syncs them.
func publishedTools(ctx context.Context, srv *httptest.Server) map[string]mcpservers.ListedTool {
	GinkgoHelper()
	client := mcp.NewClient(&mcp.Implementation{Name: "koinon-test-realguard", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = session.Close() }()
	res, err := session.ListTools(ctx, nil)
	Expect(err).NotTo(HaveOccurred())
	raw := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		Expect(err).NotTo(HaveOccurred())
		return b
	}
	out := map[string]mcpservers.ListedTool{}
	for _, t := range res.Tools {
		lt := mcpservers.ListedTool{Name: t.Name, Description: t.Description, Title: t.Title, InputSchema: raw(t.InputSchema)}
		if t.Annotations != nil {
			lt.ReadOnlyHint = t.Annotations.ReadOnlyHint
			lt.Annotations = raw(t.Annotations)
		}
		if t.OutputSchema != nil {
			lt.OutputSchema = raw(t.OutputSchema)
		}
		out[t.Name] = lt
	}
	return out
}

// Risk is the risk document of the org's only approval request, as JSON.
func (g *RealGuard) Risk(ctx context.Context) string {
	GinkgoHelper()
	var risk []byte
	Expect(g.Pool.QueryRow(ctx, `SELECT risk FROM approval_requests WHERE org_id = $1`, g.Org).Scan(&risk)).To(Succeed())
	return string(risk)
}

// startRealGuard migrates a fresh database, seeds the org and the agent as internal/mcpguard's own
// suite does, and serves the guard on every interface (a gateway in a container or a cluster reaches
// it at Env.Host). It needs Docker; both stop at the end of the current container.
func startRealGuard(ctx context.Context) *RealGuard {
	GinkgoHelper()
	testenv.Require(testenv.Docker)
	pool := pgtest.Start(ctx)
	q := store.New(pool)
	org, user, slug := pgtest.NewOrg(ctx, pool)
	a, err := q.CreateAgent(ctx, store.CreateAgentParams{OrgID: org, DisplayName: "helper", Harness: harness.OpenCode, CreatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	v, err := q.CreateAgentVersion(ctx, store.CreateAgentVersionParams{OrgID: org, AgentID: a.ID, Config: []byte(`{"instructions":"x"}`), CreatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	agent, err := q.ActivateAgent(ctx, store.ActivateAgentParams{OrgID: org, ID: a.ID, VersionID: v.ID, ActivatedBy: user})
	Expect(err).NotTo(HaveOccurred())
	t, err := q.CreateTask(ctx, store.CreateTaskParams{OrgID: org, Title: "fix", AgentID: agent.ID, CreatedByKind: store.TaskCreatorKindUser, CreatedByUserID: user})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.CreateRun(ctx, store.CreateRunParams{OrgID: org, AgentID: agent.ID, TaskID: t.ID, MaxWallClockSeconds: 1800})
	Expect(err).NotTo(HaveOccurred())
	claimed, err := q.ClaimNextRun(ctx, store.ClaimNextRunParams{OrgID: org, OwnerID: "test", LeaseSeconds: 60})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.MarkRunRunning(ctx, store.MarkRunRunningParams{OrgID: org, ID: claimed.ID, Epoch: claimed.Epoch})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.UpsertOrgWorkspace(ctx, store.UpsertOrgWorkspaceParams{OrgID: org, Workspace: "w-" + slug})
	Expect(err).NotTo(HaveOccurred())
	names := execution.AgentObjectNames(org.Bytes, agent.ID.Bytes)
	_, err = q.CreateAgentSandbox(ctx, store.CreateAgentSandboxParams{
		OrgID: org, AgentID: agent.ID, Workspace: "w-" + slug,
		SandboxName: names.Sandbox, ProviderName: names.Provider, KeySecretName: names.KeySecret,
	})
	Expect(err).NotTo(HaveOccurred())
	_, err = q.RecordAgentSandbox(ctx, store.RecordAgentSandboxParams{OrgID: org, AgentID: agent.ID, State: store.SandboxStateReady, HomeGeneration: "gen-" + slug})
	Expect(err).NotTo(HaveOccurred())

	g := &RealGuard{
		Pool: pool, Org: org, User: user, Slug: slug, Agent: agent, srv: grpc.NewServer(), ids: map[string]string{},
		svc: mcpservers.New(pool, secretstoretest.Keyring(), nil).WithURLPolicy(catalog.URLPolicy{Namespace: "koinon"}),
	}
	guard := mcpguard.New(pool, mcpguard.Options{
		ByteCap: 64, Approve: mcpask.New(pool, mcpask.Options{}).RequestApproval, OwnTargets: []string{"github", "koinon"},
	})
	extmcp.RegisterExtMcpServer(g.srv, mcpguard.NewServer(guard))
	g.local = mcpguard.NewGatewayGuard(guard, nil)
	Expect(g.local.Load(ctx)).To(Succeed())
	l, err := net.Listen("tcp", "0.0.0.0:0") //nolint:gosec,noctx // the kind cluster reaches the test host over the network
	Expect(err).NotTo(HaveOccurred())
	_, p, _ := net.SplitHostPort(l.Addr().String())
	g.port, _ = strconv.Atoi(p)
	go func() { _ = g.srv.Serve(l) }()
	DeferCleanup(g.srv.Stop)
	return g
}

func uuidOf(s string) pgtype.UUID {
	var u pgtype.UUID
	Expect(u.Scan(s)).To(Succeed())
	return u
}
