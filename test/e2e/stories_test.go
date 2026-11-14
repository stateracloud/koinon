// SPDX-License-Identifier: Apache-2.0

package e2e_test

// The run-1 story suite (issue 992, test plan W8a). Run 1 of the lab drives stories through the
// generated Connect clients against a real install: an agent from a template, an MCP tool in the ask
// tier, a task created by another user with a question the creator answers, a run canceled mid-turn and
// resumed, and the trace and org settings. A wrong RPC or field found on the lab cost a lab run each
// time. This suite drives the same calls on kind with the fake model, so it is a compile error or a red
// night instead.
//
// Every It names the server rules it asserts, and each rule has a step that fails when the rule is
// removed from the server. The rules marked "unconfirmed" in the test-infrastructure review (2026-11-14)
// are the ones this suite confirms or refutes against a running server.
//
// The suite has label "stories": test/e2e/run.sh runs it as a second driver pass after the quickstart
// (STORIES=1, set by `make test-e2e-kind` and by the nightly schedule), never in a pull request's or the
// merge queue's quickstart. It signs in on its own, so it does not depend on the quickstart having run
// first: with KOINON_E2E_NEW_PASSWORD (run.sh sets it for both passes) it logs in with that password, and
// when that fails because the bootstrap password is still unspent it spends it.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	agentv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/agent/v1alpha1"
	authv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/auth/v1alpha1"
	mcpserverv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/mcpserver/v1alpha1"
	orgsettingsv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/orgsettings/v1alpha1"
	questionv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/question/v1alpha1"
	runv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/run/v1alpha1"
	taskv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/task/v1alpha1"
	templatev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/template/v1alpha1"
	tokenv1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/token/v1alpha1"
	tracev1alpha1 "github.com/stateracloud/koinon/internal/gen/koinon/trace/v1alpha1"
	"github.com/stateracloud/koinon/test/e2e/e2eclient"
)

const (
	// storiesTemplate is the template every story agent comes from: no grant, no MCP server of its own.
	// Its instructions open with "# Web researcher", which the stories-* fake model scripts select on.
	storiesTemplate = "web-researcher"
	// storiesAskModel and storiesSlowModel are declared by kind/model-provider.yaml and scripted by
	// test/fakemodel/scripts/stories-*.yaml.
	storiesAskModel  = "stories-ask"
	storiesSlowModel = "stories-slow"
)

// expectRefused fails unless err is a Connect error with this code and a message that contains want.
func expectRefused(err error, code connect.Code, want string) {
	GinkgoHelper()
	Expect(err).To(HaveOccurred(), "the server must refuse it (%s)", want)
	Expect(connect.CodeOf(err)).To(Equal(code), "got %v", err)
	Expect(err.Error()).To(ContainSubstring(want))
}

// storiesSignIn returns a client logged in as the bootstrap admin, and the default org's ID.
func storiesSignIn(ctx context.Context, base string) (*e2eclient.Client, string) {
	GinkgoHelper()
	c := e2eclient.New(base)
	newPassword := os.Getenv("KOINON_E2E_NEW_PASSWORD")
	Expect(newPassword).NotTo(BeEmpty(), "KOINON_E2E_NEW_PASSWORD (test/e2e/run.sh sets it)")
	if _, err := c.Login(ctx, "admin", newPassword); err != nil {
		// The quickstart has not run: spend the one-time password.
		boot := os.Getenv("KOINON_E2E_PASSWORD")
		Expect(boot).NotTo(BeEmpty(), "KOINON_E2E_PASSWORD")
		required, err := c.Login(ctx, "admin", boot)
		Expect(err).NotTo(HaveOccurred())
		Expect(required).To(BeTrue(), "the one-time password must be changed first")
		_, err = c.Auth.ChangePassword(ctx, &authv1alpha1.ChangePasswordRequest{CurrentPassword: boot, NewPassword: newPassword})
		Expect(err).NotTo(HaveOccurred())
	}
	me, err := c.Auth.WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
	Expect(err).NotTo(HaveOccurred())
	Expect(me.GetPasswordChangeRequired()).To(BeFalse())
	for _, m := range me.GetMemberships() {
		if m.GetOrgSlug() == "default" {
			return c, m.GetOrgId()
		}
	}
	Fail("the default org is missing from WhoAmI")
	return nil, ""
}

