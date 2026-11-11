// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stateracloud/koinon/internal/execution"
	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	approvalv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	connectionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
	"github.com/stateracloud/koinon/internal/thinking"
	"github.com/stateracloud/koinon/test/e2e/e2eclient"
	"github.com/stateracloud/koinon/test/scenarios"
)

// What the fake model script test/fakemodel/scripts/repo-helper.yaml does is test/scenarios'
// PullRequest, which the owning packages' tests assert too; the fake GitHub's repository is
// test/e2e/kind/fakes.yaml. prBranch holds the branch Koinon files the pull request under once the
// agent exists (scenarios.PullRequest.Branch).
var (
	repository = scenarios.PullRequest.Repository
	repoName   = repository[strings.LastIndex(repository, "/")+1:]
	prFile     = scenarios.PullRequest.File
	prContent  = scenarios.PullRequest.Content
	ledger     = scenarios.PullRequest.Ledger
)

// quickstartBudget is plan item E1's bound for v0.1.0: the quickstart takes at most 20 minutes from
// `helm install` (the maintainer relaxed it from 15 on 2026-10-31, to be tightened later; the goal
// stays 12 minutes, #57). The per-phase table is printed either way.
const quickstartBudget = 20 * time.Minute

// timing is one measured step.
type timing struct {
	step string
	d    time.Duration
}

// timings records the wall clock of each quickstart step.
type timings struct {
	t0    time.Time // helm install started (KOINON_E2E_T0), or the suite's start
	steps []timing
	start time.Time
}

func (t *timings) begin() { t.start = time.Now() }

func (t *timings) end(step string) {
	d := time.Since(t.start)
	t.steps = append(t.steps, timing{step, d})
	GinkgoWriter.Printf("-- %s: %s\n", step, d.Round(100*time.Millisecond))
}

// report prints the table and writes it to KOINON_E2E_REPORT.
func (t *timings) report() {
	var b strings.Builder
	b.WriteString("| Step | Wall clock |\n| --- | --- |\n")
	var sum time.Duration
	for _, s := range t.steps {
		sum += s.d
		fmt.Fprintf(&b, "| %s | %.1f s |\n", s.step, s.d.Seconds())
	}
	fmt.Fprintf(&b, "| **Steps total** | **%.1f s** |\n", sum.Seconds())
	fmt.Fprintf(&b, "| **Total from `helm install`** | **%.1f s** |\n", time.Since(t.t0).Seconds())
	fmt.Println("\n" + b.String())
	if path := os.Getenv("KOINON_E2E_REPORT"); path != "" {
		Expect(os.WriteFile(path, []byte(b.String()), 0o600)).To(Succeed())
	}
}

// watcher reads one WatchRun stream in the background and keeps the latest run state.
type watcher struct {
	items chan *runv1alpha1.WatchRunResponse
	done  chan error
}

func watchRun(ctx context.Context, c *e2eclient.Client, org, run string, after int64) *watcher {
	stream, err := c.Runs.WatchRun(ctx, &runv1alpha1.WatchRunRequest{OrgId: org, RunId: run, AfterSeq: after})
	Expect(err).NotTo(HaveOccurred())
	w := &watcher{items: make(chan *runv1alpha1.WatchRunResponse, 4096), done: make(chan error, 1)}
	go func() {
		defer GinkgoRecover()
		defer close(w.items)
		defer func() { _ = stream.Close() }()
		for {
			msg, err := stream.Receive()
			if err != nil {
				w.done <- err
				return
			}
			w.items <- msg
		}
	}()
	return w
}

// until reads the stream until cond holds for the run, logging every event. It returns the
// last event seq seen. A run update (or a poll of GetRun, when the stream is quiet) is checked
// against cond.
func (w *watcher) until(ctx context.Context, c *e2eclient.Client, org, run string, timeout time.Duration, lastSeq *int64, kinds *[]string,
	cond func(*runv1alpha1.Run) bool,
) *runv1alpha1.Run {
	GinkgoHelper()
	deadline := time.After(timeout)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case msg, ok := <-w.items:
			if !ok {
				// The stream ends after the run finishes; fall back to polling.
				w.items = nil
				continue
			}
			if r := msg.GetRun(); r != nil {
				GinkgoWriter.Printf("   run %s park=%s\n", r.GetState(), r.GetParkReason())
				if cond(r) {
					return r
				}
			}
			if ev := msg.GetEvent(); ev != nil {
				*lastSeq = ev.GetSeq()
				*kinds = append(*kinds, ev.GetKind())
				GinkgoWriter.Printf("   event %d %s\n", ev.GetSeq(), ev.GetKind())
			}
		case <-tick.C:
			got, err := c.Runs.GetRun(ctx, &runv1alpha1.GetRunRequest{OrgId: org, RunId: run})
			Expect(err).NotTo(HaveOccurred())
			if cond(got.GetRun()) {
				return got.GetRun()
			}
			if terminal(got.GetRun().GetState()) {
				Fail(fmt.Sprintf("run ended %s: %s", got.GetRun().GetState(), got.GetRun().GetOutcomeReason()))
			}
		case <-deadline:
			got, _ := c.Runs.GetRun(ctx, &runv1alpha1.GetRunRequest{OrgId: org, RunId: run})
			Fail(fmt.Sprintf("run did not reach the expected state within %s; it is %s (%s)",
				timeout, got.GetRun().GetState(), got.GetRun().GetOutcomeReason()))
		}
	}
}

func terminal(s runv1alpha1.RunState) bool {
	switch s {
	case runv1alpha1.RunState_RUN_STATE_SUCCEEDED, runv1alpha1.RunState_RUN_STATE_FAILED, runv1alpha1.RunState_RUN_STATE_CANCELED:
		return true
	}
	return false
}

