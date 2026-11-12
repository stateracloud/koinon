// SPDX-License-Identifier: Apache-2.0

package agentd_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/approvals"
	"github.com/stateracloud/koinon/internal/connections"
	"github.com/stateracloud/koinon/internal/connectors/github"
	"github.com/stateracloud/koinon/internal/connectors/github/githubtest"
	"github.com/stateracloud/koinon/internal/db/store"
	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/githubtools"
	"github.com/stateracloud/koinon/internal/harness"
	"github.com/stateracloud/koinon/internal/secretstore"
	"github.com/stateracloud/koinon/internal/secretstore/secretstoretest"
	"github.com/stateracloud/koinon/internal/testing/fakegitea"
	"github.com/stateracloud/koinon/internal/testing/racescale"
)

// markAsk makes the fake harness call create_pull_request through Koinon's tool server.
const markAsk = "ASK"

type headerTransport struct{ agent string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(gateway.AgentHeader, h.agent)
	return http.DefaultTransport.RoundTrip(r)
}

// ghEnv is an env whose agent may propose pull requests on acme/widgets of a fake GitHub,
// through the real tool server, with the approval service that decides them.
type ghEnv struct {
	*env
	gh    *githubtest.Fake
	gt    *fakegitea.Fake // a Gitea in place of gh (newGiteaEnv)
	svc   *approvals.Service
	tools *httptest.Server
	slug  string
	ring  *secretstore.Keyring
	// toolsMu guards tools, which a restart test points at the surviving process set.
	toolsMu sync.Mutex
}

func newGitHubEnv(ctx context.Context) *ghEnv {
	g := &ghEnv{gh: githubtest.New()}
	DeferCleanup(g.gh.Server.Close)
	ring := secretstoretest.Keyring()
	g.ring = ring
	g.env = newEnvWith(ctx, harness.OpenCode, func(e *env) string {
		connID := uuid.New()
		id := pgtype.UUID{Bytes: connID, Valid: true}
		env, err := connections.SealFields(ring, e.org, id, map[string][]byte{connections.GitHubPrivateKeyField: g.gh.PEM})
		Expect(err).NotTo(HaveOccurred())
		cfg, _ := json.Marshal(connections.GitHubConfig{AppID: g.gh.AppID, InstallationID: g.gh.InstallationID})
		_, err = q.CreateConnectionWithEnvelope(ctx, store.CreateConnectionWithEnvelopeParams{
			OrgID: e.org, ID: id, Connector: "github", DisplayName: "acme",
			Config: cfg, SecretEnvelope: env, Source: store.ConfigSourceUi,
		})
		Expect(err).NotTo(HaveOccurred())
		return `{"instructions":"help","model":"` + model + `",` +
			`"grants":[{"connector":"github","scopes":["repo.read","pull_request.create"],"preset":"standard"}],` +
			`"bindings":[{"connector":"github","connectionId":"` + connID.String() + `","targets":["acme/widgets"]}]}`
	})
	Expect(db.QueryRow(ctx, `SELECT slug FROM orgs WHERE id = $1`, g.org).Scan(&g.slug)).To(Succeed())
	log := slog.New(slog.NewTextHandler(GinkgoWriter, nil))
	g.svc = approvals.New(db, approvals.Options{
		Keys: ring, GitHub: github.NewClient(github.Options{BaseURL: g.gh.URL()}), Logger: log,
	})
	tools, err := githubtools.New(db, githubtools.Options{Logger: log})
	Expect(err).NotTo(HaveOccurred())
	g.tools = httptest.NewServer(tools.Handler())
	DeferCleanup(g.tools.Close)
	return g
}

func (g *ghEnv) toolsURL() string {
	g.toolsMu.Lock()
	defer g.toolsMu.Unlock()
	return g.tools.URL
}

// callTool calls a Koinon tool as the agent and returns its text.
func (g *ghEnv) callTool(ctx context.Context, tool string, args any) (string, githubtools.Result) {
	client := mcp.NewClient(&mcp.Implementation{Name: "fake-harness", Version: "1"}, nil)
	s, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: g.toolsURL(), HTTPClient: &http.Client{Transport: headerTransport{agent: g.slug + "/" + g.agent.ID.String()}},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		return "connect: " + err.Error(), githubtools.Result{}
	}
	defer func() { _ = s.Close() }()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "call: " + err.Error(), githubtools.Result{}
	}
	var text []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			text = append(text, t.Text)
		}
	}
	var out githubtools.Result
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &out)
	return strings.Join(text, "\n"), out
}