// awaitRun polls GetRun until ok holds. A run that reached a terminal state ok does not accept ends the wait.
func awaitRun(ctx context.Context, c *e2eclient.Client, org, run string, timeout time.Duration, ok func(*runv1alpha1.Run) bool) *runv1alpha1.Run {
	GinkgoHelper()
	var last *runv1alpha1.Run
	Eventually(func(g Gomega) {
		got, err := c.Runs.GetRun(ctx, &runv1alpha1.GetRunRequest{OrgId: org, RunId: run})
		g.Expect(err).NotTo(HaveOccurred())
		last = got.GetRun()
		if ok(last) {
			return
		}
		if terminal(last.GetState()) {
			StopTrying(fmt.Sprintf("run %s ended %s: %s", run, last.GetState(), last.GetOutcomeReason())).Now()
		}
		g.Expect(ok(last)).To(BeTrue(), "run %s is %s (park %s)", run, last.GetState(), last.GetParkReason())
	}).WithContext(ctx).WithTimeout(timeout).WithPolling(2 * time.Second).Should(Succeed())
	return last
}

// agentReplies are the texts of the agent's message events on a task, oldest first.
func agentReplies(ctx context.Context, c *e2eclient.Client, org, task string) []string {
	GinkgoHelper()
	got, err := c.Tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: task})
	Expect(err).NotTo(HaveOccurred())
	var out []string
	for _, e := range got.GetEvents() {
		if e.GetKind() == "message" && e.GetActorKind() == taskv1alpha1.ActorKind_ACTOR_KIND_AGENT {
			out = append(out, e.GetPayload().GetFields()["text"].GetStringValue())
		}
	}
	return out
}