var _ = Describe("the quickstart", Ordered, func() {
	var (
		c        *e2eclient.Client
		gh       fakeGitHub
		t        timings
		org      string
		conn     *connectionv1alpha1.Connection
		agent    *agentv1alpha1.Agent
		task     *taskv1alpha1.Task
		run      *runv1alpha1.Run
		w        *watcher
		lastSeq  int64
		kinds    []string
		approval *approvalv1alpha1.Approval
		prBranch string
		prURL    string
		// admin calls /admin/v1 with a token made at step 1b.
		admin func(ctx context.Context, method, path string, body ...string) (int, []byte)
		// mcpServerID is the remote MCP server step 6b added and 6c uses.
		mcpServerID string
	)

	BeforeAll(func() {
		c = e2eclient.New(strings.TrimRight(os.Getenv("KOINON_E2E_API_URL"), "/"))
		gh = fakeGitHub{base: strings.TrimRight(os.Getenv("KOINON_E2E_FAKEGITHUB_URL"), "/")}
		Expect(gh.base).NotTo(BeEmpty(), "KOINON_E2E_FAKEGITHUB_URL")
		t.t0 = time.Now()
		if v := os.Getenv("KOINON_E2E_T0"); v != "" {
			sec, err := strconv.ParseInt(v, 10, 64)
			Expect(err).NotTo(HaveOccurred())
			t.t0 = time.Unix(sec, 0)
		}
	})

	AfterAll(func() { t.report() })

	It("1. logs in as the bootstrap admin and sets a new password", Label("smoke"), func(ctx SpecContext) {
		t.begin()
		password := os.Getenv("KOINON_E2E_PASSWORD")
		Expect(password).NotTo(BeEmpty(), "KOINON_E2E_PASSWORD")
		changeRequired, err := c.Login(ctx, "admin", password)
		Expect(err).NotTo(HaveOccurred())
		Expect(changeRequired).To(BeTrue(), "the one-time password must be changed first")

		buf := make([]byte, 16)
		_, _ = rand.Read(buf)
		_, err = c.Auth.ChangePassword(ctx, &authv1alpha1.ChangePasswordRequest{
			CurrentPassword: password, NewPassword: "e2e-" + hex.EncodeToString(buf),
		})
		Expect(err).NotTo(HaveOccurred())

		me, err := c.Auth.WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
		Expect(me.GetPasswordChangeRequired()).To(BeFalse())
		Expect(me.GetPlatformAdmin()).To(BeTrue())
		for _, m := range me.GetMemberships() {
			if m.GetOrgSlug() == "default" {
				org = m.GetOrgId()
				Expect(m.GetRole()).To(Equal(authv1alpha1.OrgRole_ORG_ROLE_OWNER))
			}
		}
		Expect(org).NotTo(BeEmpty(), "the default org")
		t.end("1. Bootstrap login and password change")
	})

	It("1b. sees the model provider the quickstart declared, through the admin API", Label("smoke"), func(ctx SpecContext) {
		t.begin()
		// The quickstart's model-provider.yaml is a labelled Secret that koinon-api imported at start.
		// A token made with the session reads it back through /admin/v1, read-only.
		tok, err := c.Tokens.CreateToken(ctx, &tokenv1alpha1.CreateTokenRequest{
			OrgId: org, Name: "e2e", Role: authv1alpha1.OrgRole_ORG_ROLE_ADMIN,
		})
		Expect(err).NotTo(HaveOccurred())
		admin = func(ctx context.Context, method, path string, body ...string) (int, []byte) {
			var rdr io.Reader
			if len(body) > 0 {
				rdr = strings.NewReader(body[0])
			}
			req, err := http.NewRequestWithContext(ctx, method, c.Base+"/admin/v1"+path, rdr)
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Authorization", "Bearer "+tok.GetSecret())
			if rdr != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			res, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = res.Body.Close() }()
			b, _ := io.ReadAll(res.Body)
			return res.StatusCode, b
		}
		var providers struct {
			Providers []struct {
				ID, Name, Source, ManagedBySecret string
			}
		}
		Eventually(func(g Gomega) {
			code, body := admin(ctx, http.MethodGet, "/providers")
			g.Expect(code).To(Equal(http.StatusOK), string(body))
			g.Expect(json.Unmarshal(body, &providers)).To(Succeed())
			// The quickstart's provider and the e2e's direct Anthropic one (kind/model-provider-anthropic.yaml).
			g.Expect(providers.Providers).To(HaveLen(2))
		}).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
		byName := map[string]int{}
		for i, p := range providers.Providers {
			byName[p.Name] = i
		}
		Expect(byName).To(HaveKey("fake"))
		fake := providers.Providers[byName["fake"]]
		Expect(fake.Source).To(Equal("secret"))
		Expect(fake.ManagedBySecret).To(Equal("fake-model"))
		Expect(byName).To(HaveKey("fake-anthropic"))
		Expect(providers.Providers[byName["fake-anthropic"]].ManagedBySecret).To(Equal("fake-anthropic"))
		code, body := admin(ctx, http.MethodDelete, "/providers/"+fake.ID)
		Expect(code).To(Equal(http.StatusConflict), "declared providers are read-only: %s", body)
		// A model has no rates of its own; its price is an org price (or a default one). The fake model
		// is in no default list, so the run is priced by this entry and the ledger's estimates work.
		code, body = admin(ctx, http.MethodPost, "/prices", `{"name":"fake-repo-helper","provider":"openai","match":{"glob":"repo-helper"},`+
			`"prices":{"input":"0.30","output":"1.20"},"maxOutput":4096}`)
		Expect(code).To(Equal(http.StatusCreated), string(body))
		t.end("1b. Declared model provider and its price (admin API)")
	})

	It("2. creates and tests the GitHub connection", Label("smoke"), func(ctx SpecContext) {
		t.begin()
		app := gh.app(ctx)
		res, err := c.Connections.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
			OrgId: org, DisplayName: "GitHub (" + app.Account + ")",
			AppId: app.AppID, InstallationId: app.InstallationID, PrivateKey: app.PrivateKey, Test: true,
		})
		Expect(err).NotTo(HaveOccurred())
		for _, r := range res.GetResults() {
			Expect(r.GetOk()).To(BeTrue(), "check %s: %s", r.GetCheck(), r.GetMessage())
		}
		conn = res.GetConnection()
		Expect(conn.GetStatus()).To(Equal(connectionv1alpha1.ConnectionStatus_CONNECTION_STATUS_OK))
		t.end("2. GitHub connection (create and test)")
	})

	It("3. creates a repo-helper agent bound to the repository and activates it", Label("smoke"), func(ctx SpecContext) {
		t.begin()
		ts, err := c.Templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{})
		Expect(err).NotTo(HaveOccurred())
		var tmpl string
		for _, tp := range ts.GetTemplates() {
			if tp.GetSlug() == "repo-helper" {
				tmpl = tp.GetId()
			}
		}
		Expect(tmpl).NotTo(BeEmpty(), "the repo-helper template")

		created, err := c.Agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: org, TemplateId: tmpl, DisplayName: "Repo helper",
			Flavor: templatev1alpha1.Flavor_FLAVOR_OPENCODE,
			Grants: []*agentv1alpha1.GrantBinding{{Connector: "github", ConnectionId: conn.GetId(), Repositories: []string{repository}}},
		})
		Expect(err).NotTo(HaveOccurred())
		agent = created.GetAgent()
		Expect(agent.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_DRAFT))
		prBranch, err = scenarios.PullRequest.Branch(agent.GetDisplayName(), agent.GetId())
		Expect(err).NotTo(HaveOccurred())

		activated, err := c.Agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
			OrgId: org, AgentId: agent.GetId(), Version: agent.GetLatestVersion(),
		})
		Expect(err).NotTo(HaveOccurred())
		agent = activated.GetAgent()
		Expect(agent.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_ACTIVE))

		// The template names no model, so the run takes the org default, and the quickstart steps need
		// it to be the quickstart's model (repo-helper's script makes the tool call step 4 parks on).
		// The org's first model sets the default on its own, but which model is first depends on the
		// order the importer reads the declared Secrets (creation time, one-second resolution, then
		// name), which this test must not rely on: it states the default, as an admin does. repo-helper
		// is priced since 1b.
		code, body := admin(ctx, http.MethodPut, "/default-model", `{"model":"repo-helper"}`)
		Expect(code).To(Equal(http.StatusOK), string(body))
		Eventually(func(g Gomega) {
			code, body := admin(ctx, http.MethodGet, "/agents/"+agent.GetId()+"/model")
			g.Expect(code).To(Equal(http.StatusOK), string(body))
			var m struct{ Effective, Source string }
			g.Expect(json.Unmarshal(body, &m)).To(Succeed())
			g.Expect(m.Source).To(Equal("org_default"), string(body))
			g.Expect(m.Effective).To(Equal("repo-helper"), string(body))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(time.Second).Should(Succeed())
		t.end("3. Agent from template, activated")
	})

	It("4. creates a task, streams the run and parks on an approval", Label("smoke"), func(ctx SpecContext) {
		t.begin()
		res, err := c.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
			OrgId: org, AgentId: agent.GetId(), Title: "Add hello.md",
			Message: "Please add a hello.md to " + repository + " that says hello from koinon.",
		})
		Expect(err).NotTo(HaveOccurred())
		task, run = res.GetTask(), res.GetRun()
		Expect(run.GetId()).NotTo(BeEmpty())

		w = watchRun(context.WithoutCancel(ctx), c, org, run.GetId(), 0)
		run = w.until(ctx, c, org, run.GetId(), 8*time.Minute, &lastSeq, &kinds, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_PARKED
		})
		Expect(run.GetParkReason()).To(Equal(runv1alpha1.ParkReason_PARK_REASON_APPROVAL))
		Expect(kinds).To(ContainElement("acp.tool_call"), "the run streamed the agent's tool call")

		list, err := c.Approvals.ListApprovals(ctx, &approvalv1alpha1.ListApprovalsRequest{OrgId: org, TicketId: task.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetApprovals()).To(HaveLen(1))
		// The list is a summary; the approval card reads the call itself.
		got, err := c.Approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: list.GetApprovals()[0].GetId()})
		Expect(err).NotTo(HaveOccurred())
		approval = got.GetApproval()
		Expect(approval.GetStatus()).To(Equal(approvalv1alpha1.Status_STATUS_PENDING))
		Expect(approval.GetCanDecide()).To(BeTrue())
		Expect(approval.GetTarget()).To(Equal("github:" + repository))
		pr := approval.GetPullRequest()
		Expect(pr.GetBranch()).To(Equal(prBranch))
		Expect(pr.GetFiles()).To(ConsistOf(HaveField("Path", prFile)))

		By("the model's streamed requests reported their usage through the gateway")
		// The gateway exports its access log in batches, so the ledger lags the run by a moment.
		var cost *runv1alpha1.Cost
		Eventually(func(g Gomega) {
			got, err := c.Runs.GetRun(ctx, &runv1alpha1.GetRunRequest{OrgId: org, RunId: run.GetId()})
			g.Expect(err).NotTo(HaveOccurred())
			cost = got.GetRun().GetCost()
			// The refused title request (no tokens) can reach the ledger before the streamed ones:
			// wait until the streamed usage is in too, not only the first entry (old issue 291).
			g.Expect(ledger.Settled(totalsOf(cost))).To(BeTrue(), "the ledger has the streamed usage: %v", cost)
		}, 60*time.Second, time.Second).Should(Succeed())
		// The streamed requests carry usage through the gateway, so none is an estimate. The title
		// request the fake model refuses (a 400 without usage) is the provider's refusal, and the
		// quickstart's provider type, openai, does not bill one: the ledger keeps its entry with no
		// tokens and no cost and marks the charge waived; it is not an estimate either (ADR 0056,
		// amendment 2026-10-29, point 3; old issue 241).
		Expect(ledger.CheckTotals(totalsOf(cost))).To(Succeed())

		By("the refused title request is in the ledger, with the provider's status, and charged nothing")
		kubectl := os.Getenv("KOINON_E2E_KUBECTL")
		Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL")
		var refused, answered []ledgerRow
		Eventually(func(g Gomega) {
			refused, answered = nil, nil
			for _, r := range ledgerRows(ctx, g, kubectl, run.GetId()) {
				if r.UpstreamStatus != 0 {
					refused = append(refused, r)
				} else {
					answered = append(answered, r)
				}
			}
			g.Expect(refused).NotTo(BeEmpty(), "the title request the fake model refuses")
		}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
		Expect(answered).NotTo(BeEmpty(), "the streamed requests")
		// OpenCode asks for a title with each prompt until it has one, so the turn's prompt and the
		// end-of-turn note's prompt make at most two such requests; a third would be a request nobody
		// accounts for.
		Expect(len(refused)).To(BeNumerically("<=", ledger.MaxRefused), "%v", refused)
		for _, r := range refused {
			Expect(ledger.CheckRefused(r.entry())).To(Succeed(), "%+v", r)
		}

		By("nothing has reached GitHub before the approval")
		repo := gh.repo(ctx, repoName)
		Expect(repo.Pulls).To(BeEmpty())
		Expect(repo.Refs).NotTo(HaveKey(prBranch))
		t.end("4. Task → run streamed → approval pending")
	})

	It("5. approves, and Koinon opens exactly one branch, commit and pull request", Label("smoke"), func(ctx SpecContext) {
		t.begin()
		_, err := c.Approvals.Approve(ctx, &approvalv1alpha1.ApproveRequest{
			OrgId: org, ApprovalId: approval.GetId(), ArgsSha256: approval.GetArgsSha256(),
		})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			got, err := c.Approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: approval.GetId()})
			g.Expect(err).NotTo(HaveOccurred())
			approval = got.GetApproval()
			g.Expect(approval.GetStatus()).To(Equal(approvalv1alpha1.Status_STATUS_EXECUTED), approval.GetError())
		}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
		Expect(approval.GetExecutions()).To(BeEquivalentTo(1))
		prURL, _ = approval.GetResult().AsMap()["url"].(string)

		repo := gh.repo(ctx, repoName)
		Expect(repo.Calls).To(HaveKeyWithValue("POST git/refs", 1))
		Expect(repo.Calls).To(HaveKeyWithValue("POST git/commits", 1))
		Expect(repo.Calls).To(HaveKeyWithValue("POST pulls", 1))
		Expect(repo.Pulls).To(HaveLen(1))
		Expect(repo.Pulls[0].Head).To(Equal(prBranch))
		Expect(repo.Pulls[0].Base).To(Equal("main"))
		Expect(repo.Pulls[0].HTMLURL).To(Equal(prURL))
		Expect(repo.Files[prBranch]).To(HaveKeyWithValue(prFile, prContent))
		t.end("5. Approve → branch, commit, PR on fake GitHub")
	})

	It("6. resumes the run, which posts the pull request URL on the task", Label("smoke"), func(ctx SpecContext) {
		t.begin()
		run = w.until(ctx, c, org, run.GetId(), 5*time.Minute, &lastSeq, &kinds, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_SUCCEEDED
		})
		got, err := c.Tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: task.GetId()})
		Expect(err).NotTo(HaveOccurred())
		var replies []string
		for _, ev := range got.GetEvents() {
			if ev.GetKind() == "message" && ev.GetActorKind() == taskv1alpha1.ActorKind_ACTOR_KIND_AGENT {
				text, _ := ev.GetPayload().AsMap()["text"].(string)
				replies = append(replies, text)
			}
		}
		Expect(replies).NotTo(BeEmpty())
		Expect(replies[len(replies)-1]).To(ContainSubstring(prURL))

		By("the pull request was opened once")
		repo := gh.repo(ctx, repoName)
		Expect(repo.Calls).To(HaveKeyWithValue("POST pulls", 1))
		Expect(repo.Pulls).To(HaveLen(1))
		t.end("6. Run resumes → PR URL on the task")
	})

	It("6a. the agent's end-of-turn note is on the task, not in its timeline as a reply", Label("smoke"), func(ctx SpecContext) {
		t.begin()
		// The note is written after each turn (the run parked once, then finished: two turns), so
		// it is there by the time the run has succeeded; the last one wins.
		var got *taskv1alpha1.GetTaskResponse
		Eventually(func(g Gomega) {
			var err error
			got, err = c.Tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: task.GetId()})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.GetTask().GetAgentSummary()).To(HavePrefix("Notes: the task asks for a hello.md"))
		}, 30*time.Second, time.Second).Should(Succeed())
		Expect(got.GetTask().GetAgentSummaryAt()).NotTo(BeNil())
		for _, ev := range got.GetEvents() {
			if ev.GetKind() == "message" {
				text, _ := ev.GetPayload().AsMap()["text"].(string)
				Expect(text).NotTo(HavePrefix("Notes:"), "the note is not a reply on the task")
			}
		}
		By("the note's model call went through the gateway and was counted with the run")
		Eventually(func(g Gomega) {
			r, err := c.Runs.GetRun(ctx, &runv1alpha1.GetRunRequest{OrgId: org, RunId: run.GetId()})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(r.GetRun().GetCost().GetRequests()).To(BeNumerically(">=", 4), "title, two turns and at least one note")
		}, 60*time.Second, time.Second).Should(Succeed())
		t.end("6a. Agent's notes on the task")
	})

	It("6a2. the saved-work upload Service exists and answers; the tools port does not serve it; the OTLP port needs a token", Label("smoke"), func(ctx SpecContext) {
		t.begin()
		kubectl := os.Getenv("KOINON_E2E_KUBECTL")
		Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL")
		post := func(addr, path string) int {
			q := "?org=00000000-0000-7000-8000-000000000001&run=00000000-0000-7000-8000-000000000002&epoch=1&n=1&exp=" +
				strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+path+q, strings.NewReader("x"))
			Expect(err).NotTo(HaveOccurred())
			res, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_ = res.Body.Close()
			return res.StatusCode
		}
		// The Service agentd's KOINON_SAVED_WORK_URL names: an unsigned post is refused, by the receiver.
		Expect(post(portForward(ctx, kubectl, "svc/koinon-api-saved-work", "8082"), "/internal/saved-work")).To(Equal(http.StatusUnauthorized))
		// The tools port, which agentd cannot reach, does not serve the path at all.
		Expect(post(portForward(ctx, kubectl, "svc/koinon-api-tools", "8081"), "/internal/saved-work")).To(Equal(http.StatusNotFound))
		// The internal OTLP listener (the gateway's access log, agentd's spans) requires its bearer token:
		// no token and a wrong one are refused, whatever the path. (The gateway's own exports carry the
		// real token, which is what keeps the spend ledger filling in the steps above.)
		otlp := portForward(ctx, kubectl, "svc/koinon-api", "4318")
		otlpStatus := func(path, auth string) int {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+otlp+path, strings.NewReader("x"))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/x-protobuf")
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			res, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			_ = res.Body.Close()
			return res.StatusCode
		}
		for _, auth := range []string{"", "Bearer koinon_test_not-a-real-token-wrong-0000000"} {
			for _, path := range []string{"/v1/logs", "/v1/traces"} {
				Expect(otlpStatus(path, auth)).To(Equal(http.StatusUnauthorized), path+" with Authorization %q", auth)
			}
		}
		// The two tokens have separate scopes (issue 55): the gateway's token gets 401 on /v1/traces
		// and agentd's token gets 401 on /v1/logs. (Each is admitted on its own path: the gateway's
		// exports fill the spend ledger, agentd's spans reach the trace router, in the steps above.)
		gatewayToken := "Bearer " + secretToken(ctx, kubectl, "koinon-otlp-token")
		agentdToken := "Bearer " + secretToken(ctx, kubectl, "koinon-otlp-traces-token")
		Expect(otlpStatus("/v1/traces", gatewayToken)).To(Equal(http.StatusUnauthorized), "the gateway's token on /v1/traces")
		Expect(otlpStatus("/v1/logs", agentdToken)).To(Equal(http.StatusUnauthorized), "agentd's token on /v1/logs")
		Expect(otlpStatus("/v1/traces", agentdToken)).NotTo(Equal(http.StatusUnauthorized), "agentd's token on /v1/traces")
		Expect(otlpStatus("/v1/logs", gatewayToken)).NotTo(Equal(http.StatusUnauthorized), "the gateway's token on /v1/logs")
		t.end("6a2. Saved-work upload Service")
	})

	It("6b. adds a remote MCP server: the gateway injects its credential, and an agent's call reaches it", func(ctx SpecContext) {
		t.begin()
		mcpServerID = mcpServerE2E(ctx, admin, org, agent.GetId())

		By("inside a task, the same kind of call is held for an approval of the exact call, and runs only once approved")
		fake := strings.TrimRight(os.Getenv("KOINON_E2E_FAKEMCP_URL"), "/")
		p := scenarios.PullRequest
		guarded := func() []fakeMCPRecord {
			var out []fakeMCPRecord
			for _, r := range fakeMCPRequests(ctx, fake) {
				if r.Tool == "write_item" && r.Args == p.TrackerGuardArgs {
					out = append(out, r)
				}
			}
			return out
		}
		res, err := c.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
			OrgId: org, AgentId: agent.GetId(), Title: "Tracker exfil",
			Message: p.TrackerGuardPrompt + ": read an item from the tracker, then write it back.",
		})
		Expect(err).NotTo(HaveOccurred())
		var seq int64
		var evKinds []string
		gw := watchRun(context.WithoutCancel(ctx), c, org, res.GetRun().GetId(), 0)
		parked := gw.until(ctx, c, org, res.GetRun().GetId(), 8*time.Minute, &seq, &evKinds, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_PARKED
		})
		Expect(parked.GetParkReason()).To(Equal(runv1alpha1.ParkReason_PARK_REASON_APPROVAL))
		list, err := c.Approvals.ListApprovals(ctx, &approvalv1alpha1.ListApprovalsRequest{OrgId: org, TicketId: res.GetTask().GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetApprovals()).To(HaveLen(1), "the guard's card is the run's only approval")
		got, err := c.Approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: list.GetApprovals()[0].GetId()})
		Expect(err).NotTo(HaveOccurred())
		ap := got.GetApproval()
		Expect(ap.GetConnector()).To(Equal("mcp"))
		Expect(ap.GetTool()).To(Equal("write_item"))
		Expect(ap.GetTarget()).To(Equal(p.TrackerAskTarget))
		Expect(ap.GetArgsJson()).To(Equal(p.TrackerGuardArgs))
		// The card names why the guard held the call: both legs, on an external-high tool (#293).
		lg := ap.GetMcp().GetLeakGuard()
		Expect(lg).NotTo(BeNil(), "the approval is the leak guard's, and its card carries the guard's reading")
		Expect(lg.GetReasons()).To(ContainElement("both_legs"))
		Expect(lg.GetSinkClass()).To(Equal("external-high"))
		Expect(lg.GetUntrustedInput()).To(BeTrue(), "the card names the untrusted-input leg")
		Expect(lg.GetPrivateData()).To(BeTrue(), "the card names the private-data leg")
		Expect(guarded()).To(BeEmpty(), "the guard held the call: nothing reached the server before the approval")

		_, err = c.Approvals.Approve(ctx, &approvalv1alpha1.ApproveRequest{OrgId: org, ApprovalId: ap.GetId(), ArgsSha256: ap.GetArgsSha256()})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			a, err := c.Approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: ap.GetId()})
			g.Expect(err).NotTo(HaveOccurred())
			ap = a.GetApproval()
			g.Expect(ap.GetStatus()).To(Equal(approvalv1alpha1.Status_STATUS_EXECUTED), ap.GetError())
		}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
		Expect(ap.GetExecutions()).To(BeEquivalentTo(1))
		calls := guarded()
		Expect(calls).To(HaveLen(1), "the server saw the approved call exactly once")
		Expect(calls[0].Agent).To(Equal("default/mcp-exec"), "Koinon's executor made it, not the agent")
		gw.until(ctx, c, org, res.GetRun().GetId(), 5*time.Minute, &seq, &evKinds, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_SUCCEEDED
		})
		t.end("6b. Remote MCP server (add, health, tiers, allowlist, call through the gateway, leak guard)")
	})

	It("6c. an agent's call to an ask-tier MCP tool parks, is approved, and runs once through the gateway with the exact arguments", func(ctx SpecContext) {
		t.begin()
		Expect(mcpServerID).NotTo(BeEmpty(), "step 6b adds the server")
		fake := strings.TrimRight(os.Getenv("KOINON_E2E_FAKEMCP_URL"), "/")
		res, err := c.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
			OrgId: org, AgentId: agent.GetId(), Title: "Tracker write",
			Message: "tracker write: please call the tracker's write tool.",
		})
		Expect(err).NotTo(HaveOccurred())
		var seq int64
		var evKinds []string
		mw := watchRun(context.WithoutCancel(ctx), c, org, res.GetRun().GetId(), 0)
		parked := mw.until(ctx, c, org, res.GetRun().GetId(), 8*time.Minute, &seq, &evKinds, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_PARKED
		})
		Expect(parked.GetParkReason()).To(Equal(runv1alpha1.ParkReason_PARK_REASON_APPROVAL))

		list, err := c.Approvals.ListApprovals(ctx, &approvalv1alpha1.ListApprovalsRequest{OrgId: org, TicketId: res.GetTask().GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetApprovals()).To(HaveLen(1))
		got, err := c.Approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: list.GetApprovals()[0].GetId()})
		Expect(err).NotTo(HaveOccurred())
		ap := got.GetApproval()
		Expect(ap.GetConnector()).To(Equal("mcp"))
		Expect(ap.GetTool()).To(Equal("write_item"))
		Expect(ap.GetTarget()).To(Equal(scenarios.PullRequest.TrackerAskTarget))
		Expect(ap.GetArgsJson()).To(Equal(scenarios.PullRequest.TrackerAskArgs))
		Expect(ap.GetMcp().GetContentHash()).NotTo(BeEmpty())
		Expect(ap.GetMcp().GetContentHash()).To(Equal(ap.GetMcp().GetCurrentContentHash()))
		for _, r := range fakeMCPRequests(ctx, fake) {
			// Step 6b's in-task leak-guard case wrote once, with its own arguments.
			Expect(r.Tool == "write_item" && r.Args == scenarios.PullRequest.TrackerAskArgs).To(BeFalse(), "nothing reached the server before the approval")
		}

		_, err = c.Approvals.Approve(ctx, &approvalv1alpha1.ApproveRequest{OrgId: org, ApprovalId: ap.GetId(), ArgsSha256: ap.GetArgsSha256()})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			a, err := c.Approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: ap.GetId()})
			g.Expect(err).NotTo(HaveOccurred())
			ap = a.GetApproval()
			g.Expect(ap.GetStatus()).To(Equal(approvalv1alpha1.Status_STATUS_EXECUTED), ap.GetError())
		}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
		Expect(ap.GetExecutions()).To(BeEquivalentTo(1))

		var calls []fakeMCPRecord
		for _, r := range fakeMCPRequests(ctx, fake) {
			if r.Tool == "write_item" && r.Args != scenarios.PullRequest.TrackerGuardArgs {
				calls = append(calls, r)
			}
		}
		Expect(calls).To(HaveLen(1), "the server saw the approved call exactly once")
		Expect(calls[0].Args).To(Equal(scenarios.PullRequest.TrackerAskArgs))
		Expect(calls[0].Agent).To(Equal("default/mcp-exec"), "through the gateway, with the org's executor key")
		Expect(calls[0].Authorized).To(BeTrue())

		mw.until(ctx, c, org, res.GetRun().GetId(), 5*time.Minute, &seq, &evKinds, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_SUCCEEDED
		})
		code, body := admin(ctx, http.MethodDelete, "/mcp-servers/"+mcpServerID)
		Expect(code).To(Equal(http.StatusNoContent), string(body))
		t.end("6c. Ask-tier MCP call: parks, approved, runs once through the gateway")
	})

	It("6d. a direct Anthropic model runs in the Messages format through OpenShell and the gateway, and its second request reads the cache", func(ctx SpecContext) {
		t.begin()
		// The run's wire format comes from the agent's effective model (ADR 0072, 0069), so the agent
		// is set to claude-e2e below; that does not depend on how the org's models sort.
		// Prices in USD per million tokens, as Claude's list: a cache read costs a tenth of an input.
		price := map[string]string{"input": "3.00", "output": "15.00", "cacheRead": "0.30", "cacheWrite": "3.75"}
		code, body := admin(ctx, http.MethodPost, "/prices", `{"name":"fake-claude-e2e","provider":"anthropic","match":{"glob":"claude-e2e"},`+
			`"prices":{"input":"3.00","output":"15.00","cacheRead":"0.30","cacheWrite":"3.75"},"maxOutput":4096}`)
		Expect(code).To(Equal(http.StatusCreated), string(body))

		// A model is settable once the org serves it, which the reconciler does after the price exists.
		modelPath := "/agents/" + agent.GetId() + "/model"
		DeferCleanup(func(ctx context.Context) {
			code, body := admin(ctx, http.MethodPut, modelPath, `{"model":"default"}`)
			Expect(code).To(Equal(http.StatusOK), string(body))
		})
		Eventually(func(g Gomega) {
			code, body := admin(ctx, http.MethodPut, modelPath, `{"model":"claude-e2e"}`)
			g.Expect(code).To(Equal(http.StatusOK), string(body))
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())

		res, err := c.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
			OrgId: org, AgentId: agent.GetId(), Title: "Cache probe", Message: "Run the cache probe and say when it is done.",
		})
		Expect(err).NotTo(HaveOccurred())
		var seq int64
		var evKinds []string
		cw := watchRun(context.WithoutCancel(ctx), c, org, res.GetRun().GetId(), 0)
		done := cw.until(ctx, c, org, res.GetRun().GetId(), 8*time.Minute, &seq, &evKinds, func(r *runv1alpha1.Run) bool {
			return terminal(r.GetState())
		})
		Expect(done.GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_SUCCEEDED), done.GetOutcomeReason())
		Expect(evKinds).NotTo(ContainElement("run.uncached"), "a native-format model caches; the run does not say it runs uncached")

		By("every request of the run went to /v1/messages, through the sandbox profile and the key policy")
		kubectl := os.Getenv("KOINON_E2E_KUBECTL")
		Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL")
		fakeModel := strings.TrimRight(os.Getenv("KOINON_E2E_FAKEMODEL_URL"), "/")
		Expect(fakeModel).NotTo(BeEmpty(), "KOINON_E2E_FAKEMODEL_URL")
		var seen []string
		Eventually(func(g Gomega) {
			seen = fakeModelPaths(ctx, g, fakeModel, "claude-e2e")
			g.Expect(len(seen)).To(BeNumerically(">=", 2), "the task's request and the end-of-turn note")
		}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
		Expect(seen).To(HaveEach("/v1/messages"))

		By("the ledger has a cache write, then a cache read priced at the cache-read rate")
		var rows []ledgerRow
		Eventually(func(g Gomega) {
			rows = ledgerRows(ctx, g, kubectl, done.GetId())
			var reads int
			for _, r := range rows {
				if r.CacheRead > 0 {
					reads++
				}
			}
			g.Expect(reads).To(BeNumerically(">=", 1), "a ledger entry with cache_read_tokens > 0: %v", rows)
		}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
		writeIdx, readIdx := -1, -1
		for i, r := range rows {
			Expect(r.Model).To(Equal("claude-e2e"))
			if r.CacheWrite > 0 && writeIdx < 0 {
				writeIdx = i
			}
			if r.CacheRead > 0 && readIdx < 0 {
				readIdx = i
			}
			// The ledger's input_tokens includes the cache reads and writes (the gateway normalises
			// Anthropic's input_tokens that way); each part is priced at its own rate.
			want := new(big.Rat).Add(
				new(big.Rat).Add(rate(price["input"], r.Input-r.CacheRead-r.CacheWrite), rate(price["cacheRead"], r.CacheRead)),
				new(big.Rat).Add(rate(price["cacheWrite"], r.CacheWrite), rate(price["output"], r.Output)))
			Expect(r.Cost.Cmp(want)).To(Equal(0), "entry %+v costs %s, want %s", r, r.Cost.FloatString(9), want.FloatString(9))
		}
		Expect(writeIdx).To(BeNumerically(">=", 0), "a request wrote the cache: %v", rows)
		Expect(writeIdx).To(BeNumerically("<", readIdx), "the cache write comes before the first cache read: %v", rows)
		t.end("6d. Direct Anthropic model: Messages through the gateway, cache read priced")
	})

	It("7. took at most 20 minutes from helm install", func() {
		total := time.Since(t.t0)
		Expect(total).To(BeNumerically("<=", quickstartBudget),
			"the quickstart took %s from helm install, more than the plan's %s", total.Round(time.Second), quickstartBudget)
	})

	It("8. (not part of the quickstart, after its time check) explicit thinking on and off crosses the real gateway to the strict fake model (rc.7, old issue 240, old issue 316)", func(ctx SpecContext) {
		t.begin()
		// The fake model server runs with -strict-messages (test/e2e/kind/fakes.yaml): it answers 400
		// to a body field whose beta is missing or a thinking type the model does not take, as
		// api.anthropic.com does, so a run that succeeds shows OpenCode's real body is one Anthropic
		// takes. claude-haiku-5-5 is an ID the thinking table matches (internal/thinking) and one that
		// can stop thinking; claude-e2e of 6d is not.
		const model = "claude-haiku-5-5"
		code, body := admin(ctx, http.MethodPost, "/prices", `{"name":"fake-claude-haiku-5-5","provider":"anthropic","match":{"glob":"`+model+`"},`+
			`"prices":{"input":"3.00","output":"15.00","cacheRead":"0.30","cacheWrite":"3.75"},"maxOutput":4096}`)
		Expect(code).To(Equal(http.StatusCreated), string(body))

		modelPath := "/agents/" + agent.GetId() + "/model"
		thinkingPath := "/agents/" + agent.GetId() + "/thinking"
		DeferCleanup(func(ctx context.Context) {
			code, body := admin(ctx, http.MethodPut, thinkingPath, `{"thinking":"default"}`)
			Expect(code).To(Equal(http.StatusOK), string(body))
			code, body = admin(ctx, http.MethodPut, modelPath, `{"model":"default"}`)
			Expect(code).To(Equal(http.StatusOK), string(body))
		})
		Eventually(func(g Gomega) {
			code, body := admin(ctx, http.MethodPut, modelPath, `{"model":"`+model+`"}`)
			g.Expect(code).To(Equal(http.StatusOK), string(body))
		}).WithContext(ctx).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())

		fakeModel := strings.TrimRight(os.Getenv("KOINON_E2E_FAKEMODEL_URL"), "/")
		Expect(fakeModel).NotTo(BeEmpty(), "KOINON_E2E_FAKEMODEL_URL")
		runTask := func(title string) {
			GinkgoHelper()
			res, err := c.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
				OrgId: org, AgentId: agent.GetId(), Title: title, Message: "Run the cache probe and say when it is done.",
			})
			Expect(err).NotTo(HaveOccurred())
			var seq int64
			var evKinds []string
			cw := watchRun(context.WithoutCancel(ctx), c, org, res.GetRun().GetId(), 0)
			done := cw.until(ctx, c, org, res.GetRun().GetId(), 8*time.Minute, &seq, &evKinds, func(r *runv1alpha1.Run) bool {
				return terminal(r.GetState())
			})
			Expect(done.GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_SUCCEEDED), done.GetOutcomeReason())
		}
		count := func() int {
			var n int
			Eventually(func(g Gomega) {
				n = len(fakeModelRequests(ctx, g, fakeModel, model))
			}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
			return n
		}

		By("thinking on: every request carries adaptive thinking with a summary, and no beta reaches the model")
		code, body = admin(ctx, http.MethodPut, thinkingPath, `{"thinking":"on"}`)
		Expect(code).To(Equal(http.StatusOK), string(body))
		runTask("Thinking on")
		var on []fakeRequest
		Eventually(func(g Gomega) {
			on = fakeModelRequests(ctx, g, fakeModel, model)
			g.Expect(len(on)).To(BeNumerically(">=", 2), "the task's request and the end-of-turn note")
		}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
		for _, r := range on {
			Expect(r.Path).To(Equal("/v1/messages"))
			Expect(r.Thinking).To(Equal("adaptive"), "%+v", r)
			Expect(r.ThinkingDisplay).To(Equal("summarized"), "%+v", r)
			Expect(r.Betas).To(BeEmpty(), "no row pins a beta and the gateway drops the client's: %+v", r)
		}

		By("thinking off: every request turns thinking off explicitly")
		before := count()
		code, body = admin(ctx, http.MethodPut, thinkingPath, `{"thinking":"off"}`)
		Expect(code).To(Equal(http.StatusOK), string(body))
		runTask("Thinking off")
		var all []fakeRequest
		Eventually(func(g Gomega) {
			all = fakeModelRequests(ctx, g, fakeModel, model)
			g.Expect(len(all)).To(BeNumerically(">=", before+2))
		}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
		for _, r := range all[before:] {
			Expect(r.Path).To(Equal("/v1/messages"))
			Expect(r.Thinking).To(Equal(string(thinking.Disabled)), "%+v", r)
		}
		t.end("8. Adaptive thinking on and off: strict fake behind the real gateway")
	})
	// Not part of the quickstart (old issue 262). An agent holds its gateway key and is untrusted, so it can put
	// the key in a header of its own or in the URL; the upstream is a third party that logs both.
	It("8. the gateway sends the model upstream only the headers a format needs, and refuses an agent key in a header or the URL (old issue 262)", func(ctx SpecContext) {
		t.begin()
		// Step 8 priced claude-haiku-5-5 and the reconciler serves it. The request is what a sandbox
		// would send, from here through the gateway's Service with the agent's own key.
		const model = "claude-haiku-5-5"
		kubectl := os.Getenv("KOINON_E2E_KUBECTL")
		Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL")
		fakeModel := strings.TrimRight(os.Getenv("KOINON_E2E_FAKEMODEL_URL"), "/")
		Expect(fakeModel).NotTo(BeEmpty(), "KOINON_E2E_FAKEMODEL_URL")
		key := agentGatewayKey(ctx, kubectl, org, agent.GetId())
		local := portForward(ctx, kubectl, "svc/koinon-gateway", "8080")

		send := func(query string, header ...string) (int, string) {
			GinkgoHelper()
			body := `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+local+"/v1/messages"+query, strings.NewReader(body))
			Expect(err).NotTo(HaveOccurred())
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			for i := 0; i+1 < len(header); i += 2 {
				req.Header.Set(header[i], header[i+1])
			}
			res, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = res.Body.Close() }()
			out, _ := io.ReadAll(res.Body)
			return res.StatusCode, string(out)
		}
		count := func() int {
			var n int
			Eventually(func(g Gomega) { n = len(fakeModelRequests(ctx, g, fakeModel, model)) }).
				WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
			return n
		}

		By("a request with headers of the agent's own choosing reaches the model without them")
		before := count()
		Eventually(func(g Gomega) {
			code, out := send("", "X-Custom", "hello", "X-Forwarded-For", "203.0.113.9", "User-Agent", "koinon-e2e")
			g.Expect(code).To(Equal(http.StatusOK), out)
		}).WithContext(ctx).WithTimeout(3 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
		var got []fakeRequest
		Eventually(func(g Gomega) {
			got = fakeModelDetail(ctx, g, fakeModel, model)
			g.Expect(len(got)).To(BeNumerically(">", before))
		}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
		for _, r := range got[before:] {
			for name, values := range r.Headers {
				for _, v := range values {
					Expect(v).NotTo(ContainSubstring("kgw_"), "header %s reached the upstream: %+v", name, r.Headers)
				}
			}
			for _, name := range []string{"X-Custom", "X-Forwarded-For"} {
				Expect(r.Headers).NotTo(HaveKey(name), "the upstream got the header the agent invented")
			}
			Expect(r.Headers.Get("Content-Type")).To(Equal("application/json"))
		}

		By("a request with the key in any header but Authorization is refused before it reaches the model")
		n := count()
		for _, h := range [][2]string{
			{"X-Canary", key},
			{"X-Embedded", "prefix-" + key + "-suffix"},
			{"X-Api-Key", key},
			{"Cookie", "session=" + key},
			{"User-Agent", "harness/1.0 (" + key + ")"},
		} {
			code, out := send("", h[0], h[1])
			Expect(code).To(Equal(http.StatusForbidden), h[0]+": "+out)
		}
		Expect(count()).To(Equal(n), "nothing reached the upstream")

		By("a request with the key in the query or the path is refused before it reaches the model")
		n = count()
		for _, q := range []string{"?canary=" + key, "?canary=" + strings.Replace(key, "_", "%5F", 1)} {
			code, out := send(q)
			Expect(code).To(Equal(http.StatusForbidden), out)
		}
		Expect(count()).To(Equal(n), "nothing reached the upstream")
		for _, r := range fakeModelDetail(ctx, Default, fakeModel, model) {
			Expect(r.Query).NotTo(ContainSubstring("kgw_"))
		}
		t.end("8. Gateway header allowlist and key-in-header/URL refusal")
	})

	// Not part of the quickstart, and after its time check: the flavor image is broken for the rest of
	// the run (the cluster is deleted afterwards).
	It("8. a flavor image that cannot be pulled fails the run with the cause image_pull, read from the pod by the sandbox observer", func(ctx SpecContext) {
		t.begin()
		kubectl := os.Getenv("KOINON_E2E_KUBECTL")
		Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL")
		kc := func(args ...string) string {
			GinkgoHelper()
			out, err := exec.CommandContext(ctx, kubectl, args...).CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "kubectl %s: %s", strings.Join(args, " "), out)
			return strings.TrimSpace(string(out))
		}

		By("the reconciler bound the observer role in the workspace namespace, to itself only")
		orgID, err := uuid.Parse(org)
		Expect(err).NotTo(HaveOccurred())
		// This org's own workspace, not any OpenShell namespace: other suites (the escape hook of step 6b)
		// make workspaces of their own in the same cluster.
		ws := execution.WorkspaceName(orgID)
		ns := kc("get", "ns", "-l", "openshell.ai/managed-by=openshell,openshell.ai/sandbox-workspace="+ws, "-o", "jsonpath={.items[0].metadata.name}")
		Expect(ns).To(Equal("openshell-openshell-" + ws))
		Expect(kc("-n", ns, "get", "rolebinding", "koinon-koinon-sandbox-observer", "-o",
			"jsonpath={.roleRef.kind}/{.roleRef.name} {range .subjects[*]}{.kind}:{.namespace}/{.name} {end}")).
			To(Equal("ClusterRole/koinon-koinon-sandbox-observer ServiceAccount:koinon/koinon-reconciler"))

		By("pointing the flavor at an image nobody can pull, and shortening the sandbox wait cap")
		cfgYAML := kc("-n", "koinon", "get", "cm", "koinon-config", "-o", "jsonpath={.data.config\\.yaml}")
		broken := regexp.MustCompile(`(?m)^(\s+image: ).*flavors/opencode.*$`).ReplaceAllString(cfgYAML, "${1}kind.local/koinon-escape/agent:nope")
		Expect(broken).NotTo(Equal(cfgYAML), "the flavor image line")
		patch, err := json.Marshal(map[string]any{"data": map[string]string{"config.yaml": broken}})
		Expect(err).NotTo(HaveOccurred())
		kc("-n", "koinon", "patch", "cm", "koinon-config", "--type", "merge", "-p", string(patch))
		kc("-n", "koinon", "rollout", "restart", "deploy/koinon-reconciler")
		// 30 s, not the 2 minutes this step used to wait out in every run (#572). The cap counts from the
		// run's first claim (internal/agentd/sandbox.go), and the failure names its cause from the last pod
		// reason the observer read, so the cap must outlast: the sandbox render and reconcile (a healthy
		// sandbox in this cluster is ready in about 3 s), the kubelet's first failed pull of an image on
		// a host that does not exist (ErrImagePull, seconds), and one observer read per 1 s poll (the org's
		// workspace namespace already has its RoleBinding, so the 10 s RoleBinding backoff does not apply).
		// That chain is well under 10 s; 30 s leaves three times that. A cap that is too short fails here
		// as cause "unknown" instead of "image_pull". Only CI can show that 30 s is stable on its runners.
		kc("-n", "koinon", "set", "env", "deploy/koinon-agentd", "KOINON_SANDBOX_WAIT_CAP=30s")
		kc("-n", "koinon", "rollout", "status", "deploy/koinon-reconciler", "--timeout=3m")
		kc("-n", "koinon", "rollout", "status", "deploy/koinon-agentd", "--timeout=3m")

		By("a task for a new agent: its sandbox is rendered with the broken image")
		ts, err := c.Templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{})
		Expect(err).NotTo(HaveOccurred())
		var tmpl string
		for _, tp := range ts.GetTemplates() {
			if tp.GetSlug() == "repo-helper" {
				tmpl = tp.GetId()
			}
		}
		Expect(tmpl).NotTo(BeEmpty())
		created, err := c.Agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: org, TemplateId: tmpl, DisplayName: "Missing image", Flavor: templatev1alpha1.Flavor_FLAVOR_OPENCODE,
			Grants: []*agentv1alpha1.GrantBinding{{Connector: "github", ConnectionId: conn.GetId(), Repositories: []string{repository}}},
		})
		Expect(err).NotTo(HaveOccurred())
		bad := created.GetAgent()
		_, err = c.Agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: bad.GetId(), Version: bad.GetLatestVersion()})
		Expect(err).NotTo(HaveOccurred())
		res, err := c.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
			OrgId: org, AgentId: bad.GetId(), Title: "Never starts", Message: "Say hello.",
		})
		Expect(err).NotTo(HaveOccurred())
		var seq int64
		var evKinds []string
		bw := watchRun(context.WithoutCancel(ctx), c, org, res.GetRun().GetId(), 0)
		failed := bw.until(ctx, c, org, res.GetRun().GetId(), 8*time.Minute, &seq, &evKinds, func(r *runv1alpha1.Run) bool {
			return terminal(r.GetState())
		})

		Expect(failed.GetState()).To(Equal(runv1alpha1.RunState_RUN_STATE_FAILED))
		Expect(failed.GetFailureClass()).To(Equal(runv1alpha1.FailureClass_FAILURE_CLASS_SANDBOX_UNAVAILABLE))
		Expect(failed.GetOutcomeReason()).To(ContainSubstring("the image could not be pulled"))
		// The kubelet alternates between ErrImagePull and ImagePullBackOff while it retries.
		Expect(failed.GetOutcomeReason()).To(MatchRegexp(`ErrImagePull|ImagePullBackOff`), "the pod's reason is in the detail")
		Expect(failed.GetOutcomeReason()).NotTo(ContainSubstring("ConfigurationPending"), "not OpenShell's opaque reason")
		got, err := c.Tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: res.GetTask().GetId()})
		Expect(err).NotTo(HaveOccurred())
		var cause string
		for _, ev := range got.GetEvents() {
			if ev.GetKind() == "run.failed" {
				cause, _ = ev.GetPayload().AsMap()["sandboxCause"].(string)
			}
		}
		Expect(cause).To(Equal("image_pull"))

		By("the metrics count it, by cause")
		metrics := func(target, port string) string {
			GinkgoHelper()
			res, err := http.Get("http://" + portForward(ctx, kubectl, target, port) + "/metrics")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = res.Body.Close() }()
			b, _ := io.ReadAll(res.Body)
			return string(b)
		}
		Expect(metrics("deploy/koinon-agentd", "9090")).To(MatchRegexp(`(?m)^koinon_agentd_sandbox_wait_cap_total\{cause="image_pull"\} [1-9]`))
		Expect(metrics("deploy/koinon-reconciler", "9090")).To(MatchRegexp(`(?m)^koinon_reconciler_sandbox_observer_errors_total\{reason="forbidden"\} 0$`),
			"the observer read the pod without being refused")
		t.end("8. Missing flavor image: image_pull from the pod, through the observer")
	})
})

// fakeModelPaths returns the URL paths of the requests the fake model server received for model,
// from its /_fake/requests listing.
func fakeModelPaths(ctx context.Context, g Gomega, base, model string) []string {
	var out []string
	for _, r := range fakeModelRequests(ctx, g, base, model) {
		out = append(out, r.Path)
	}
	return out
}

// fakeRequest is one request the fake model server received: the path, the thinking type and
// display of the body ("" for none) and the anthropic-beta values.
type fakeRequest struct {
	Path, Model, Thinking, ThinkingDisplay string
	Betas                                  []string
	// Query and Headers are filled by fakeModelDetail (?detail=1).
	Query   string
	Headers http.Header
}

// fakeModelRequests returns the requests the fake model server got for model, in arrival order.
func fakeModelRequests(ctx context.Context, g Gomega, base, model string) []fakeRequest {
	return fakeModelList(ctx, g, base+"/_fake/requests", model)
}

// fakeModelDetail is fakeModelRequests with the query and every header of each request.
func fakeModelDetail(ctx context.Context, g Gomega, base, model string) []fakeRequest {
	return fakeModelList(ctx, g, base+"/_fake/requests?detail=1", model)
}

func fakeModelList(ctx context.Context, g Gomega, url, model string) []fakeRequest {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	g.Expect(err).NotTo(HaveOccurred())
	res, err := http.DefaultClient.Do(req)
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { _ = res.Body.Close() }()
	g.Expect(res.StatusCode).To(Equal(http.StatusOK))
	var list []fakeRequest
	g.Expect(json.NewDecoder(res.Body).Decode(&list)).To(Succeed())
	var out []fakeRequest
	for _, r := range list {
		if r.Model == model {
			out = append(out, r)
		}
	}
	return out
}

// ledgerRow is one spend ledger entry (spend_entries).
func totalsOf(c *runv1alpha1.Cost) scenarios.Totals {
	return scenarios.Totals{
		Requests: c.GetRequests(), EstimatedRequests: c.GetEstimatedRequests(),
		InputTokens: c.GetInputTokens(), OutputTokens: c.GetOutputTokens(),
	}
}

type ledgerRow struct {
	Model                                string
	Input, Output, CacheRead, CacheWrite int64
	Cost                                 *big.Rat
	// UpstreamStatus is the provider's error status, 0 when it answered (ADR 0056 amendment
	// 2026-10-29); ChargeWaived says why a refusal was charged nothing, "" when it was not waived.
	UpstreamStatus int
	ChargeWaived   string
}

// ledgerRows reads the run's spend entries straight from the primary Postgres pod, as its local
// superuser: the admin API reports cost, not cache tokens.
func ledgerRows(ctx context.Context, g Gomega, kubectl, run string) []ledgerRow {
	pod, err := exec.CommandContext(ctx, kubectl, "-n", "koinon", "get", "pod", "-l", "cnpg.io/instanceRole=primary", "-o", "name").Output()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(strings.TrimSpace(string(pod))).NotTo(BeEmpty(), "the primary Postgres pod")
	query := "SELECT model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd::text, coalesce(upstream_status, 0), coalesce(estimate->>'charge_waived', '') FROM spend_entries WHERE run_id = '" + run + "' ORDER BY occurred_at"
	out, err := exec.CommandContext(ctx, kubectl, "-n", "koinon", "exec", strings.TrimSpace(string(pod)), "-c", "postgres", "--",
		"psql", "-U", "postgres", "-d", "koinon", "-At", "-F", "|", "-c", query).CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(), string(out))
	var rows []ledgerRow
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "|")
		if len(f) != 8 {
			continue
		}
		r := ledgerRow{Model: f[0]}
		for i, dst := range []*int64{&r.Input, &r.Output, &r.CacheRead, &r.CacheWrite} {
			n, err := strconv.ParseInt(f[i+1], 10, 64)
			g.Expect(err).NotTo(HaveOccurred())
			*dst = n
		}
		var ok bool
		r.Cost, ok = new(big.Rat).SetString(f[5])
		g.Expect(ok).To(BeTrue(), line)
		r.UpstreamStatus, err = strconv.Atoi(f[6])
		g.Expect(err).NotTo(HaveOccurred())
		r.ChargeWaived = f[7]
		rows = append(rows, r)
	}
	return rows
}

// rate is tokens at a USD price per million tokens.
func rate(perMillion string, tokens int64) *big.Rat {
	p, _ := new(big.Rat).SetString(perMillion)
	return p.Mul(p, big.NewRat(tokens, 1_000_000))
}

// entry is the row as test/scenarios checks it. The psql query does not read the estimated flag:
// the run's EstimatedRequests total, checked beside it, is where an estimate shows.
func (r ledgerRow) entry() scenarios.Entry {
	return scenarios.Entry{
		UpstreamStatus: r.UpstreamStatus, Input: r.Input, Output: r.Output,
		CostZero: r.Cost.Sign() == 0, ChargeWaived: r.ChargeWaived,
	}
}
