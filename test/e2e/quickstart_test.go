// SPDX-License-Identifier: Apache-2.0

package e2e_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	approvalv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/approval/v1alpha1"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	connectionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/connection/v1alpha1"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	ticketv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/ticket/v1alpha1"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
)

// What the fake model script test/fakemodel/scripts/repo-helper.yaml does, and the fake
// GitHub's repository (test/e2e/kind/fakes.yaml).
const (
	repoName   = "hello"
	repository = "koinon-demo/" + repoName
	prBranch   = "koinon/add-hello-md"
	prFile     = "hello.md"
	prContent  = "hello from koinon\n"
)

// quickstartBudget is plan item E1's bound: the quickstart takes at most 15 minutes from
// `helm install`.
const quickstartBudget = 15 * time.Minute

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

func watchRun(ctx context.Context, c *client, org, run string, after int64) *watcher {
	stream, err := c.runs.WatchRun(ctx, &runv1alpha1.WatchRunRequest{OrgId: org, RunId: run, AfterSeq: after})
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
func (w *watcher) until(ctx context.Context, c *client, org, run string, timeout time.Duration, lastSeq *int64, kinds *[]string,
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
			got, err := c.runs.GetRun(ctx, &runv1alpha1.GetRunRequest{OrgId: org, RunId: run})
			Expect(err).NotTo(HaveOccurred())
			if cond(got.GetRun()) {
				return got.GetRun()
			}
			if terminal(got.GetRun().GetState()) {
				Fail(fmt.Sprintf("run ended %s: %s", got.GetRun().GetState(), got.GetRun().GetOutcomeReason()))
			}
		case <-deadline:
			got, _ := c.runs.GetRun(ctx, &runv1alpha1.GetRunRequest{OrgId: org, RunId: run})
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
		c        *client
		gh       fakeGitHub
		t        timings
		org      string
		conn     *connectionv1alpha1.Connection
		agent    *agentv1alpha1.Agent
		ticket   *ticketv1alpha1.Ticket
		run      *runv1alpha1.Run
		w        *watcher
		lastSeq  int64
		kinds    []string
		approval *approvalv1alpha1.Approval
		prURL    string
		// admin calls /admin/v1 with a token made at step 1b.
		admin func(ctx context.Context, method, path string, body ...string) (int, []byte)
		// mcpServerID is the remote MCP server step 6b added and 6c uses.
		mcpServerID string
	)

	BeforeAll(func() {
		c = newClient(strings.TrimRight(os.Getenv("KOINON_E2E_API_URL"), "/"))
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

	It("1. logs in as the bootstrap admin and sets a new password", func(ctx SpecContext) {
		t.begin()
		password := os.Getenv("KOINON_E2E_PASSWORD")
		Expect(password).NotTo(BeEmpty(), "KOINON_E2E_PASSWORD")
		Expect(c.login(ctx, "admin", password)).To(BeTrue(), "the one-time password must be changed first")

		buf := make([]byte, 16)
		_, _ = rand.Read(buf)
		_, err := c.auth.ChangePassword(ctx, &authv1alpha1.ChangePasswordRequest{
			CurrentPassword: password, NewPassword: "e2e-" + hex.EncodeToString(buf),
		})
		Expect(err).NotTo(HaveOccurred())

		me, err := c.auth.WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
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

	It("1b. sees the model provider the quickstart declared, through the admin API", func(ctx SpecContext) {
		t.begin()
		// The quickstart's model-provider.yaml is a labelled Secret that koinon-api imported at start.
		// A token made with the session reads it back through /admin/v1, read-only.
		tok, err := c.tokens.CreateToken(ctx, &tokenv1alpha1.CreateTokenRequest{
			OrgId: org, Name: "e2e", Role: authv1alpha1.OrgRole_ORG_ROLE_ADMIN,
		})
		Expect(err).NotTo(HaveOccurred())
		admin = func(ctx context.Context, method, path string, body ...string) (int, []byte) {
			var rdr io.Reader
			if len(body) > 0 {
				rdr = strings.NewReader(body[0])
			}
			req, err := http.NewRequestWithContext(ctx, method, c.base+"/admin/v1"+path, rdr)
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
			g.Expect(providers.Providers).To(HaveLen(1))
		}).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
		Expect(providers.Providers[0].Name).To(Equal("fake"))
		Expect(providers.Providers[0].Source).To(Equal("secret"))
		Expect(providers.Providers[0].ManagedBySecret).To(Equal("fake-model"))
		code, body := admin(ctx, http.MethodDelete, "/providers/"+providers.Providers[0].ID)
		Expect(code).To(Equal(http.StatusConflict), "declared providers are read-only: %s", body)
		// A model has no rates of its own; its price is an org price (or a default one). The fake model
		// is in no default list, so the run is priced by this entry and the ledger's estimates work.
		code, body = admin(ctx, http.MethodPost, "/prices", `{"name":"fake-repo-helper","provider":"openai","match":{"glob":"repo-helper"},`+
			`"prices":{"input":"0.30","output":"1.20"},"maxOutput":4096}`)
		Expect(code).To(Equal(http.StatusCreated), string(body))
		t.end("1b. Declared model provider and its price (admin API)")
	})

	It("2. creates and tests the GitHub connection", func(ctx SpecContext) {
		t.begin()
		app := gh.app(ctx)
		res, err := c.connections.CreateGitHubConnection(ctx, &connectionv1alpha1.CreateGitHubConnectionRequest{
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

	It("3. creates a repo-helper agent bound to the repository and activates it", func(ctx SpecContext) {
		t.begin()
		ts, err := c.templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{})
		Expect(err).NotTo(HaveOccurred())
		var tmpl string
		for _, tp := range ts.GetTemplates() {
			if tp.GetSlug() == "repo-helper" {
				tmpl = tp.GetId()
			}
		}
		Expect(tmpl).NotTo(BeEmpty(), "the repo-helper template")

		created, err := c.agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: org, TemplateId: tmpl, DisplayName: "Repo helper",
			Flavor: templatev1alpha1.Flavor_FLAVOR_OPENCODE,
			Grants: []*agentv1alpha1.GrantBinding{{Connector: "github", ConnectionId: conn.GetId(), Repositories: []string{repository}}},
		})
		Expect(err).NotTo(HaveOccurred())
		agent = created.GetAgent()
		Expect(agent.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_DRAFT))

		activated, err := c.agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{
			OrgId: org, AgentId: agent.GetId(), Version: agent.GetLatestVersion(),
		})
		Expect(err).NotTo(HaveOccurred())
		agent = activated.GetAgent()
		Expect(agent.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_ACTIVE))
		t.end("3. Agent from template, activated")
	})

	It("4. creates a ticket, streams the run and parks on an approval", func(ctx SpecContext) {
		t.begin()
		res, err := c.tickets.CreateTicket(ctx, &ticketv1alpha1.CreateTicketRequest{
			OrgId: org, AgentId: agent.GetId(), Title: "Add hello.md",
			Message: "Please add a hello.md to " + repository + " that says hello from koinon.",
		})
		Expect(err).NotTo(HaveOccurred())
		ticket, run = res.GetTicket(), res.GetRun()
		Expect(run.GetId()).NotTo(BeEmpty())

		w = watchRun(context.WithoutCancel(ctx), c, org, run.GetId(), 0)
		run = w.until(ctx, c, org, run.GetId(), 8*time.Minute, &lastSeq, &kinds, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_PARKED
		})
		Expect(run.GetParkReason()).To(Equal(runv1alpha1.ParkReason_PARK_REASON_APPROVAL))
		Expect(kinds).To(ContainElement("acp.tool_call"), "the run streamed the agent's tool call")

		list, err := c.approvals.ListApprovals(ctx, &approvalv1alpha1.ListApprovalsRequest{OrgId: org, TicketId: ticket.GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetApprovals()).To(HaveLen(1))
		// The list is a summary; the approval card reads the call itself.
		got, err := c.approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: list.GetApprovals()[0].GetId()})
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
			got, err := c.runs.GetRun(ctx, &runv1alpha1.GetRunRequest{OrgId: org, RunId: run.GetId()})
			g.Expect(err).NotTo(HaveOccurred())
			cost = got.GetRun().GetCost()
			g.Expect(cost.GetRequests()).To(BeNumerically(">", 0), "the ledger recorded the run's requests")
		}, 60*time.Second, time.Second).Should(Succeed())
		// The streamed requests carry usage through the gateway; the title request the fake model
		// refuses (a 400 without usage) is charged its input only. OpenCode asks for a title with each
		// prompt until it has one, so the turn's prompt and the end-of-turn note's prompt make exactly
		// two such requests; a third would be a request nobody accounts for.
		Expect(cost.GetRequests()-cost.GetEstimatedRequests()).To(BeNumerically(">=", 1), "streamed requests reported their usage")
		Expect(cost.GetEstimatedRequests()).To(BeNumerically("<=", 2))
		Expect(cost.GetEstimatedRequests()).To(BeNumerically(">=", 1))
		Expect(cost.GetInputTokens()).To(BeNumerically(">=", 1200), "the fake model's reported input tokens")
		Expect(cost.GetOutputTokens()).To(BeNumerically("<", 1000), "no output is charged for the refused request")

		By("nothing has reached GitHub before the approval")
		repo := gh.repo(ctx, repoName)
		Expect(repo.Pulls).To(BeEmpty())
		Expect(repo.Refs).NotTo(HaveKey(prBranch))
		t.end("4. Ticket → run streamed → approval pending")
	})

	It("5. approves, and Koinon opens exactly one branch, commit and pull request", func(ctx SpecContext) {
		t.begin()
		_, err := c.approvals.Approve(ctx, &approvalv1alpha1.ApproveRequest{
			OrgId: org, ApprovalId: approval.GetId(), ArgsSha256: approval.GetArgsSha256(),
		})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			got, err := c.approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: approval.GetId()})
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

	It("6. resumes the run, which posts the pull request URL on the ticket", func(ctx SpecContext) {
		t.begin()
		run = w.until(ctx, c, org, run.GetId(), 5*time.Minute, &lastSeq, &kinds, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_SUCCEEDED
		})
		got, err := c.tickets.GetTicket(ctx, &ticketv1alpha1.GetTicketRequest{OrgId: org, TicketId: ticket.GetId()})
		Expect(err).NotTo(HaveOccurred())
		var replies []string
		for _, ev := range got.GetEvents() {
			if ev.GetKind() == "message" && ev.GetActorKind() == ticketv1alpha1.ActorKind_ACTOR_KIND_AGENT {
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
		t.end("6. Run resumes → PR URL on the ticket")
	})

	It("6a. the agent's end-of-turn note is on the ticket, not in its timeline as a reply", func(ctx SpecContext) {
		t.begin()
		// The note is written after each turn (the run parked once, then finished: two turns), so
		// it is there by the time the run has succeeded; the last one wins.
		var got *ticketv1alpha1.GetTicketResponse
		Eventually(func(g Gomega) {
			var err error
			got, err = c.tickets.GetTicket(ctx, &ticketv1alpha1.GetTicketRequest{OrgId: org, TicketId: ticket.GetId()})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.GetTicket().GetAgentSummary()).To(HavePrefix("Notes: the task asks for a hello.md"))
		}, 30*time.Second, time.Second).Should(Succeed())
		Expect(got.GetTicket().GetAgentSummaryAt()).NotTo(BeNil())
		for _, ev := range got.GetEvents() {
			if ev.GetKind() == "message" {
				text, _ := ev.GetPayload().AsMap()["text"].(string)
				Expect(text).NotTo(HavePrefix("Notes:"), "the note is not a reply on the ticket")
			}
		}
		By("the note's model call went through the gateway and was counted with the run")
		Eventually(func(g Gomega) {
			r, err := c.runs.GetRun(ctx, &runv1alpha1.GetRunRequest{OrgId: org, RunId: run.GetId()})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(r.GetRun().GetCost().GetRequests()).To(BeNumerically(">=", 4), "title, two turns and at least one note")
		}, 60*time.Second, time.Second).Should(Succeed())
		t.end("6a. Agent's notes on the ticket")
	})

	It("6a2. the saved-work upload Service exists and answers; the tools port does not serve it", func(ctx SpecContext) {
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
		t.end("6a2. Saved-work upload Service")
	})

	It("6b. adds a remote MCP server: the gateway injects its credential, and an agent's call reaches it", func(ctx SpecContext) {
		t.begin()
		mcpServerID = mcpServerE2E(ctx, admin, org, agent.GetId())
		t.end("6b. Remote MCP server (add, health, tiers, allowlist, call through the gateway)")
	})

	It("6c. an agent's call to an ask-tier MCP tool parks, is approved, and runs once through the gateway with the exact arguments", func(ctx SpecContext) {
		t.begin()
		Expect(mcpServerID).NotTo(BeEmpty(), "step 6b adds the server")
		fake := strings.TrimRight(os.Getenv("KOINON_E2E_FAKEMCP_URL"), "/")
		res, err := c.tickets.CreateTicket(ctx, &ticketv1alpha1.CreateTicketRequest{
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

		list, err := c.approvals.ListApprovals(ctx, &approvalv1alpha1.ListApprovalsRequest{OrgId: org, TicketId: res.GetTicket().GetId()})
		Expect(err).NotTo(HaveOccurred())
		Expect(list.GetApprovals()).To(HaveLen(1))
		got, err := c.approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: list.GetApprovals()[0].GetId()})
		Expect(err).NotTo(HaveOccurred())
		ap := got.GetApproval()
		Expect(ap.GetConnector()).To(Equal("mcp"))
		Expect(ap.GetTool()).To(Equal("write_item"))
		Expect(ap.GetTarget()).To(Equal("mcp:tracker"))
		Expect(ap.GetArgsJson()).To(Equal(`{"title":"from the e2e ask"}`))
		Expect(ap.GetMcp().GetContentHash()).NotTo(BeEmpty())
		Expect(ap.GetMcp().GetContentHash()).To(Equal(ap.GetMcp().GetCurrentContentHash()))
		for _, r := range fakeMCPRequests(ctx, fake) {
			Expect(r.Tool).NotTo(Equal("write_item"), "nothing reached the server before the approval")
		}

		_, err = c.approvals.Approve(ctx, &approvalv1alpha1.ApproveRequest{OrgId: org, ApprovalId: ap.GetId(), ArgsSha256: ap.GetArgsSha256()})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			a, err := c.approvals.GetApproval(ctx, &approvalv1alpha1.GetApprovalRequest{OrgId: org, ApprovalId: ap.GetId()})
			g.Expect(err).NotTo(HaveOccurred())
			ap = a.GetApproval()
			g.Expect(ap.GetStatus()).To(Equal(approvalv1alpha1.Status_STATUS_EXECUTED), ap.GetError())
		}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
		Expect(ap.GetExecutions()).To(BeEquivalentTo(1))

		var calls []fakeMCPRecord
		for _, r := range fakeMCPRequests(ctx, fake) {
			if r.Tool == "write_item" {
				calls = append(calls, r)
			}
		}
		Expect(calls).To(HaveLen(1), "the server saw the approved call exactly once")
		Expect(calls[0].Args).To(Equal(`{"title":"from the e2e ask"}`))
		Expect(calls[0].Agent).To(Equal("default/mcp-exec"), "through the gateway, with the org's executor key")
		Expect(calls[0].Authorized).To(BeTrue())

		mw.until(ctx, c, org, res.GetRun().GetId(), 5*time.Minute, &seq, &evKinds, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_SUCCEEDED
		})
		code, body := admin(ctx, http.MethodDelete, "/mcp-servers/"+mcpServerID)
		Expect(code).To(Equal(http.StatusNoContent), string(body))
		t.end("6c. Ask-tier MCP call: parks, approved, runs once through the gateway")
	})

	It("7. took at most 15 minutes from helm install", func() {
		total := time.Since(t.t0)
		Expect(total).To(BeNumerically("<=", quickstartBudget),
			"the quickstart took %s from helm install, more than the plan's %s", total.Round(time.Second), quickstartBudget)
	})
})