var _ = Describe("the run-1 stories", Ordered, Label("stories"), func() {
	var (
		c        *e2eclient.Client
		org      string
		adminTok string
		tmplID   string
	)

	// admin calls /admin/v1 with a token made at BeforeAll.
	admin := func(ctx context.Context, method, path string, body ...string) (int, []byte) {
		GinkgoHelper()
		code, b, err := c.Admin(ctx, adminTok, method, path, body...)
		Expect(err).NotTo(HaveOccurred())
		return code, b
	}

	// newAgent makes an agent from the web-researcher template by its UUID, activates it, gives it a
	// scripted model and archives it when the story ends (a sandbox per agent is the cost on kind).
	newAgent := func(ctx context.Context, name, model string) string {
		GinkgoHelper()
		created, err := c.Agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: org, TemplateId: tmplID, DisplayName: name,
		})
		Expect(err).NotTo(HaveOccurred())
		a := created.GetAgent()
		_, err = c.Agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: a.GetId(), Version: a.GetLatestVersion()})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func(ctx context.Context) {
			_, _ = c.Agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: a.GetId()})
		})
		set, err := c.Agents.SetAgentModel(ctx, &agentv1alpha1.SetAgentModelRequest{OrgId: org, AgentId: a.GetId(), Model: model})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetModel().GetEffective()).To(Equal(model))
		return a.GetId()
	}

	BeforeAll(func(ctx SpecContext) {
		base := strings.TrimRight(os.Getenv("KOINON_E2E_API_URL"), "/")
		c, org = storiesSignIn(ctx, base)

		tok, err := c.Tokens.CreateToken(ctx, &tokenv1alpha1.CreateTokenRequest{
			OrgId: org, Name: "e2e-stories", Role: authv1alpha1.OrgRole_ORG_ROLE_ADMIN,
		})
		Expect(err).NotTo(HaveOccurred())
		adminTok = tok.GetSecret()

		// The scripted models have no price of their own; a model is served only with one (SetAgentModel).
		code, body := admin(ctx, http.MethodPost, "/prices", `{"name":"fake-stories","provider":"openai","match":{"glob":"stories-*"},`+
			`"prices":{"input":"0.30","output":"1.20"},"maxOutput":4096}`)
		Expect(code).To(Or(Equal(http.StatusCreated), Equal(http.StatusConflict)), string(body))

		ts, err := c.Templates.ListTemplates(ctx, &templatev1alpha1.ListTemplatesRequest{})
		Expect(err).NotTo(HaveOccurred())
		for _, tp := range ts.GetTemplates() {
			if tp.GetSlug() == storiesTemplate {
				tmplID = tp.GetId()
			}
		}
		Expect(tmplID).NotTo(BeEmpty(), "the %s template", storiesTemplate)
	})

	It("S1. makes an agent from a template by its UUID, and refuses the slug, a stale version and a task before activation", func(ctx SpecContext) {
		By("rule: templateId is the template's UUID; the slug is refused")
		_, err := c.Agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: org, TemplateId: storiesTemplate, DisplayName: "stories slug",
		})
		expectRefused(err, connect.CodeInvalidArgument, "template_id is not a UUID")

		By("by its UUID the agent is a draft at version 1")
		created, err := c.Agents.CreateAgentFromTemplate(ctx, &agentv1alpha1.CreateAgentFromTemplateRequest{
			OrgId: org, TemplateId: tmplID, DisplayName: "stories template agent",
		})
		Expect(err).NotTo(HaveOccurred())
		a := created.GetAgent()
		DeferCleanup(func(ctx context.Context) {
			_, _ = c.Agents.ArchiveAgent(ctx, &agentv1alpha1.ArchiveAgentRequest{OrgId: org, AgentId: a.GetId()})
		})
		Expect(a.GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_DRAFT))
		Expect(a.GetTemplateSlug()).To(Equal(storiesTemplate))
		Expect(a.GetLatestVersion()).To(BeEquivalentTo(1))
		Expect(a.GetActiveVersion()).To(BeZero())

		By("rule: a draft agent takes no task")
		_, err = c.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{OrgId: org, AgentId: a.GetId(), Title: "too early", Message: "stories draft"})
		expectRefused(err, connect.CodeFailedPrecondition, "agent is not active")

		By("rule: ActivateAgent needs the agent's latest version; leaving version out is refused")
		_, err = c.Agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: a.GetId()})
		expectRefused(err, connect.CodeFailedPrecondition, "version does not match")
		_, err = c.Agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: a.GetId(), Version: a.GetLatestVersion() + 1})
		expectRefused(err, connect.CodeFailedPrecondition, "version does not match")

		activated, err := c.Agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: a.GetId(), Version: a.GetLatestVersion()})
		Expect(err).NotTo(HaveOccurred())
		Expect(activated.GetAgent().GetStatus()).To(Equal(agentv1alpha1.AgentStatus_AGENT_STATUS_ACTIVE))
		Expect(activated.GetAgent().GetActiveVersion()).To(Equal(a.GetLatestVersion()))

		By("rule: activating an active agent at its active version is refused")
		_, err = c.Agents.ActivateAgent(ctx, &agentv1alpha1.ActivateAgentRequest{OrgId: org, AgentId: a.GetId(), Version: a.GetLatestVersion()})
		expectRefused(err, connect.CodeFailedPrecondition, "already active")
	})

	It("S2. puts every new MCP tool in the ask tier, and refuses an unset tier", func(ctx SpecContext) {
		fakeURL := fakeMCPURL
		By("a server with a credential is added through the Connect client")
		created, err := c.McpServers.CreateServer(ctx, &mcpserverv1alpha1.CreateServerRequest{
			OrgId: org, Name: "stories-tracker", Url: fakeURL,
			CredentialHeader: "X-Api-Key", CredentialValue: fakeMCPKey,
		})
		Expect(err).NotTo(HaveOccurred())
		srv := created.GetServer()
		Expect(srv.GetHasCredential()).To(BeTrue())
		DeferCleanup(func(ctx context.Context) {
			_, _ = c.McpServers.DeleteServer(ctx, &mcpserverv1alpha1.DeleteServerRequest{OrgId: org, ServerId: srv.GetId()})
		})

		By("the prober lists its tools through the gateway and marks the server healthy")
		var tools []*mcpserverv1alpha1.Tool
		Eventually(func(g Gomega) {
			got, err := c.McpServers.ListServers(ctx, &mcpserverv1alpha1.ListServersRequest{OrgId: org})
			g.Expect(err).NotTo(HaveOccurred())
			var health mcpserverv1alpha1.Health
			for _, s := range got.GetServers() {
				if s.GetId() == srv.GetId() {
					health = s.GetHealth()
					g.Expect(health).To(Equal(mcpserverv1alpha1.Health_HEALTH_HEALTHY), s.GetHealthReason())
				}
			}
			g.Expect(health).To(Equal(mcpserverv1alpha1.Health_HEALTH_HEALTHY))
			lt, err := c.McpServers.ListTools(ctx, &mcpserverv1alpha1.ListToolsRequest{OrgId: org, ServerId: srv.GetId()})
			g.Expect(err).NotTo(HaveOccurred())
			tools = lt.GetTools()
			g.Expect(tools).NotTo(BeEmpty())
		}).WithContext(ctx).WithTimeout(8 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		By("rule: every tool a server adds starts in tier ask")
		Expect(tools).To(ConsistOf(
			HaveField("Name", "read_item"), HaveField("Name", "write_item"), HaveField("Name", "fetch_url")))
		for _, t := range tools {
			Expect(t.GetTier()).To(Equal(mcpserverv1alpha1.Tier_TIER_ASK), "tool %s starts in ask", t.GetName())
			Expect(t.GetPresent()).To(BeTrue())
		}

		By("rule: SetToolTier refuses an unset tier")
		_, err = c.McpServers.SetToolTier(ctx, &mcpserverv1alpha1.SetToolTierRequest{OrgId: org, ServerId: srv.GetId(), Tool: "read_item"})
		expectRefused(err, connect.CodeInvalidArgument, "tier must be ask, auto or never")

		By("an admin confirms the read tool as auto, and the others stay in ask")
		set, err := c.McpServers.SetToolTier(ctx, &mcpserverv1alpha1.SetToolTierRequest{
			OrgId: org, ServerId: srv.GetId(), Tool: "read_item", Tier: mcpserverv1alpha1.Tier_TIER_AUTO,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetTool().GetTier()).To(Equal(mcpserverv1alpha1.Tier_TIER_AUTO))
		lt, err := c.McpServers.ListTools(ctx, &mcpserverv1alpha1.ListToolsRequest{OrgId: org, ServerId: srv.GetId()})
		Expect(err).NotTo(HaveOccurred())
		for _, t := range lt.GetTools() {
			want := mcpserverv1alpha1.Tier_TIER_ASK
			if t.GetName() == "read_item" {
				want = mcpserverv1alpha1.Tier_TIER_AUTO
			}
			Expect(t.GetTier()).To(Equal(want), t.GetName())
		}
	})

	It("S3. asks the task's creator, who is another user with a session of their own, and resumes with the answer", func(ctx SpecContext) {
		agent := newAgent(ctx, "stories asker", storiesAskModel)

		By("a second local user, made as an operator does (koinon-api users create), signs in with its own session")
		buf := make([]byte, 6)
		_, _ = rand.Read(buf)
		username := "stories-" + hex.EncodeToString(buf)
		password := "e2e-" + hex.EncodeToString(buf) + "-pw"
		kubectl := os.Getenv("KOINON_E2E_KUBECTL")
		Expect(kubectl).NotTo(BeEmpty(), "KOINON_E2E_KUBECTL")
		cmd := exec.CommandContext(ctx, kubectl, "-n", "koinon", "exec", "-i", "deploy/koinon-api", "--",
			"koinon-api", "users", "create", "--org", "default", "--username", username, "--role", "member",
			"--password-stdin", "--must-change-password=false")
		cmd.Stdin = bytes.NewBufferString(password + "\n")
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "users create: %s", out)
		creator := e2eclient.New(c.Base)
		_, err = creator.Login(ctx, username, password)
		Expect(err).NotTo(HaveOccurred())
		me, err := creator.Auth.WhoAmI(ctx, &authv1alpha1.WhoAmIRequest{})
		Expect(err).NotTo(HaveOccurred())
		creatorID := me.GetUser().GetId()
		Expect(me.GetPlatformAdmin()).To(BeFalse())
		Expect(creator.SessionToken()).NotTo(Equal(c.SessionToken()), "a session of its own")

		By("the creator makes the task; the agent asks with ask_user")
		made, err := creator.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
			OrgId: org, AgentId: agent, Title: "Stories question", Message: "stories ask: plan the change and ask first",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(made.GetTask().GetCreatedBy().GetUserId()).To(Equal(creatorID))
		runID := made.GetRun().GetId()
		Expect(runID).NotTo(BeEmpty(), "the task queued a run")
		parked := awaitRun(ctx, c, org, runID, 6*time.Minute, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_PARKED
		})
		Expect(parked.GetParkReason()).To(Equal(runv1alpha1.ParkReason_PARK_REASON_INPUT_REQUIRED))

		By("rule: the question goes to the task's creator, with the agent's choices and default")
		var q *questionv1alpha1.Question
		Eventually(func(g Gomega) {
			got, err := creator.Questions.ListQuestions(ctx, &questionv1alpha1.ListQuestionsRequest{OrgId: org, TaskId: made.GetTask().GetId()})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.GetQuestions()).To(HaveLen(1))
			q = got.GetQuestions()[0]
			g.Expect(q.GetState()).To(Equal(questionv1alpha1.QuestionState_QUESTION_STATE_OPEN))
		}).WithContext(ctx).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
		Expect(q.GetRunId()).To(Equal(runID))
		Expect(q.GetQuestion()).To(Equal("Shall I go ahead with the plan?"))
		Expect(q.GetChoices()).To(Equal([]string{"yes", "no"}))
		Expect(q.GetDefaultAnswer()).To(Equal("no"))
		Expect(q.GetCanAnswer()).To(BeTrue())
		Expect(q.GetRecipients()).To(HaveLen(1))
		Expect(q.GetRecipients()[0].GetUserId()).To(Equal(creatorID))
		inbox, err := c.Questions.ListQuestions(ctx, &questionv1alpha1.ListQuestionsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		for _, other := range inbox.GetQuestions() {
			Expect(other.GetId()).NotTo(Equal(q.GetId()), "the admin is not a recipient, so the question is not in the admin's inbox")
		}

		By("rule: an answer is one of the choices, verbatim")
		_, err = creator.Questions.AnswerQuestion(ctx, &questionv1alpha1.AnswerQuestionRequest{
			OrgId: org, QuestionId: q.GetId(), Answer: &questionv1alpha1.AnswerQuestionRequest_Choice{Choice: "maybe"},
		})
		Expect(err).To(HaveOccurred(), "a choice the question does not offer")

		By("the creator answers; the run resumes with the answer and succeeds")
		answered, err := creator.Questions.AnswerQuestion(ctx, &questionv1alpha1.AnswerQuestionRequest{
			OrgId: org, QuestionId: q.GetId(), Answer: &questionv1alpha1.AnswerQuestionRequest_Choice{Choice: "yes"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(answered.GetQuestion().GetState()).To(Equal(questionv1alpha1.QuestionState_QUESTION_STATE_ANSWERED))
		Expect(answered.GetQuestion().GetAnswer()).To(Equal("yes"))
		Expect(answered.GetQuestion().GetAnswerKind()).To(Equal(questionv1alpha1.AnswerKind_ANSWER_KIND_CHOICE))
		Expect(answered.GetQuestion().GetAnsweredBy()).To(Equal(creatorID))
		done := awaitRun(ctx, c, org, runID, 6*time.Minute, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_SUCCEEDED
		})
		Expect(done.GetId()).To(Equal(runID), "the same run resumed")
		Eventually(func() []string { return agentReplies(ctx, c, org, made.GetTask().GetId()) }).
			WithContext(ctx).WithTimeout(time.Minute).WithPolling(2 * time.Second).
			Should(ContainElement(ContainSubstring("The answer was received")))

		By("rule: a question that was answered cannot be answered again")
		_, err = creator.Questions.AnswerQuestion(ctx, &questionv1alpha1.AnswerQuestionRequest{
			OrgId: org, QuestionId: q.GetId(), Answer: &questionv1alpha1.AnswerQuestionRequest_Choice{Choice: "no"},
		})
		Expect(err).To(HaveOccurred())
	})

	It("S4. cancels a run mid-turn, leaves its task open, and resumes it with a message; CancelTask ends the task for good", func(ctx SpecContext) {
		agent := newAgent(ctx, "stories slow", storiesSlowModel)
		fake := strings.TrimRight(os.Getenv("KOINON_E2E_FAKEMODEL_URL"), "/")
		Expect(fake).NotTo(BeEmpty(), "KOINON_E2E_FAKEMODEL_URL")
		modelRequests := func() int {
			reqs, err := e2eclient.FakeModelRequests(ctx, fake)
			Expect(err).NotTo(HaveOccurred())
			n := 0
			for _, r := range reqs {
				if r.Model == storiesSlowModel {
					n++
				}
			}
			return n
		}

		By("a task starts a run; the fake model holds its first answer for 40 seconds")
		made, err := c.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
			OrgId: org, AgentId: agent, Title: "Stories interrupt", Message: "stories slow: take your time",
		})
		Expect(err).NotTo(HaveOccurred())
		taskID, first := made.GetTask().GetId(), made.GetRun().GetId()
		Expect(first).NotTo(BeEmpty())
		awaitRun(ctx, c, org, first, 8*time.Minute, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_RUNNING
		})
		Eventually(modelRequests).WithContext(ctx).WithTimeout(2*time.Minute).WithPolling(time.Second).Should(BeNumerically(">=", 1), "the turn is in progress")

		By("rule: CancelRun ends the run, not the task")
		_, err = c.Runs.CancelRun(ctx, &runv1alpha1.CancelRunRequest{OrgId: org, RunId: first, Reason: "stories: planned interrupt"})
		Expect(err).NotTo(HaveOccurred())
		awaitRun(ctx, c, org, first, 2*time.Minute, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_CANCELED
		})
		got, err := c.Tasks.GetTask(ctx, &taskv1alpha1.GetTaskRequest{OrgId: org, TaskId: taskID})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.GetTask().GetState()).NotTo(Equal(taskv1alpha1.TaskState_TASK_STATE_CANCELED), "the task stays open")
		Expect(got.GetTask().GetState()).NotTo(Equal(taskv1alpha1.TaskState_TASK_STATE_FAILED))

		By("rule: a message on the open task queues a new run, which finishes")
		next, err := c.Tasks.AppendMessage(ctx, &taskv1alpha1.AppendMessageRequest{OrgId: org, TaskId: taskID, Text: "continue"})
		Expect(err).NotTo(HaveOccurred())
		second := next.GetRun().GetId()
		Expect(second).NotTo(BeEmpty(), "the message queued a run")
		Expect(second).NotTo(Equal(first))
		awaitRun(ctx, c, org, second, 8*time.Minute, func(r *runv1alpha1.Run) bool {
			return r.GetState() == runv1alpha1.RunState_RUN_STATE_SUCCEEDED
		})
		Eventually(func() []string { return agentReplies(ctx, c, org, taskID) }).
			WithContext(ctx).WithTimeout(time.Minute).WithPolling(2 * time.Second).
			Should(ContainElement(ContainSubstring("slow task is done")))

		By("rule: CancelTask ends the task, and a message on an ended task is refused")
		other, err := c.Tasks.CreateTask(ctx, &taskv1alpha1.CreateTaskRequest{
			OrgId: org, AgentId: agent, Title: "Stories ended", Message: "stories slow: to be canceled",
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = c.Tasks.CancelTask(ctx, &taskv1alpha1.CancelTaskRequest{OrgId: org, TaskId: other.GetTask().GetId()})
		Expect(err).NotTo(HaveOccurred())
		_, err = c.Tasks.AppendMessage(ctx, &taskv1alpha1.AppendMessageRequest{OrgId: org, TaskId: other.GetTask().GetId(), Text: "continue"})
		expectRefused(err, connect.CodeFailedPrecondition, "task has ended")
	})

	It("S5. refuses trace settings without platformCopyLevel, and a destination with a koinon. attribute", func(ctx SpecContext) {
		By("rule: UpdateSettings needs both levels; leaving platformCopyLevel out is refused")
		_, err := c.Trace.UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
		})
		expectRefused(err, connect.CodeInvalidArgument, "content_level and platform_copy_level are required")
		_, err = c.Trace.UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
		})
		expectRefused(err, connect.CodeInvalidArgument, "content_level and platform_copy_level are required")

		By("rule: the platform's copy cannot carry more than the org's own content level")
		_, err = c.Trace.UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
			PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_REDACTED,
		})
		expectRefused(err, connect.CodeInvalidArgument, "platform copy cannot carry more")

		By("rule: the sample rate is between 0 and 1")
		rate := 2.0
		_, err = c.Trace.UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
			PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE, SampleRate: &rate,
		})
		expectRefused(err, connect.CodeInvalidArgument, "sample rate must be between 0 and 1")

		By("with both levels the settings are stored, and a sample rate left out stays")
		rate = 0.5
		set, err := c.Trace.UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
			PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE, SampleRate: &rate,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetSettings().GetContentLevel()).To(Equal(tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE))
		Expect(set.GetSettings().GetPlatformCopyLevel()).To(Equal(tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE))
		Expect(set.GetSettings().GetSampleRate()).To(BeNumerically("==", 0.5))
		again, err := c.Trace.UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
			PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(again.GetSettings().GetSampleRate()).To(BeNumerically("==", 0.5))
		rate = 1
		_, err = c.Trace.UpdateSettings(ctx, &tracev1alpha1.UpdateSettingsRequest{
			OrgId: org, ContentLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE,
			PlatformCopyLevel: tracev1alpha1.ContentLevel_CONTENT_LEVEL_NONE, SampleRate: &rate,
		})
		Expect(err).NotTo(HaveOccurred())

		By("rule: the koinon. prefix of a resource attribute is Koinon's (checked before any test trace is sent)")
		destination := func(attrs map[string]string) *tracev1alpha1.CreateDestinationRequest {
			return &tracev1alpha1.CreateDestinationRequest{OrgId: org, Destination: &tracev1alpha1.DestinationInput{
				Name: "stories-collector", Backend: tracev1alpha1.Backend_BACKEND_GENERIC,
				EndpointUrl:        "https://collector.example.invalid/v1/traces",
				Secrets:            map[string]string{"X-Stories-Key": "koinon_test_not-a-real-token"},
				ResourceAttributes: attrs,
			}}
		}
		_, err = c.Trace.CreateDestination(ctx, destination(map[string]string{"koinon.lab.run": "stories"}))
		expectRefused(err, connect.CodeInvalidArgument, `is set by Koinon`)
		_, err = c.Trace.CreateDestination(ctx, destination(map[string]string{"gen_ai.system": "x"}))
		expectRefused(err, connect.CodeInvalidArgument, `is set by Koinon`)
		_, err = c.Trace.CreateDestination(ctx, destination(map[string]string{"Koinon.Lab": "x"}))
		expectRefused(err, connect.CodeInvalidArgument, "lower-case letters")
		// A refused destination leaves nothing behind.
		list, err := c.Trace.ListDestinations(ctx, &tracev1alpha1.ListDestinationsRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		for _, d := range list.GetDestinations() {
			Expect(d.GetName()).NotTo(Equal("stories-collector"))
		}
	})

	It("S6. keeps the org's run-content retention between its bounds, and back to the default at 0", func(ctx SpecContext) {
		cur, err := c.OrgSettings.GetRunContentRetention(ctx, &orgsettingsv1alpha1.GetRunContentRetentionRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		r := cur.GetRetention()
		Expect(r.GetDefaultDays()).To(BeNumerically(">", 0))
		Expect(r.GetMinDays()).To(BeNumerically(">", 0))
		Expect(r.GetMaxDays()).To(BeNumerically(">=", r.GetMinDays()))
		DeferCleanup(func(ctx context.Context) {
			_, _ = c.OrgSettings.SetRunContentRetention(ctx, &orgsettingsv1alpha1.SetRunContentRetentionRequest{OrgId: org})
		})

		By("rule: a value above the maximum is refused")
		_, err = c.OrgSettings.SetRunContentRetention(ctx, &orgsettingsv1alpha1.SetRunContentRetentionRequest{OrgId: org, Days: r.GetMaxDays() + 1})
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument), "got %v", err)

		By("a value inside the bounds is stored")
		want := r.GetMinDays()
		set, err := c.OrgSettings.SetRunContentRetention(ctx, &orgsettingsv1alpha1.SetRunContentRetentionRequest{OrgId: org, Days: want})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetRetention().GetDays()).To(Equal(want))

		By("rule: 0 returns it to the default")
		set, err = c.OrgSettings.SetRunContentRetention(ctx, &orgsettingsv1alpha1.SetRunContentRetentionRequest{OrgId: org})
		Expect(err).NotTo(HaveOccurred())
		Expect(set.GetRetention().GetDays()).To(Equal(set.GetRetention().GetDefaultDays()))
	})
})