// script makes the harness propose a pull request on an ASK prompt and, once resumed, ask
// for the request's status.
func (g *ghEnv) script() (ids chan string) {
	ids = make(chan string, 1)
	var mu sync.Mutex
	var id string
	g.harness.mu.Lock()
	g.harness.onPrompt = func(ctx context.Context, text string) (string, bool) {
		switch {
		case strings.Contains(text, markAsk):
			out, res := g.callTool(ctx, githubtools.ToolCreatePullRequest, map[string]any{
				"branch": "koinon/fix-typo", "title": "Fix the typo",
				"files": []map[string]any{{"path": "README.md", "content": "# Widgets, fixed\n"}},
			})
			mu.Lock()
			id = res.ID
			mu.Unlock()
			ids <- res.ID
			return "Proposed the change. " + out, true
		case strings.Contains(text, "[Koinon]"):
			mu.Lock()
			cur := id
			mu.Unlock()
			out, _ := g.callTool(ctx, githubtools.ToolGetApprovalStatus, map[string]any{"id": cur})
			return "status: " + out, true
		}
		return "", false
	}
	g.harness.mu.Unlock()
	return ids
}

func (g *ghEnv) taskStatus(ctx context.Context, t store.Task) func() store.TaskState {
	return func() store.TaskState {
		got, err := q.GetTask(ctx, store.GetTaskParams{OrgID: g.org, ID: t.ID})
		Expect(err).NotTo(HaveOccurred())
		return got.State
	}
}

func taskKinds(evs []store.TaskEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

var _ = Describe("Parking on approvals", func() {
	It("parks the run, and resumes it with the pull request's URL once approved and executed", func(ctx SpecContext) {
		g := newGitHubEnv(ctx)
		ids := g.script()
		t := g.task(ctx, markAsk+": fix the typo")
		r := g.enqueue(ctx, t)
		g.pool("agentd-a")

		var id string
		Eventually(ids).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&id))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))
		By("the instructions list the granted repository with its clone URL on the default host")
		written, _ := g.files.get(instructionsFile)
		Expect(string(written)).To(ContainSubstring("`acme/widgets` on GitHub (connector `github`, host `github.com`): `git clone https://github.com/acme/widgets`"))
		parked := getRun(ctx, r)
		Expect(parked.ParkReason.RunParkReason).To(Equal(store.RunParkReasonApproval))
		Expect(parked.LeaseExpiresAt.Valid).To(BeFalse(), "the run is released")
		Expect(parked.ParkExpiresAt.Time).To(BeTemporally("~", time.Now().Add(7*24*time.Hour), time.Minute))
		Expect(g.taskStatus(ctx, t)()).To(Equal(store.TaskStateInputRequired))
		Expect(taskKinds(taskEvents(ctx, g.env, t))).To(Equal([]string{"message", approvals.KindRequested, "message", "run.parked"}))
		Expect(kinds(events(ctx, r))).To(ContainElement("run.parked"))
		Consistently(runState(ctx, r)).WithTimeout(500 * time.Millisecond).Should(Equal(store.RunStateParked))

		row, err := q.GetApprovalRequest(ctx, store.GetApprovalRequestParams{OrgID: g.org, ID: mustUUID(id)})
		Expect(err).NotTo(HaveOccurred())
		decided, err := g.svc.Approve(ctx, g.org, row.ID, g.user, row.ArgsSha256, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(decided.Status).To(Equal(store.ApprovalStatusExecuted), decided.Error.String)
		const url = "https://github.test/acme/widgets/pull/1"

		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		prompts, loads, _, _ := g.harness.snapshot()
		Expect(prompts).To(HaveLen(2))
		Expect(prompts[1]).To(HavePrefix("[Koinon] Your run was paused while a human decided"))
		Expect(prompts[1]).To(ContainSubstring("create_pull_request (approval id=" + id))
		Expect(prompts[1]).To(ContainSubstring("approved and executed by Koinon: " + url))
		Expect(loads).To(Equal([]string{"ses_1"}), "the resumed run loads the task's session")

		By("get_approval_status says the same")
		evs := events(ctx, r)
		expectOrdered(evs)
		Expect(chunks(evs)[len(chunks(evs))-1]).To(ContainSubstring("status=executed url=" + url))

		final, err := q.GetApprovalRequest(ctx, store.GetApprovalRequestParams{OrgID: g.org, ID: row.ID})
		Expect(err).NotTo(HaveOccurred())
		Expect(final.DeliveredAt.Valid).To(BeTrue())
		By("the task follows the run's outcome after the park, like a run that never parked (old issue 324)")
		Expect(g.taskStatus(ctx, t)()).To(Equal(store.TaskStateCompleted))
		Expect(getRun(ctx, r).ParkedSeconds).To(BeNumerically(">=", 0))
		Expect(g.gh.Snapshot("widgets").Pulls).To(HaveLen(1))
	})

	It("resumes with the denial and the approver's note", func(ctx SpecContext) {
		g := newGitHubEnv(ctx)
		ids := g.script()
		t := g.task(ctx, markAsk+": fix the typo")
		r := g.enqueue(ctx, t)
		g.pool("agentd-a")
		var id string
		Eventually(ids).WithTimeout(racescale.D(10 * time.Second)).Should(Receive(&id))
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateParked))

		_, err := g.svc.Deny(ctx, g.org, mustUUID(id), g.user, "touch only the docs")
		Expect(err).NotTo(HaveOccurred())
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		prompts, _, _, _ := g.harness.snapshot()
		Expect(prompts[1]).To(ContainSubstring("denied by the approver, who wrote: touch only the docs"))
		Expect(chunks(events(ctx, r))).To(ContainElement(ContainSubstring("status=denied")))
		Expect(g.gh.Mints()).To(BeZero(), "nothing reached GitHub")
		Expect(g.taskStatus(ctx, t)()).To(Equal(store.TaskStateCompleted))
	})

	It("ends a run queued behind the parked run without a turn once the resumed run read its message (#21)", func(ctx SpecContext) {
		g := newGitHubEnv(ctx)
		ids := g.script()
		t := g.task(ctx, markAsk+": fix the typo")
		r := g.enqueue(ctx, t)
		g.pool("agentd-a")
		var id string
		Eventually(ids).WithTimeout(10 * time.Second).Should(Receive(&id))
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateParked))

		By("a message arrives while the run is parked, and queues a run behind it")
		g.message(ctx, t, "Also mention the README.")
		queued := g.enqueue(ctx, t)

		_, err := g.svc.Deny(ctx, g.org, mustUUID(id), g.user, "touch only the docs")
		Expect(err).NotTo(HaveOccurred())
		Eventually(runState(ctx, r)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))
		Eventually(runState(ctx, queued)).WithTimeout(10 * time.Second).Should(Equal(store.RunStateSucceeded))

		By("the resumed run took the message, and the queued run did not spend a model turn")
		prompts, _, _, _ := g.harness.snapshot()
		Expect(prompts).To(HaveLen(2))
		Expect(prompts[1]).To(ContainSubstring("Also mention the README."))
		Expect(kinds(events(ctx, queued))).NotTo(ContainElement("run.prompt"))
		Expect(getRun(ctx, queued).OutcomeReason.String).To(ContainSubstring("already read"))
		By("the task still ends, which the queued run was holding back")
		Eventually(g.taskStatus(ctx, t)).WithTimeout(10 * time.Second).Should(Equal(store.TaskStateCompleted))
	})

	It("does not wait when the request was decided before the turn ended", func(ctx SpecContext) {
		g := newGitHubEnv(ctx)
		g.harness.mu.Lock()
		g.harness.onPrompt = func(ctx context.Context, text string) (string, bool) {
			if !strings.Contains(text, markAsk) {
				return "", false
			}
			_, res := g.callTool(ctx, githubtools.ToolCreatePullRequest, map[string]any{
				"branch": "koinon/x", "title": "X", "files": []map[string]any{{"path": "x.txt", "content": "x"}},
			})
			_, err := g.svc.Deny(ctx, g.org, mustUUID(res.ID), g.user, "no")
			Expect(err).NotTo(HaveOccurred())
			return "asked", true
		}
		g.harness.mu.Unlock()
		t := g.task(ctx, markAsk)
		r := g.enqueue(ctx, t)
		g.pool("agentd-a")
		Eventually(runState(ctx, r)).WithTimeout(racescale.D(10 * time.Second)).Should(Equal(store.RunStateSucceeded))
		prompts, _, _, _ := g.harness.snapshot()
		Expect(prompts).To(HaveLen(2))
		Expect(prompts[1]).To(ContainSubstring("denied by the approver, who wrote: no"))
		Expect(g.taskStatus(ctx, t)()).NotTo(Equal(store.TaskStateInputRequired))
	})
})

func mustUUID(s string) pgtype.UUID {
	var id pgtype.UUID
	ExpectWithOffset(1, id.Scan(s)).To(Succeed())
	return id
}
